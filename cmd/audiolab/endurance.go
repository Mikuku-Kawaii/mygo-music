package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"mygo-music/internal/audioengine/analysis"
	"mygo-music/internal/audioengine/controller"
	"mygo-music/internal/audioengine/diagnostics"
	"mygo-music/internal/audioengine/dsp"
	"mygo-music/internal/audioengine/fixture"
	"mygo-music/internal/audioengine/pcm"
	"mygo-music/internal/audioengine/render"
)

type enduranceOptions struct {
	mode, device, evidence     string
	duration, interval, period time.Duration
	rate, bits, container      int
	stress, stableBuffer       bool
}

type enduranceSummary struct {
	Requested, Elapsed                time.Duration
	Continuations, Underruns, Missing uint64
	ParameterUpdates                  int
	ParameterP95Upper, ParameterMax   time.Duration
	AllocationPressure                bool
	First, Last                       diagnostics.Report
}

type enduranceRecord struct {
	Event         string
	ElapsedNS     int64
	Continuations uint64
	Report        diagnostics.Report
}

// endurance drives the production EngineHandle and the same presentation
// handoff as the app. The media are two short, silent WAVs, reused for the
// entire run. It never reads the app's settings, library or sign-in state.
func endurance(parent context.Context, opt enduranceOptions) (summary enduranceSummary, resultErr error) {
	summary.Requested, summary.AllocationPressure = opt.duration, opt.stress
	if opt.duration < time.Second || opt.duration > 24*time.Hour || opt.interval < time.Second {
		return summary, errors.New("endurance requires a duration between 1s and 24h and an interval of at least 1s")
	}
	config := controller.Config{EndpointID: opt.device, Period: opt.period, ContainerBits: opt.container, StableBuffer: opt.stableBuffer}
	switch opt.mode {
	case "shared":
		config.Shared = true
	case "exclusive":
	case "processed":
		config.Processing, config.InitialVolume = true, .3
		config.DSP = dsp.Config{Rate: opt.rate, PreampDB: -3, Limit: true}
		config.DSP.Filters[0] = dsp.Filter{Enabled: true, Kind: dsp.Peak, Hz: 1000, DB: 3, Q: .7}
		config.DSP.Filters[1] = dsp.Filter{Enabled: true, Kind: dsp.LowShelf, Hz: 140, DB: -4, Q: .7}
		config.DSP.Filters[2] = dsp.Filter{Enabled: true, Kind: dsp.HighShelf, Hz: 10000, DB: 2, Q: .7}
	default:
		return summary, errors.New("unknown endurance output")
	}
	mode := "wasapi-" + opt.mode
	config.Analysis = new(analysis.Subscription)
	config.Analysis.SetEnabled(true)
	config.Next = new(render.NextSlot)
	dir, err := os.MkdirTemp("", "mygo-endurance-")
	if err != nil {
		return summary, err
	}
	defer os.Remove(dir)
	rates := [2]int{opt.rate, opt.rate}
	if config.Processing {
		rates = [2]int{44100, 96000}
	}
	var media [2]controller.Media
	for i, rate := range rates {
		f := pcm.Format{Kind: pcm.Integer, Rate: rate, Channels: 2, ChannelMask: 3, ValidBits: opt.bits, ContainerBits: opt.bits}
		if err := f.Validate(); err != nil {
			return summary, err
		}
		name := fmt.Sprintf("silent-%d", i)
		path := filepath.Join(dir, name+".wav")
		defer os.Remove(path)
		if err := fixture.WriteWAV(path, f, make([]int32, 4*rate)); err != nil {
			return summary, err
		}
		media[i] = controller.Media{Item: controller.Item{Source: "fixture", ID: name}, Path: path}
	}
	offer := func(i int, token uint64) bool {
		m := media[i]
		return config.Next.Offer(render.NextMedia{Token: token, Source: m.Source, ID: m.ID, Path: m.Path})
	}
	if !offer(1, 1) {
		return summary, errors.New("could not reserve initial continuation")
	}
	var record func(enduranceRecord) error
	if opt.evidence != "" {
		f, err := os.OpenFile(opt.evidence, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return summary, err
		}
		defer func() { resultErr = errors.Join(resultErr, f.Close()) }()
		encoder := json.NewEncoder(f)
		record = func(r enduranceRecord) error { return encoder.Encode(r) }
	}
	h, err := controller.New(config)
	if err != nil {
		return summary, err
	}
	defer func() { resultErr = errors.Join(resultErr, h.Close()) }()
	snapshot := func(s controller.Snapshot) diagnostics.Report {
		return diagnostics.Capture(diagnostics.Playback{
			Mode: mode, Endpoint: opt.device, Ready: s.Output.Format.Rate != 0, Loading: s.State == controller.Preparing,
			Playing: s.DesiredPlaying, Failed: s.State == controller.Failed, Native: s,
		})
	}
	ctx, cancel := context.WithTimeout(parent, opt.duration+15*time.Second)
	defer cancel()
	sample := func(event string, s controller.Snapshot, elapsed time.Duration) (diagnostics.Report, error) {
		r := snapshot(s)
		if record == nil {
			return r, nil
		}
		return r, record(enduranceRecord{Event: event, ElapsedNS: int64(elapsed), Continuations: s.ContinuationToken, Report: r})
	}
	if receipt, err := h.SetQueue(ctx, media[:1], 0); err != nil {
		return summary, err
	} else if _, err := receipt.Wait(ctx); err != nil {
		summary.Last, _ = sample("open-failed", h.Snapshot(), 0)
		return summary, err
	}
	if receipt, err := h.Play(ctx); err != nil {
		return summary, err
	} else if _, err := receipt.Wait(ctx); err != nil {
		summary.Last, _ = sample("start-failed", h.Snapshot(), 0)
		return summary, err
	}
	start := time.Now()
	first := h.Snapshot()
	summary.First, err = sample("start", first, 0)
	if err != nil {
		return summary, err
	}
	var delays [1001]uint64 // 1 ms buckets, last bucket includes all longer delays
	defer func() {
		last := h.Snapshot()
		summary.Elapsed = time.Since(start)
		summary.Continuations, summary.Underruns, summary.Missing = last.ContinuationToken, last.Underruns, last.OutputStats.UnderrunFrames
		var count uint64
		target := uint64((summary.ParameterUpdates*95 + 99) / 100)
		for i, n := range delays {
			count += n
			if target > 0 && count >= target {
				summary.ParameterP95Upper = time.Duration(i) * time.Millisecond
				if i == len(delays)-1 {
					summary.ParameterP95Upper = summary.ParameterMax
				}
				break
			}
		}
		if (config.Processing || config.Shared) && resultErr == nil {
			switch {
			case summary.ParameterUpdates == 0:
				resultErr = errors.New("endurance recorded no applied gain updates")
			case summary.ParameterP95Upper > 100*time.Millisecond:
				resultErr = fmt.Errorf("gain update p95 upper bound exceeded 100 ms (%s)", summary.ParameterP95Upper)
			}
		}
		lastEvent := "end"
		if resultErr != nil {
			lastEvent = "failed"
		}
		var err error
		summary.Last, err = sample(lastEvent, last, summary.Elapsed)
		resultErr = errors.Join(resultErr, err)
	}()
	timer := time.NewTimer(opt.duration)
	defer timer.Stop()
	heartbeat := time.NewTicker(opt.interval)
	defer heartbeat.Stop()
	var pressure <-chan time.Time
	if opt.stress {
		t := time.NewTicker(250 * time.Millisecond)
		defer t.Stop()
		pressure = t.C
	}
	var adjustments <-chan time.Time
	if config.Processing || config.Shared {
		t := time.NewTicker(500 * time.Millisecond)
		defer t.Stop()
		adjustments = t.C
	}
	var memory [5][]byte
	allocated := 0
	var nextToken, handledConfig, missed uint64
	var loggedMiss time.Time
	for {
		select {
		case <-ctx.Done():
			return summary, ctx.Err()
		case <-timer.C:
			s := h.Snapshot()
			if s.Underruns > 0 {
				return summary, fmt.Errorf("sustained playback recorded %d underruns", s.Underruns)
			}
			if s.ContinuationToken+1 < uint64(opt.duration/(2*time.Second)) {
				return summary, errors.New("presentation did not sustain the expected continuation count")
			}
			return summary, nil
		case <-heartbeat.C:
			if _, err := sample("sample", h.Snapshot(), time.Since(start)); err != nil {
				return summary, err
			}
		case <-pressure:
			memory[allocated%len(memory)] = make([]byte, 4<<20)
			memory[allocated%len(memory)][(4<<20)-1] = 1
			allocated++
		case <-adjustments:
			s := h.Snapshot()
			if s.RequestedGeneration.Config != s.AppliedGeneration.Config {
				continue
			}
			if config.Processing && s.AppliedGeneration.Config%2 == 0 {
				_, err = h.SetPreamp(ctx, -10-s.AppliedPreampDB)
			} else {
				_, err = h.SetVolume(ctx, .7-s.AppliedVolume)
			}
			if err != nil {
				return summary, err
			}
		case <-h.Changes():
			s := h.Snapshot()
			if s.State == controller.Failed || s.State == controller.Ended || s.OutputStats.Epoch != first.OutputStats.Epoch {
				return summary, fmt.Errorf("endurance lost playback (%s, %s)", s.State, s.ErrorKind)
			}
			if s.ContinuationToken > nextToken {
				nextToken = s.ContinuationToken
				config.Next.Acknowledge(nextToken)
				if !offer(int((nextToken+1)%2), nextToken+1) {
					return summary, errors.New("could not prepare next endurance track")
				}
			}
			if config.Processing || config.Shared {
				if rev := s.AppliedGeneration.Config; rev > handledConfig && s.ParameterDelay > 0 {
					ms := (s.ParameterDelay + time.Millisecond - 1) / time.Millisecond
					delays[min(int(ms), len(delays)-1)]++
					summary.ParameterUpdates++
					summary.ParameterMax = max(summary.ParameterMax, s.ParameterDelay)
					handledConfig = rev
				}
			}
			if s.Underruns > missed && time.Since(loggedMiss) >= time.Second {
				if _, err := sample("underrun", s, time.Since(start)); err != nil {
					return summary, err
				}
				missed, loggedMiss = s.Underruns, time.Now()
			}
		}
	}
}
