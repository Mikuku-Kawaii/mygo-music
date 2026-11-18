package render

import (
	"context"
	"errors"
	"math"
	"sync/atomic"
	"time"

	"mygo-music/internal/audioengine/buffer"
	"mygo-music/internal/audioengine/decode"
	"mygo-music/internal/audioengine/dsp"
	"mygo-music/internal/audioengine/pcm"
)

// NextMedia is one continuation chosen by PlaybackController. Tokens identify
// occurrences, including repeated copies of the same media ID.
type NextMedia struct {
	Token                   uint64
	Source, ID, Title, Path string
	Gains                   dsp.Gains
	Resolve                 func(context.Context) (string, dsp.Gains, error)
}

type offer struct {
	media    NextMedia
	original *offer // populated only after Render wins the boundary
}

// NextSlot is a replaceable one-item plan. The application's controller is
// the only caller that changes plans; Render may atomically commit one.
type NextSlot struct{ plan atomic.Pointer[offer] }

func (s *NextSlot) Offer(media NextMedia) bool {
	o := &offer{media: media}
	for {
		old := s.plan.Load()
		if old != nil && old.original != nil {
			return false
		}
		if s.plan.CompareAndSwap(old, o) {
			return true
		}
	}
}

// Clear returns false if the old plan is already in the output pipeline.
func (s *NextSlot) Clear() bool {
	for {
		old := s.plan.Load()
		if old == nil {
			return true
		}
		if old.original != nil {
			return false
		}
		if s.plan.CompareAndSwap(old, nil) {
			return true
		}
	}
}

// Acknowledge frees the slot after the presentation clock reaches the boundary.
func (s *NextSlot) Acknowledge(token uint64) {
	if o := s.plan.Load(); o != nil && o.original != nil && o.media.Token == token {
		s.plan.CompareAndSwap(o, nil)
	}
}

type Boundary struct {
	Media         NextMedia
	Source        decode.Info
	OutputFrame   uint64
	Latency, Tail int
}

type LiveOptions struct {
	StableBuffer bool
	Next         *NextSlot
	Shared       bool
	Processing   bool
	DoP          bool
	DSP          dsp.Config
	Gains        dsp.Gains
	Volume       float64
	Seed         uint64
}

type prepared struct {
	request, committed *offer
	source             decode.Info
	blocks             *buffer.Blocks
	processor          *dsp.Processor
	boundary           *Boundary
	used               atomic.Bool
}

func (p *Pipeline) Boundary() *Boundary { return p.boundary.Load() }
func (p *Pipeline) LatencyFrames() int  { return p.latency }
func (p *Pipeline) TailFrames() int     { return p.tail }

func (p *Pipeline) SetLevels(amp, preamp float64, revision uint64, accepted time.Time) {
	if old := p.volume.Load(); old == nil || old.amp != amp || old.preamp != preamp || old.revision != revision {
		p.volume.Store(&gainUpdate{amp: amp, preamp: preamp, revision: revision, accepted: accepted})
	}
}
func (p *Pipeline) AppliedLevels() (volume, preamp float64, revision uint64, enqueueDelay time.Duration) {
	v := p.appliedVolume.Load()
	if v == nil {
		return 0, 0, 0, 0
	}
	return v.amp, v.preamp, v.revision, v.delay
}

type gainUpdate struct {
	amp, preamp float64
	revision    uint64
	accepted    time.Time
	delay       time.Duration // set once, before appliedVolume publishes it
}

// Progress is sampled outside Render. Ages describe this observation, not
// necessarily what the producer was doing at an earlier underrun.
type Progress struct {
	PCMQueueBytes                           int
	LastWriteAge, DecodeWait, MaxDecodeWait time.Duration
}

func (p *Pipeline) Progress() Progress {
	s := Progress{PCMQueueBytes: int(p.currentPCM.Load()), MaxDecodeWait: time.Duration(p.maxDecodeWait.Load())}
	if q := p.readyNext.Load(); q != nil && !q.used.Load() {
		s.PCMQueueBytes += q.blocks.PCMBytes()
	}
	now := time.Since(p.opened)
	if at := p.lastWrite.Load(); at != 0 {
		s.LastWriteAge = max(now-time.Duration(at), 0)
	}
	if at := p.decodeWait.Load(); at != 0 {
		s.DecodeWait = max(now-time.Duration(at), 0)
		s.MaxDecodeWait = max(s.MaxDecodeWait, s.DecodeWait)
	}
	return s
}

// StartLive renders one track and any later plans compatible with the
// negotiated output. It owns d on success.
func StartLive(parent context.Context, d decode.Decoder, out pcm.Format, g pcm.Generation, start int64, opt LiveOptions) (*Pipeline, error) {
	in := d.Info()
	if in.Format.Channels != 2 || out.Channels != 2 || in.Format.ChannelMask != 3 || out.ChannelMask != 3 {
		return nil, errors.New("render: native output requires stereo PCM")
	}
	pack, err := pcm.NewPacker(out)
	if err != nil {
		return nil, err
	}
	var process *dsp.Processor
	if opt.DoP {
		if opt.Processing || opt.Shared || in.DSDRate == 0 || !out.NativeEqual(in.Format) {
			return nil, errors.New("render: invalid DSD carrier path")
		}
		// Each track begins a fresh carrier epoch.
		opt.Next = nil
	} else if opt.Processing {
		process, err = dsp.Prepare(opt.DSP, in.Format.Rate, out.Rate, out.ValidBits, opt.Gains, opt.Volume, opt.Seed)
		if err != nil {
			return nil, err
		}
	} else if !in.NativePrecision || !out.NativeEqual(in.Format) {
		return nil, errors.New("render: strict native format conflict")
	}
	if process != nil {
		process.SetInputBits(in.Format.ValidBits)
	}
	bufferMS := 80
	switch {
	case opt.StableBuffer:
		bufferMS = 400
	case opt.Processing:
		// Mixed-rate DSP stress exposed scheduler and GC stalls beyond
		// the native PCM margin. Keep a bounded reserve for the processor.
		bufferMS = 120
	case !opt.Shared:
		// An exclusive driver can request many packets back to back at
		// start, before its clock advances by a full device period.
		bufferMS = 160
	}
	ring, err := buffer.NewRing((out.Rate*bufferMS+999)/1000, out.FrameBytes())
	if err != nil {
		return nil, err
	}
	q, err := makeQueue(in.Format, 2)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(parent)
	p := &Pipeline{Ring: ring, Format: out, done: make(chan struct{}), cancel: cancel, opened: time.Now()}
	p.currentPCM.Store(int64(q.PCMBytes()))
	if process != nil {
		p.latency, p.tail = process.LatencyFrames(), process.TailFrames()
		p.appliedVolume.Store(&gainUpdate{amp: opt.Volume, preamp: opt.DSP.PreampDB, revision: g.Config})
	}
	currentDone, nextDone := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(currentDone)
		defer d.Close()
		decodeInto(ctx, d, q, g, 0, start)
	}()
	go func() {
		defer close(nextDone)
		prepareNext(ctx, &p.readyNext, out, g, in, opt)
	}()
	go func() {
		defer close(p.done)
		live := liveRender{p: p, ctx: ctx, pack: pack, scratch: make([]byte, 2048*out.FrameBytes()),
			samples: make([]int32, 4096), process: process, opt: opt, gains: opt.Gains}
		live.tick = time.NewTicker(time.Millisecond)
		p.err = live.run(q, in, &p.readyNext)
		live.tick.Stop()
		cancel()
		<-currentDone
		<-nextDone
	}()
	return p, nil
}

func makeQueue(format pcm.Format, seconds int) (*buffer.Blocks, error) {
	blocks := (format.Rate*seconds + 2047) / 2048
	if format.Kind == pcm.Float64 {
		return buffer.NewFloat64Blocks(min(blocks, (32<<20)/(2048*2*8)), 2048, 2)
	}
	return buffer.NewBlocks(min(blocks, (32<<20)/(2048*2*4)), 2048, 2)
}

func prepareNext(ctx context.Context, ready *atomic.Pointer[prepared], out pcm.Format, generation pcm.Generation, first decode.Info, opt LiveOptions) {
	if opt.Next == nil || !first.Trimmed {
		return
	}
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	type preRead struct {
		request *offer
		cancel  context.CancelFunc
		done    chan struct{}
	}
	var pending, current *preRead
	defer func() {
		if pending != nil {
			pending.cancel()
			<-pending.done
		}
		if current != nil {
			current.cancel()
			<-current.done
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		want := opt.Next.plan.Load()
		if want != nil && want.original != nil {
			continue
		}
		if pending != nil && pending.request != want {
			c := ready.Load()
			if c != nil && c.request == pending.request && c.used.Load() {
				// Acknowledging the boundary frees the mailbox, not its
				// now-current decoder. Leave that worker running while the
				// other slot prepares the new successor.
				if current != nil {
					select {
					case <-current.done:
						current.cancel()
					default:
						continue
					}
				}
				current = pending
			} else {
				pending.cancel()
				select {
				case <-pending.done:
				default:
					continue
				}
			}
			pending = nil
			ready.Store(nil)
		}
		if want == nil || pending != nil {
			continue
		}
		jobCtx, stop := context.WithCancel(ctx)
		pending = &preRead{request: want, cancel: stop, done: make(chan struct{})}
		go func(want *offer, done chan struct{}) {
			defer close(done)
			media := want.media
			if media.Resolve != nil {
				var err error
				media.Path, media.Gains, err = media.Resolve(jobCtx)
				if err != nil {
					return
				}
				media.Resolve = nil
			}
			open := decode.Open
			if opt.Processing {
				open = decode.OpenProcessed
			}
			d, err := open(jobCtx, media.Path)
			if err != nil {
				return
			}
			defer d.Close()
			info := d.Info()
			if !info.Trimmed || info.Frames <= 0 || info.Format.Channels != 2 || info.Format.ChannelMask != 3 {
				return
			}
			if opt.Processing {
				if opt.DSP.Rate == 0 && info.Format.Rate != out.Rate {
					return
				}
				if opt.DSP.Validate(info.Format.Rate) != nil {
					return
				}
			} else if !info.NativePrecision || !out.NativeEqual(info.Format) {
				return
			}
			q, err := makeQueue(info.Format, 1)
			if err != nil {
				return
			}
			c := &prepared{request: want, committed: &offer{media: media, original: want}, source: info, blocks: q,
				boundary: &Boundary{Media: media, Source: info}}
			if opt.Processing {
				c.processor, err = dsp.Prepare(opt.DSP, info.Format.Rate, out.Rate, out.ValidBits, media.Gains, opt.Volume, opt.Seed+media.Token)
				if err != nil {
					return
				}
			}
			ready.Store(c)
			decodeInto(jobCtx, d, q, generation, want.media.Token, 0)
		}(want, pending.done)
	}
}

type liveRender struct {
	p                          *Pipeline
	ctx                        context.Context
	pack                       *pcm.Packer
	scratch                    []byte
	samples                    []int32
	n                          int
	tick                       *time.Ticker
	process                    *dsp.Processor
	opt                        LiveOptions
	written, base, sourceCount uint64
	volume                     *gainUpdate
	gainReadyAt                uint64
	gains                      dsp.Gains
}

func (l *liveRender) run(q *buffer.Blocks, in decode.Info, ready *atomic.Pointer[prepared]) error {
	emit := l.emit
	var waiting time.Duration
	for {
		if err := l.ctx.Err(); err != nil {
			return err
		}
		s := q.Peek()
		if s == nil {
			if waiting == 0 {
				waiting = time.Since(l.p.opened)
				l.p.decodeWait.Store(int64(waiting))
			}
			if err := l.wait(); err != nil {
				return err
			}
			continue
		}
		if waiting != 0 {
			delay := time.Since(l.p.opened) - waiting
			l.p.maxDecodeWait.Store(max(l.p.maxDecodeWait.Load(), int64(delay)))
			l.p.decodeWait.Store(0)
			waiting = 0
		}
		if s.Err != nil {
			q.Release()
			return s.Err
		}
		if l.process != nil {
			l.applyLevels()
			scale := math.Ldexp(1, 1-in.Format.ValidBits)
			for i := 0; i < s.Frames; i++ {
				var v dsp.Frame
				switch in.Format.Kind {
				case pcm.Float64:
					v = dsp.Frame{s.Float64[2*i], s.Float64[2*i+1]}
				case pcm.Float32:
					v = dsp.Frame{float64(math.Float32frombits(uint32(s.Samples[2*i]))),
						float64(math.Float32frombits(uint32(s.Samples[2*i+1])))}
				default:
					v = dsp.Frame{float64(s.Samples[2*i]) * scale, float64(s.Samples[2*i+1]) * scale}
				}
				if err := l.process.Process(v, emit); err != nil {
					q.Release()
					return err
				}
			}
			l.sourceCount += uint64(s.Frames)
			if err := l.flush(); err != nil {
				q.Release()
				return err
			}
		} else if err := l.write(s.Samples[:2*s.Frames]); err != nil {
			q.Release()
			return err
		}
		eos := s.EOS
		q.Release()
		if !eos {
			continue
		}
		next := l.takeNext(ready)
		if next == nil {
			if err := l.drain(emit); err != nil {
				return err
			}
			l.p.Ring.Finish()
			return nil
		}
		at := l.written
		latency, tail := 0, 0
		if l.process != nil {
			l.gains = next.boundary.Media.Gains
			if in.Format.Rate != next.source.Format.Rate {
				if err := l.drain(emit); err != nil {
					return err
				}
				at = l.written
				l.base, l.sourceCount = at, 0
				l.process = next.processor
				if l.volume != nil {
					l.process.SetInitialVolume(l.volume.amp)
				}
				l.process.SetInitialGain(l.opt.DSP.TrackGain(l.gains))
				l.volume = nil
			} else {
				// Filter, converter, limiter, and dither state stay continuous.
				l.process.SetGain(l.opt.DSP.TrackGain(l.gains))
				rate := uint64(in.Format.Rate)
				at = l.base + (l.sourceCount*uint64(l.p.Format.Rate)+rate-1)/rate
			}
			l.process.SetInputBits(next.source.Format.ValidBits)
			latency, tail = l.process.LatencyFrames(), l.process.TailFrames()
		}
		b := next.boundary
		b.OutputFrame, b.Latency, b.Tail = at, latency, tail
		l.p.boundary.Store(b)
		in, q = next.source, next.blocks
		l.p.currentPCM.Store(int64(q.PCMBytes()))
	}
}

func (l *liveRender) takeNext(ready *atomic.Pointer[prepared]) *prepared {
	if l.opt.Next == nil {
		return nil
	}
	start := time.Now()
	waitingForPresentation := false
	for l.ctx.Err() == nil {
		o := l.opt.Next.plan.Load()
		if o == nil && !waitingForPresentation {
			return nil
		}
		if o != nil && o.original != nil {
			waitingForPresentation = true
		}
		n := ready.Load()
		if n != nil && n.request == o {
			if first := n.blocks.Peek(); first != nil && first.Err == nil && first.Frames > 0 {
				if l.opt.Next.plan.CompareAndSwap(n.request, n.committed) {
					n.used.Store(true)
					return n
				}
			}
		}
		// Use available audio headroom, with a short initial priming
		// allowance for files shorter than a device period.
		timeout := 25 * time.Millisecond
		headroom := l.p.Format.Rate / 50
		if waitingForPresentation {
			// A very short track can finish rendering before its start is
			// presented. Let the app fill the freed slot while PCM drains.
			timeout, headroom = 70*time.Millisecond, l.p.Format.Rate/100
		}
		if time.Since(start) >= timeout || l.p.Ring.Buffered() < headroom && time.Since(start) >= 5*time.Millisecond {
			break
		}
		if l.wait() != nil {
			break
		}
	}
	return nil
}

func (l *liveRender) applyLevels() {
	if v := l.p.volume.Load(); v != nil && v != l.volume {
		l.opt.DSP.PreampDB = v.preamp
		l.process.SetGain(l.opt.DSP.TrackGain(l.gains))
		l.process.SetVolume(v.amp)
		l.volume = v
		l.gainReadyAt = l.written + uint64(l.n/2) + l.process.PendingFrames()
	}
}

func (l *liveRender) drain(emit func([2]int32) error) error {
	if l.process == nil {
		return nil
	}
	if err := l.process.Drain(emit); err != nil {
		return err
	}
	return l.flush()
}

func (l *liveRender) emit(v [2]int32) error {
	l.samples[l.n], l.samples[l.n+1] = v[0], v[1]
	l.n += 2
	if l.n == len(l.samples) {
		return l.flush()
	}
	return nil
}

func (l *liveRender) flush() error {
	n := l.n
	l.n = 0
	return l.write(l.samples[:n])
}

func (l *liveRender) write(samples []int32) error {
	fb := l.p.Format.FrameBytes()
	b := l.scratch[:len(samples)/2*fb]
	if err := l.pack.Pack(b, samples); err != nil {
		return err
	}
	for len(b) > 0 {
		n := l.p.Ring.Write(b)
		l.written += uint64(n)
		if n != 0 {
			l.p.lastWrite.Store(int64(time.Since(l.p.opened)))
		}
		if l.volume != nil && l.written > l.gainReadyAt && l.p.appliedVolume.Load() != l.volume {
			if !l.volume.accepted.IsZero() {
				l.volume.delay = time.Since(l.volume.accepted)
			}
			l.p.appliedVolume.Store(l.volume)
		}
		b = b[n*fb:]
		if len(b) > 0 {
			if err := l.wait(); err != nil {
				return err
			}
		}
	}
	return nil
}

func (l *liveRender) wait() error {
	select {
	case <-l.ctx.Done():
		return l.ctx.Err()
	case <-l.tick.C:
		return nil
	}
}
