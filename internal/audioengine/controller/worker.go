package controller

import (
	"context"
	"errors"
	"time"

	"mygo-music/internal/audioengine/analysis"
	"mygo-music/internal/audioengine/decode"
	"mygo-music/internal/audioengine/output"
	"mygo-music/internal/audioengine/pcm"
	"mygo-music/internal/audioengine/render"
)

// transport is private: mutable resources never escape through EngineHandle.
// One transport worker serializes open/control/close; native Output continues
// to own its locked device thread and does not wait for this control worker.
type transport interface {
	setPlaying(context.Context, bool) error
	observe() (observation, error)
	close() error
}
type opener func(context.Context, Config, Media, pcm.Generation, time.Duration) (transport, error)
type job struct {
	volume          float64
	ctx             context.Context
	revision        uint64
	generation      pcm.Generation
	active, playing bool
	media           Media
	position        time.Duration
	config          Config
	configAccepted  time.Time
}
type observation struct {
	state                                   State
	playing                                 bool
	source                                  decode.Info
	output                                  output.Opened
	position, submitted, content, underruns uint64
	exact                                   bool
	start                                   uint64
	media                                   Media
	transition, config                      uint64
	volume, preamp                          float64
	latency, tail                           int
	outputStats                             output.Snapshot
	progress                                render.Progress
	measurement                             analysis.Measurement
	analysisDropped                         uint64
	parameterDelay                          time.Duration
}
type workerReport struct {
	volume      float64
	revision    uint64
	generation  pcm.Generation
	item        Item
	observation observation
	err         error
	final       bool
}

func (h *EngineHandle) runWorker(config Config, open opener) {
	defer close(h.workerDone)
	interval := 100 * time.Millisecond
	if config.Processing || config.Next != nil {
		interval = 10 * time.Millisecond
	}
	tick := time.NewTicker(interval)
	defer tick.Stop()
	var session transport
	var current job
	closeSession := func() error {
		if session == nil {
			return nil
		}
		err := session.close()
		session = nil
		return err
	}
	defer func() { h.closeErr = closeSession() }()
	report := func(r workerReport) {
		select {
		case h.reports <- r:
		case <-h.ctx.Done():
		}
	}
	for {
		select {
		case <-h.ctx.Done():
			return
		case j := <-h.jobs:
			if j.ctx.Err() != nil {
				continue
			}
			r := workerReport{revision: j.revision, generation: j.generation, item: j.media.Item, volume: j.volume, final: true, observation: observation{state: Idle}}
			if !j.active || session == nil || current.generation.Session != j.generation.Session || current.generation.Seek != j.generation.Seek {
				if err := closeSession(); err != nil {
					r.err = err
					r.item = Item{}
					report(r)
					continue
				}
				if j.ctx.Err() != nil {
					continue
				}
				if j.active {
					opened, err := open(j.ctx, j.config, j.media, j.generation, j.position)
					if j.ctx.Err() != nil {
						if opened != nil {
							_ = opened.close()
						}
						continue
					}
					if err != nil {
						if opened != nil {
							_ = opened.close()
						}
						r.err = err
						r.item = Item{}
						report(r)
						continue
					}
					session = opened
				}
			}
			current = j
			if session != nil {
				if config.Shared {
					r.err = session.(interface {
						setVolume(context.Context, float64) error
					}).setVolume(j.ctx, j.volume)
				}
				if config.Processing {
					session.(interface {
						setProcessedLevels(float64, float64, uint64, time.Time)
					}).setProcessedLevels(j.volume, j.config.DSP.PreampDB, j.generation.Config, j.configAccepted)
				}
				if r.err == nil {
					r.err = session.setPlaying(j.ctx, j.playing)
				}
				if j.ctx.Err() != nil {
					continue
				}
				if r.err == nil {
					r.observation, r.err = session.observe()
				}
				if r.err != nil {
					r.err = errors.Join(r.err, closeSession())
					r.observation.playing = false
				}
			}
			report(r)
		case <-tick.C:
			if session == nil || current.ctx.Err() != nil {
				continue
			}
			o, err := session.observe()
			if err != nil {
				err = errors.Join(err, closeSession())
				o.playing = false
			}
			report(workerReport{revision: current.revision, generation: current.generation, item: current.media.Item, observation: o, err: err})
		}
	}
}
