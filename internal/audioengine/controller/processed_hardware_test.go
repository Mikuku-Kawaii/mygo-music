package controller

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"runtime/metrics"
	"slices"
	"testing"
	"time"

	"mygo-music/internal/audioengine/dsp"
	"mygo-music/internal/audioengine/fixture"
	"mygo-music/internal/audioengine/pcm"
	"mygo-music/internal/audioengine/render"
)

// Silent native output exercises fixed-rate DSP, continuation, volume
// updates and runtime allocation pressure together on the actual endpoint.
func TestM2ProcessedHardware(t *testing.T) {
	if os.Getenv("PRISM_NATIVE_SMOKE") != "1" {
		t.Skip("opt-in silent WASAPI integration")
	}
	duration := 5 * time.Second
	if raw := os.Getenv("PRISM_M2_STRESS"); raw != "" {
		var err error
		duration, err = time.ParseDuration(raw)
		if err != nil || duration < 5*time.Second {
			t.Fatalf("invalid duration: %q", raw)
		}
	}
	dir := t.TempDir()
	paths := [2]string{filepath.Join(dir, "96.wav"), filepath.Join(dir, "44.wav")}
	for i, rate := range []int{96000, 44100} {
		f := pcm.Format{Kind: pcm.Integer, Rate: rate, Channels: 2, ChannelMask: 3, ValidBits: 24, ContainerBits: 24}
		if err := fixture.WriteWAV(paths[i], f, make([]int32, rate*4)); err != nil {
			t.Fatal(err)
		}
	}
	ctx, stop := context.WithTimeout(context.Background(), duration+20*time.Second)
	defer stop()
	slot := new(render.NextSlot)
	if !slot.Offer(render.NextMedia{Token: 1, Source: "local", ID: "B", Path: paths[1]}) {
		t.Fatal("first offer")
	}
	config := dsp.Config{Rate: 48000, Limit: true, PreampDB: -3}
	config.Filters[0] = dsp.Filter{Enabled: true, Kind: dsp.Peak, Hz: 1000, DB: 4, Q: .7}
	config.Filters[1] = dsp.Filter{Enabled: true, Kind: dsp.LowShelf, Hz: 120, DB: -4, Q: .7}
	config.Filters[2] = dsp.Filter{Enabled: true, Kind: dsp.HighShelf, Hz: 9500, DB: 2, Q: .7}
	h, err := New(Config{Processing: true, DSP: config, Next: slot, InitialVolume: .3})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	r, err := h.SetQueue(ctx, []Media{{Item: Item{Source: "local", ID: "A"}, Path: paths[0]}}, 0)
	first := await(t, r, err).Snapshot
	r, err = h.Play(ctx)
	await(t, r, err)
	start := time.Now()
	var base runtime.MemStats
	runtime.ReadMemStats(&base)
	timer := time.NewTimer(duration)
	defer timer.Stop()
	pressure := time.NewTicker(250 * time.Millisecond)
	defer pressure.Stop()
	volume := time.NewTicker(500 * time.Millisecond)
	defer volume.Stop()
	progress := time.NewTicker(5 * time.Minute)
	defer progress.Stop()
	var retained [][]byte
	var transition uint64
	var accepted time.Time
	gain := .3
	preamp := config.PreampDB
	targetVolume, targetPreamp := gain, preamp
	water := first.RingLowWater
	var maxUnderruns uint64
	var latencies []time.Duration
	for {
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-progress.C:
			s := h.Snapshot()
			t.Logf("elapsed=%v continuations=%d applied=%d low-water=%d underruns=%d", time.Since(start).Truncate(time.Second),
				transition, len(latencies), water, s.Underruns)
		case <-timer.C:
			s := h.Snapshot()
			var end runtime.MemStats
			runtime.ReadMemStats(&end)
			slices.Sort(latencies)
			if len(latencies) == 0 {
				t.Fatal("no applied gain measurements")
			}
			p95 := latencies[(95*len(latencies)+99)/100-1]
			hist := []metrics.Sample{{Name: "/sched/latencies:seconds"}, {Name: "/sched/pauses/total/gc:seconds"}}
			metrics.Read(hist)
			for _, m := range hist {
				if m.Value.Kind() != metrics.KindFloat64Histogram {
					continue
				}
				h := m.Value.Float64Histogram()
				var count uint64
				var highest float64
				for i, n := range h.Counts {
					count += n
					if n > 0 {
						highest = h.Buckets[i+1]
					}
				}
				t.Logf("%s process count=%d max bucket upper=%g seconds", m.Name, count, highest)
			}
			b, _ := json.Marshal(s)
			t.Logf("elapsed=%v changes=%d responses=%d gain P95=%v max=%v GC=%d GC pause=%v low-water=%d underruns=%d", time.Since(start),
				transition, len(latencies), p95, latencies[len(latencies)-1], end.NumGC-base.NumGC,
				time.Duration(end.PauseTotalNs-base.PauseTotalNs), water, maxUnderruns)
			t.Log(string(b))
			if transition < uint64(duration/(2*time.Second))-1 || maxUnderruns != 0 || p95 > 100*time.Millisecond {
				t.Fatal("continuity, underrun, or response target missed")
			}
			return
		case <-pressure.C:
			b := make([]byte, 4<<20)
			b[len(b)-1] = 1
			retained = append(retained, b)
			if len(retained) == 5 {
				retained = nil
			}
		case <-volume.C:
			if !accepted.IsZero() {
				continue
			}
			var err error
			if len(latencies)%2 == 0 {
				targetVolume = .7 - gain
				_, err = h.SetVolume(ctx, targetVolume)
			} else {
				targetPreamp = -10 - preamp
				_, err = h.SetPreamp(ctx, targetPreamp)
			}
			if err != nil {
				t.Fatal(err)
			}
			accepted = time.Now()
		case <-h.Changes():
			s := h.Snapshot()
			if s.Underruns > maxUnderruns {
				t.Logf("elapsed=%v underruns=%d transition=%d source=%d source-frame=%d service-max=%v",
					time.Since(start).Truncate(time.Millisecond), s.Underruns, s.ContinuationToken, s.Source.Format.Rate, s.PositionFrames, s.MaxService)
			}
			maxUnderruns = max(maxUnderruns, s.Underruns)
			water = min(water, s.RingLowWater)
			if s.State == Failed || s.State == Ended || s.AppliedGeneration.Format != first.AppliedGeneration.Format || s.Output.Format.Rate != 48000 {
				t.Fatalf("lost the initial device epoch: %+v", s)
			}
			if !accepted.IsZero() && s.AppliedVolume == targetVolume && s.AppliedPreampDB == targetPreamp && s.AppliedGeneration.Config == s.RequestedGeneration.Config {
				latencies = append(latencies, time.Since(accepted))
				gain, preamp, accepted = targetVolume, targetPreamp, time.Time{}
			}
			if s.ContinuationToken > transition {
				transition = s.ContinuationToken
				slot.Acknowledge(transition)
				i := int((transition + 1) % 2)
				if !slot.Offer(render.NextMedia{Token: transition + 1, Source: "local", ID: []string{"A", "B"}[i], Path: paths[i]}) {
					t.Fatal("continuation slot not acknowledged")
				}
			}
		}
	}
}
