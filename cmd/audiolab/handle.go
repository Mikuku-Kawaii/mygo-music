package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"mygo-music/internal/audioengine/controller"
	"mygo-music/internal/audioengine/fixture"
	"mygo-music/internal/audioengine/pcm"
)

type handleStep struct {
	Name     string
	Snapshot controller.Snapshot
}
type handleReport struct {
	Steps    []handleStep
	Closed   controller.Snapshot
	Evidence string
}

// handleSmoke drives the public asynchronous facade without application state
// or UI. Both generated local files contain only one second of digital silence.
func handleSmoke(ctx context.Context, config controller.Config, f pcm.Format) (report handleReport, err error) {
	report.Evidence = "PP-M1-09 silent WAV/FLAC facade lifecycle; device presentation only, no external digital capture or automatic next-track claim."
	if config.ASIO != "" {
		report.Evidence = "PP-M5-02 silent WAV/FLAC ASIO callbacks and driver sample clock; no external digital capture."
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	dir, err := os.MkdirTemp("", "prism-handle-")
	if err != nil {
		return report, err
	}
	defer os.Remove(dir)
	wav, flac := filepath.Join(dir, "silent.wav"), filepath.Join(dir, "silent.flac")
	defer os.Remove(wav)
	defer os.Remove(flac)
	samples := make([]int32, f.Rate*f.Channels)
	if err = fixture.WriteWAV(wav, f, samples); err != nil {
		return report, err
	}
	if err = fixture.WriteFLAC(flac, f, samples); err != nil {
		return report, err
	}
	h, err := controller.New(config)
	if err != nil {
		return report, err
	}
	defer func() { err = errors.Join(err, h.Close()); report.Closed = h.Snapshot() }()
	step := func(name string, submit func(context.Context) (*controller.Receipt, error)) (controller.Snapshot, error) {
		r, e := submit(ctx)
		if e != nil {
			return h.Snapshot(), fmt.Errorf("%s: %w", name, e)
		}
		result, e := r.Wait(ctx)
		report.Steps = append(report.Steps, handleStep{name, result.Snapshot})
		if e != nil {
			return result.Snapshot, fmt.Errorf("%s: %w", name, e)
		}
		if result.Status != controller.Applied || result.Snapshot.AppliedCommand != r.ID() {
			return result.Snapshot, fmt.Errorf("%s: command was not confirmed", name)
		}
		return result.Snapshot, nil
	}
	queue := []controller.Media{
		{Item: controller.Item{Source: "fixture", ID: "wav", Title: "Silent WAV"}, Path: wav},
		{Item: controller.Item{Source: "fixture", ID: "flac", Title: "Silent FLAC"}, Path: flac},
	}
	prepared, err := step("prepare-wav", func(ctx context.Context) (*controller.Receipt, error) { return h.SetQueue(ctx, queue, 0) })
	if err != nil {
		return report, err
	}
	if prepared.State != controller.Paused || prepared.EffectivePlaying {
		return report, errors.New("queue did not prepare paused")
	}
	if _, err = step("play-wav", h.Play); err != nil {
		return report, err
	}
	if err = delay(ctx, 150*time.Millisecond); err != nil {
		return report, err
	}
	paused, err := step("pause-wav", h.Pause)
	if err != nil {
		return report, err
	}
	if err = delay(ctx, 150*time.Millisecond); err != nil {
		return report, err
	}
	stable := h.Snapshot()
	report.Steps = append(report.Steps, handleStep{"paused-stable", stable})
	if stable.PositionFrames != paused.PositionFrames || stable.State != controller.Paused || stable.EffectivePlaying {
		return report, errors.New("presentation advanced during pause")
	}
	if config.ASIO != "" {
		seek, err := step("seek-wav-while-paused", func(ctx context.Context) (*controller.Receipt, error) {
			return h.Seek(ctx, 250*time.Millisecond)
		})
		if err != nil {
			return report, err
		}
		if seek.State != controller.Paused || seek.EffectivePlaying || seek.SourceStartFrame != uint64(f.Rate/4) {
			return report, errors.New("paused ASIO seek did not preserve the requested anchor")
		}
	}
	if _, err = step("resume-wav", h.Play); err != nil {
		return report, err
	}
	if _, err = step("pause-before-select", h.Pause); err != nil {
		return report, err
	}
	selected, err := step("select-flac", func(ctx context.Context) (*controller.Receipt, error) { return h.Select(ctx, 1) })
	if err != nil {
		return report, err
	}
	if selected.State != controller.Paused || selected.Current.ID != "flac" || selected.PositionFrames != 0 || selected.AppliedGeneration.Session <= prepared.AppliedGeneration.Session {
		return report, errors.New("selection did not preserve pause or reset presentation generation")
	}
	if _, err = step("play-flac", h.Play); err != nil {
		return report, err
	}
	for {
		s := h.Snapshot()
		if s.State == controller.Failed {
			return report, errors.New(s.Error)
		}
		if s.State == controller.Ended {
			report.Steps = append(report.Steps, handleStep{"presented-end", s})
			clockConfirmed := s.PositionExact || config.ASIO != "" && s.Output.ASIO && s.OutputStats.ClockInaccurate == 0
			if s.Current.ID != "flac" || s.Index != 1 || s.PositionFrames != uint64(f.Rate) || s.ContentSubmittedFrames != uint64(f.Rate) || !clockConfirmed || s.Underruns != 0 {
				return report, errors.New("unexpected presented frames or end state")
			}
			break
		}
		select {
		case <-ctx.Done():
			return report, ctx.Err()
		case <-h.Changes():
		}
	}
	stopped, err := step("stop", h.Stop)
	if err != nil {
		return report, err
	}
	if stopped.State != controller.Idle || stopped.Current.ID != "" || stopped.Selected.ID != "flac" {
		return report, errors.New("stop did not release session and retain selection")
	}
	return report, nil
}
