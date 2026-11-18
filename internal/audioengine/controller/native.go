package controller

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"time"

	"mygo-music/internal/audioengine/decode"
	"mygo-music/internal/audioengine/output"
	"mygo-music/internal/audioengine/pcm"
	"mygo-music/internal/audioengine/render"
)

type nativeTransport struct {
	followDefault          bool
	lastObserve, lastProbe time.Time
	probe                  func(context.Context, string, bool) error
	pipeline               *render.Pipeline
	device                 *output.Stream
	source                 decode.Info
	cancel                 context.CancelFunc
	start                  uint64
	base, lastPresented    uint64
	transition             uint64
	media                  Media
	processing             bool
	latency, tail          int
}

func openNative(ctx context.Context, config Config, media Media, g pcm.Generation, position time.Duration) (transport, error) {
	open := decode.Open
	if config.DoP {
		open = decode.OpenDoP
	} else if config.Processing {
		open = decode.OpenProcessed
	}
	d, err := open(ctx, media.Path)
	if err != nil {
		return nil, err
	}
	info := d.Info()
	start, err := seekDecoder(ctx, d, position)
	if err != nil {
		d.Close()
		return nil, err
	}
	f := info.Format
	// Choose a packed integer container from effective source precision, not
	// the decoder's int32 workspace (notably FLAC16/24). No samples change.
	switch {
	case f.ValidBits <= 16:
		f.ContainerBits = 16
	case f.ValidBits <= 24:
		f.ContainerBits = 24
	default:
		f.ContainerBits = 32
	}
	if config.ContainerBits != 0 {
		f.ContainerBits = config.ContainerBits
	}
	var seed [8]byte
	if config.Processing {
		f.Kind, f.ValidBits, f.ContainerBits = pcm.Integer, 32, 32
		if config.DSP.Rate != 0 {
			f.Rate = config.DSP.Rate
		}
		if config.DSP.Dither && !config.DSP.Bypass {
			_, _ = rand.Read(seed[:])
		}
	}
	// Session lifetime is NOT the command context: a later Pause must cancel
	// obsolete preparation without canceling an already accepted audio stream.
	life, cancel := context.WithCancel(context.Background())
	if config.ASIO != "" {
		config.Next = nil
	}
	p, err := render.StartLive(life, d, f, g, int64(start), render.LiveOptions{
		Next: config.Next, Shared: config.Shared, Processing: config.Processing, DoP: config.DoP, StableBuffer: config.StableBuffer,
		DSP: config.DSP, Gains: media.Gains,
		Seed: binary.LittleEndian.Uint64(seed[:]), Volume: config.InitialVolume,
	})
	if err != nil {
		cancel()
		d.Close()
		return nil, err
	}
	n := &nativeTransport{pipeline: p, source: info, cancel: cancel, start: start, media: media, processing: config.Processing,
		latency: p.LatencyFrames(), tail: p.TailFrames(), followDefault: config.EndpointID == "", probe: output.CheckEndpoint}
	if err = ctx.Err(); err != nil {
		n.close()
		return nil, err
	}
	s, err := output.Open(life, output.Config{Shared: config.Shared, DoP: config.DoP, ASIO: config.ASIO, EndpointID: config.EndpointID,
		Format: f, Period: config.Period, StableBuffer: config.StableBuffer, Epoch: g.Format, Analysis: config.Analysis}, p.Ring)
	if err != nil {
		n.close()
		return nil, err
	}
	n.device = s
	if s.Info.ASIO {
		n.probe = nil // ASIO callbacks report hardware/format invalidation
	}
	prefill := s.Info.BufferFrames
	if config.StableBuffer || !config.Shared && !config.Processing {
		prefill = p.Ring.Capacity()
	}
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		if err = ctx.Err(); err != nil {
			n.close()
			return nil, err
		}
		select {
		case <-p.Done():
			if err = p.Wait(); err != nil {
				n.close()
				return nil, err
			}
		default:
		}
		if p.Ring.Buffered() >= prefill || p.Ring.Finished() {
			return n, nil
		}
		select {
		case <-ctx.Done():
			n.close()
			return nil, ctx.Err()
		case <-s.Done():
			err = s.Err()
			n.close()
			return nil, err
		case <-tick.C:
		}
	}
}
func (n *nativeTransport) setPlaying(ctx context.Context, playing bool) error {
	if !playing && n.device.Snapshot().State == output.Ended {
		return nil
	}
	if playing {
		return n.device.Start(ctx)
	}
	return n.device.Pause(ctx)
}
func (n *nativeTransport) setVolume(ctx context.Context, gain float64) error {
	return n.device.SetVolume(ctx, gain)
}
func (n *nativeTransport) setProcessedLevels(gain, preamp float64, revision uint64, accepted time.Time) {
	n.pipeline.SetLevels(gain, preamp, revision, accepted)
}
func (n *nativeTransport) checkHealth(now time.Time) error {
	if !n.lastObserve.IsZero() && now.Sub(n.lastObserve) > 5*time.Second {
		return output.ErrResumeRequired
	}
	n.lastObserve = now
	if n.probe != nil && now.Sub(n.lastProbe) > time.Second {
		n.lastProbe = now
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		err := n.probe(ctx, n.device.Info.EndpointID, n.followDefault)
		cancel()
		if err != nil {
			return err
		}
	}
	return nil
}
func (n *nativeTransport) observe() (observation, error) {
	if err := n.checkHealth(time.Now()); err != nil {
		return observation{}, err
	}
	s := n.device.Snapshot()
	// Missing frames extend the device clock. Subtract all known holes
	// conservatively and never move back on a new underrun report.
	presented := min(s.Presented-min(s.Presented, s.UnderrunFrames), s.ContentSubmitted)
	if s.State == output.Ended {
		presented = s.ContentSubmitted
	}
	n.lastPresented = max(n.lastPresented, presented)
	if b := n.pipeline.Boundary(); b != nil && b.Media.Token != n.transition && presented > b.OutputFrame {
		n.transition, n.source, n.base, n.start = b.Media.Token, b.Source, b.OutputFrame, 0
		n.latency, n.tail = b.Latency, b.Tail
		n.media = Media{Item: Item{Source: b.Media.Source, ID: b.Media.ID, Title: b.Media.Title}, Path: b.Media.Path, Gains: b.Media.Gains}
	}
	delta := n.lastPresented - min(n.lastPresented, n.base)
	rate := uint64(n.device.Info.Format.Rate)
	in := uint64(n.source.Format.Rate)
	position := n.start + delta/rate*in + delta%rate*in/rate
	o := observation{source: n.source, output: n.device.Info, start: n.start, position: position,
		submitted: s.Submitted, content: s.ContentSubmitted, underruns: s.Underruns,
		exact: !n.device.Info.ASIO && (n.start == 0 || n.source.ExactSeek) && s.Underruns == 0 && s.ClockInaccurate == 0 && rate == in,
		media: n.media, transition: n.transition, latency: n.latency, tail: n.tail,
		outputStats: s, progress: n.pipeline.Progress()}
	if n.source.DecodeLookahead > 0 {
		lookahead := (n.source.DecodeLookahead*int(rate) + int(in) - 1) / int(in)
		o.latency += lookahead
	}
	if n.processing {
		o.volume, o.preamp, o.config, o.parameterDelay = n.pipeline.AppliedLevels()
	}
	if n.source.Frames >= 0 {
		o.position = min(o.position, uint64(n.source.Frames))
	}
	switch s.State {
	case output.Ready, output.Paused:
		o.state = Paused
	case output.Playing:
		o.state = Playing
		o.playing = true
	case output.Draining:
		o.state = Draining
		o.playing = true
	case output.Ended:
		o.state = Ended
		if n.source.Frames >= 0 && n.lastPresented > n.base {
			o.position = uint64(n.source.Frames)
		}
	case output.Failed:
		o.state = Failed
	case output.Closed:
		o.state = Idle
	}
	o.measurement, o.analysisDropped = n.device.Analysis(s.Epoch, s.Presented, n.base)
	if !o.playing {
		o.measurement.Valid = false
	}
	if err := n.device.Err(); err != nil {
		return o, err
	}
	select {
	case <-n.pipeline.Done():
		if err := n.pipeline.Wait(); err != nil {
			return o, err
		}
	default:
	}
	return o, nil
}

// The decoder has not entered a render worker yet. Duration arithmetic avoids
// float rounding and overflow; a known EOF is a valid, exact paused location.
func seekDecoder(ctx context.Context, d decode.Decoder, position time.Duration) (uint64, error) {
	info := d.Info()
	if position < 0 {
		return 0, ErrSeekRange
	}
	frame := int64(position/time.Second)*int64(info.Format.Rate) + int64(position%time.Second)*int64(info.Format.Rate)/int64(time.Second)
	if info.Frames >= 0 {
		frame = min(frame, info.Frames)
	}
	if frame > 0 && !info.ExactSeek {
		return 0, errors.New("engine: decoder does not support exact seek")
	}
	if err := d.Seek(ctx, frame); err != nil {
		return 0, err
	}
	return uint64(frame), nil
}
func (n *nativeTransport) close() error {
	// Stop the output before canceling its lifetime, so an ordinary command
	// cancellation does not turn a successful Close into a device failure.
	var err error
	if n.device != nil {
		err = n.device.Close()
	}
	n.cancel()
	if e := n.pipeline.Close(); e != nil && !errors.Is(e, context.Canceled) {
		err = errors.Join(err, e)
	}
	return err
}
