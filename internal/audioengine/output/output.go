// Package output owns native audio streams and their presentation clocks. It has
// no dependency on MyGo, the application player, decoders or plugins.
package output

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"mygo-music/internal/audioengine/analysis"
	"mygo-music/internal/audioengine/buffer"
	"mygo-music/internal/audioengine/pcm"
	"mygo-music/internal/audioengine/timeline"
)

type Support struct {
	Format    pcm.Format
	Supported bool
	HRESULT   uint32
}
type Device struct {
	ID, Name                               string
	Default                                bool
	DefaultPeriod100ns, MinimumPeriod100ns int64
	Formats                                []Support
	Error                                  string
}
type Config struct {
	Shared       bool
	DoP          bool
	ASIO         string // CLSID of an installed ASIO driver
	StableBuffer bool   // WASAPI buffer policy
	EndpointID   string
	Format       pcm.Format
	Period       time.Duration
	Epoch        uint64
	Analysis     *analysis.Subscription
}
type Opened struct {
	Shared         bool
	DoP            bool
	ASIO           bool
	StableBuffer   bool
	DriverFormat   pcm.Format
	MixFormat      pcm.Format // Windows mix format; Format is the submitted PCM
	EndpointID     string
	Format         pcm.Format
	BufferFrames   int
	Period         time.Duration
	Latency        time.Duration
	ClockFrequency uint64
	MMCSS          bool
}
type State uint32

const (
	Ready State = iota
	Playing
	Paused
	Draining
	Ended
	Failed
	Closed
	setGain // internal command; never published as a stream state
)

type Snapshot struct {
	State                                                                                                      State
	Epoch, Submitted, ContentSubmitted, Presented, Underruns, UnderrunFrames, TrailingSilence, ClockInaccurate uint64
	RingLowWater, RingBuffered, RingCapacity                                                                   int
	MaxService, MaxPacketGap                                                                                   time.Duration
	LastUnderrunFrame                                                                                          uint64
}

var (
	ErrUnsupported = errors.New("output: WASAPI requires supported integer stereo PCM on Windows amd64")
	ErrClosed      = errors.New("output: stream closed")
	ErrStarved     = errors.New("output: prefill not ready")
	ErrPaused      = errors.New("output: drain requires a playing stream")
)

// native is confined to one locked OS thread, including close on open failure.
type native interface {
	open(Config) (Opened, error)
	wake() error
	closeWake() error                 // called by Close after joining the worker and all callers
	wait(time.Duration) (bool, error) // true = audio event; false = control/timeout
	buffer() ([]byte, error)
	release() error
	start() error
	stop() error
	reset() error
	position() (uint64, bool, error)
	close()
}

// pullNative services packets on a driver-owned callback. The locked control
// thread keeps command and device lifetime ownership in either backend.
type pullNative interface {
	attach(*Stream, *buffer.Ring, Opened)
	endFrame() (uint64, bool)
}
type command struct {
	volume float64
	kind   State
	ring   *buffer.Ring
	epoch  uint64
	ctx    context.Context
	reply  chan error
}
type counters struct {
	state                                                                                   atomic.Uint32
	epoch, submitted, content, presented, underruns, missing, trailing, inaccurate, service atomic.Uint64
	packetGap, lastUnderrun                                                                 atomic.Uint64
	low, buffered, capacity                                                                 atomic.Int64
}
type Stream struct {
	Info     Opened
	commands chan command
	done     chan struct{}
	mu       sync.Mutex // callers only; never acquired by Output
	closed   bool
	n        native
	stats    counters
	tap      *analysis.Tap // published with Info before ready signals
	err      error         // immutable after done closes
}

// Open prepares a shared or exclusive stream but does not start it. Start succeeds only
// after prefill and IAudioClient.Start. Caller must Close even after a device
// failure; Close serializes native wake-handle disposal with command senders.
func Open(ctx context.Context, c Config, r *buffer.Ring) (*Stream, error) {
	if c.ASIO != "" {
		if c.Shared || c.DoP || c.StableBuffer {
			return nil, ErrASIOFormat
		}
		return openWith(ctx, c, r, newASIO())
	}
	return openWith(ctx, c, r, newNative())
}
func openWith(ctx context.Context, c Config, r *buffer.Ring, n native) (*Stream, error) {
	if err := c.Format.Validate(); err != nil {
		return nil, err
	}
	if c.Format.Kind != pcm.Integer || c.Format.Channels != 2 || c.Format.ChannelMask != 3 {
		return nil, ErrUnsupported
	}
	if c.DoP && (c.Shared || c.EndpointID == "" || c.Format.ValidBits != 24 ||
		(c.Format.Rate != 176400 && c.Format.Rate != 352800)) {
		return nil, ErrDoPFormat
	}
	if r == nil || r.FrameBytes() != c.Format.FrameBytes() {
		return nil, pcm.ErrBuffer
	}
	if c.Period < 0 || c.Period > time.Second {
		return nil, errors.New("output: invalid device period")
	}
	s := &Stream{commands: make(chan command, 8), done: make(chan struct{}), n: n}
	ready := make(chan error, 1)
	go s.run(ctx, c, r, ready)
	if err := <-ready; err != nil {
		<-s.done
		n.closeWake()
		return nil, err
	}
	return s, nil
}
func (s *Stream) Snapshot() Snapshot {
	return Snapshot{State: State(s.stats.state.Load()), Epoch: s.stats.epoch.Load(),
		Submitted: s.stats.submitted.Load(), ContentSubmitted: s.stats.content.Load(), Presented: s.stats.presented.Load(),
		Underruns: s.stats.underruns.Load(), UnderrunFrames: s.stats.missing.Load(), TrailingSilence: s.stats.trailing.Load(), ClockInaccurate: s.stats.inaccurate.Load(),
		RingLowWater: int(s.stats.low.Load()), RingBuffered: int(s.stats.buffered.Load()), RingCapacity: int(s.stats.capacity.Load()),
		MaxService: time.Duration(s.stats.service.Load()), MaxPacketGap: time.Duration(s.stats.packetGap.Load()), LastUnderrunFrame: s.stats.lastUnderrun.Load()}
}

func (s *Stream) Analysis(epoch, presented, contentStart uint64) (analysis.Measurement, uint64) {
	if s.tap == nil {
		return analysis.Measurement{}, 0
	}
	return s.tap.At(epoch, presented, contentStart), s.tap.Dropped()
}

func (s *Stream) Done() <-chan struct{} { return s.done }
func (s *Stream) Err() error {
	select {
	case <-s.done:
		return s.err
	default:
		return nil
	}
}
func (s *Stream) Start(ctx context.Context) error { return s.send(ctx, Playing, nil, 0) }
func (s *Stream) Pause(ctx context.Context) error { return s.send(ctx, Paused, nil, 0) }
func (s *Stream) Drain(ctx context.Context) error { return s.send(ctx, Draining, nil, 0) }

// SetVolume changes a shared stream's system gain on its device thread.
func (s *Stream) SetVolume(ctx context.Context, gain float64) error {
	if !s.Info.Shared || !(gain >= 0 && gain <= 1) {
		return errors.New("output: invalid shared gain")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	return s.request(ctx, command{kind: setGain, volume: gain, ctx: ctx, reply: make(chan error, 1)})
}

// Reset installs a different, prepared ring and a newer epoch, leaving playback
// paused. The old Render must be canceled separately; it cannot write the new
// ring. Reset never mutates shared cursors underneath a running producer.
func (s *Stream) Reset(ctx context.Context, r *buffer.Ring, epoch uint64) error {
	return s.send(ctx, Ready, r, epoch)
}
func (s *Stream) send(ctx context.Context, kind State, r *buffer.Ring, epoch uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	return s.request(ctx, command{kind: kind, ring: r, epoch: epoch, ctx: ctx, reply: make(chan error, 1)})
}
func (s *Stream) request(ctx context.Context, c command) error {
	select {
	case <-s.done:
		if s.err != nil {
			return s.err
		}
		return ErrClosed
	default:
	}
	select {
	case s.commands <- c:
	case <-ctx.Done():
		return ctx.Err()
	case <-s.done:
		return ErrClosed
	}
	if err := s.n.wake(); err != nil {
		return err
	}
	select {
	case err := <-c.reply:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-s.done:
		return s.err
	}
}
func (s *Stream) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return s.err
	}
	s.closed = true
	select {
	case <-s.done:
	default:
		_ = s.request(context.Background(), command{kind: Closed, ctx: context.Background(), reply: make(chan error, 1)})
	}
	<-s.done
	err := s.n.closeWake()
	if s.err != nil {
		return s.err
	}
	return err
}

func (s *Stream) run(ctx context.Context, c Config, r *buffer.Ring, ready chan<- error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	defer close(s.done)
	defer func() {
		s.n.close()
		if s.tap != nil {
			s.tap.Close()
		}
	}()
	info, err := s.n.open(c)
	if err != nil {
		s.err = err
		s.stats.state.Store(uint32(Failed))
		ready <- err
		return
	}
	if info.BufferFrames > r.Capacity() {
		err = errors.New("output: negotiated device packet exceeds ring capacity")
		s.err = err
		ready <- err
		return
	}
	info.DoP = c.DoP
	s.Info = info
	s.stats.epoch.Store(c.Epoch)
	s.stats.low.Store(int64(r.Capacity()))
	s.stats.capacity.Store(int64(r.Capacity()))
	s.stats.buffered.Store(int64(r.Buffered()))
	if !info.DoP {
		s.tap = analysis.New(c.Analysis, info.Format, info.BufferFrames)
	}
	pull, _ := s.n.(pullNative)
	if pull != nil {
		pull.attach(s, r, info)
	}
	ready <- nil
	clock, err := timeline.NewClock(c.Epoch, 0, 0, info.ClockFrequency, uint32(c.Format.Rate))
	if err != nil {
		s.fail(err)
		return
	}
	state := Ready
	prefilled := false
	var finalFrame uint64
	finalKnown := false
	lastAdvance := time.Now()
	var lastPosition uint64
	var lastPacket time.Time
	for {
		if err := ctx.Err(); err != nil {
			s.fail(err)
			return
		}
		select {
		case cmd := <-s.commands:
			if err := cmd.ctx.Err(); err != nil {
				cmd.reply <- err
				continue
			}
			var e error
			nativeCall := false
			switch cmd.kind {
			case setGain:
				if v, ok := s.n.(interface{ volume(float64) error }); ok && info.Shared {
					nativeCall = true
					e = v.volume(cmd.volume)
				} else {
					e = errors.New("output: shared volume unavailable")
				}
			case Closed:
				_ = s.n.stop()
				s.stats.state.Store(uint32(Closed))
				cmd.reply <- nil
				return
			case Playing:
				if state == Ended {
					e = errors.New("output: reset required after drain")
					break
				}
				if state == Playing || state == Draining {
					break
				}
				if !prefilled {
					if r.Buffered() < info.BufferFrames && !r.Finished() {
						e = ErrStarved
						break
					}
					if pull == nil {
						nativeCall = true
						e = s.fill(r, info, &finalFrame, &finalKnown)
						if e != nil {
							break
						}
					}
					prefilled = true
				}
				nativeCall = true
				e = s.n.start()
				if e == nil {
					state = Playing
					lastAdvance = time.Now()
					lastPacket = time.Time{}
				}
			case Paused:
				nativeCall = true
				e = s.n.stop()
				if e == nil {
					state = Paused
				}
			case Draining:
				if state != Playing && state != Draining {
					e = ErrPaused
				} else {
					state = Draining
				}
			case Ready:
				if pull != nil {
					e = errors.New("ASIO reset requires a reopened stream")
					break
				}
				if cmd.ring == nil || cmd.ring == r || cmd.ring.FrameBytes() != c.Format.FrameBytes() || cmd.ring.Capacity() < info.BufferFrames || cmd.epoch <= s.stats.epoch.Load() {
					e = errors.New("output: reset requires a new compatible ring and newer epoch")
					break
				}
				nativeCall = true
				e = s.n.stop()
				if e == nil {
					e = s.n.reset()
				}
				if e == nil {
					r = cmd.ring
					c.Epoch = cmd.epoch
					clock, _ = timeline.NewClock(c.Epoch, 0, 0, info.ClockFrequency, uint32(c.Format.Rate))
					prefilled = false
					finalKnown = false
					finalFrame = 0
					lastPosition = 0
					state = Ready
					s.stats.epoch.Store(c.Epoch)
					s.stats.submitted.Store(0)
					s.stats.content.Store(0)
					s.stats.presented.Store(0)
					s.stats.underruns.Store(0)
					s.stats.missing.Store(0)
					s.stats.trailing.Store(0)
					s.stats.inaccurate.Store(0)
					s.stats.low.Store(int64(r.Capacity()))
					s.stats.capacity.Store(int64(r.Capacity()))
					s.stats.buffered.Store(int64(r.Buffered()))
					s.stats.service.Store(0)
					s.stats.packetGap.Store(0)
					s.stats.lastUnderrun.Store(0)
				}
			}
			s.stats.state.Store(uint32(state))
			cmd.reply <- e
			if nativeCall && e != nil {
				s.fail(e)
				return
			}
		default:
		}
		audioEvent, e := s.n.wait(20 * time.Millisecond)
		if e != nil {
			s.fail(e)
			return
		}
		if state != Playing && state != Draining {
			continue
		}
		position, accurate, e := s.n.position()
		if e != nil {
			s.fail(e)
			return
		}
		if !accurate {
			s.stats.inaccurate.Add(1)
		} else {
			if position < lastPosition {
				s.fail(errors.New("output: device clock regressed"))
				return
			}
			presented, e := clock.Presented(c.Epoch, position, s.stats.submitted.Load())
			if e != nil {
				s.fail(e)
				return
			}
			s.stats.presented.Store(presented)
			if position > lastPosition {
				lastPosition = position
				lastAdvance = time.Now()
			}
		}
		if time.Since(lastAdvance) > 2*time.Second {
			s.fail(ErrClockStopped)
			return
		}
		if pull != nil {
			finalFrame, finalKnown = pull.endFrame()
			if finalKnown {
				state = Draining
				s.stats.state.Store(uint32(state))
			}
		}
		if finalKnown && s.stats.presented.Load() >= finalFrame {
			if e = s.n.stop(); e != nil {
				s.fail(e)
				return
			}
			state = Ended
			s.stats.state.Store(uint32(state))
			continue
		}
		if audioEvent {
			before := time.Now()
			if !lastPacket.IsZero() && uint64(before.Sub(lastPacket)) > s.stats.packetGap.Load() {
				s.stats.packetGap.Store(uint64(before.Sub(lastPacket)))
			}
			lastPacket = before
			e = s.fill(r, info, &finalFrame, &finalKnown)
			elapsed := time.Since(before)
			if uint64(elapsed) > s.stats.service.Load() {
				s.stats.service.Store(uint64(elapsed))
			}
			if e != nil {
				s.fail(e)
				return
			}
			if finalKnown {
				state = Draining
				s.stats.state.Store(uint32(state))
			}
		}
	}
}
func (s *Stream) fail(err error) { s.err = err; s.stats.state.Store(uint32(Failed)) }
func (s *Stream) fill(r *buffer.Ring, info Opened, finalFrame *uint64, finalKnown *bool) error {
	b, err := s.n.buffer()
	if err != nil {
		return err
	}
	if len(b) == 0 {
		return nil
	}
	frames := len(b) / info.Format.FrameBytes()
	// Capture finished BEFORE Read, so observing it guarantees all committed
	// final bytes are visible. A producer finishing concurrently is handled on
	// the next event; a short live read is truthfully counted as underrun.
	finished := r.Finished()
	n := r.Read(b)
	base := s.stats.submitted.Load()
	if info.DoP {
		frameDoP(b, info.Format.ContainerBits/8, n, base)
	} else {
		clear(b[n*info.Format.FrameBytes():])
	}
	buffered := r.Buffered()
	s.stats.buffered.Store(int64(buffered))
	if int64(buffered) < s.stats.low.Load() {
		s.stats.low.Store(int64(buffered))
	}
	content := s.stats.content.Load()
	captured := s.tap != nil && s.tap.Stage(b[:n*info.Format.FrameBytes()], s.stats.epoch.Load(), base, content)
	if err = s.n.release(); err != nil {
		return err
	}
	if captured {
		s.tap.Commit()
	}
	s.stats.submitted.Add(uint64(frames))
	s.stats.content.Add(uint64(n))
	if finished && r.Buffered() == 0 && !*finalKnown {
		*finalFrame = base + uint64(n)
		*finalKnown = true
	}
	if n < frames {
		if finished || *finalKnown {
			s.stats.trailing.Add(uint64(frames - n))
		} else {
			s.stats.lastUnderrun.Store(base + uint64(n))
			s.stats.missing.Add(uint64(frames - n))
			s.stats.underruns.Add(1)
		}
	}
	return nil
}
