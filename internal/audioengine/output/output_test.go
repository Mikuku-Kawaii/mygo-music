package output

import (
	"bytes"
	"context"
	"errors"
	"runtime"
	"testing"
	"time"

	"mygo-music/internal/audioengine/buffer"
	"mygo-music/internal/audioengine/pcm"
)

// fakeDevice uses explicit events, never an elapsed-time-derived position.
type fakeDevice struct {
	format                 pcm.Format
	packet                 []byte
	events                 chan bool
	running                bool
	pos                    uint64
	startErr, errorOnEvent error
}

func newFake() *fakeDevice { return &fakeDevice{events: make(chan bool, 32)} }
func (f *fakeDevice) open(c Config) (Opened, error) {
	f.format = c.Format
	f.packet = make([]byte, 4*c.Format.FrameBytes())
	return Opened{Format: c.Format, BufferFrames: 4, ClockFrequency: uint64(c.Format.Rate) * 10}, nil
}
func (f *fakeDevice) wake() error      { f.events <- false; return nil }
func (f *fakeDevice) closeWake() error { return nil }
func (f *fakeDevice) wait(d time.Duration) (bool, error) {
	select {
	case audio := <-f.events:
		if audio && f.errorOnEvent != nil {
			return false, f.errorOnEvent
		}
		if audio && f.running {
			f.pos += 40
		}
		return audio, nil
	case <-time.After(d):
		return false, nil
	}
}
func (f *fakeDevice) buffer() ([]byte, error) { return f.packet, nil }
func (f *fakeDevice) release() error          { return nil }
func (f *fakeDevice) start() error {
	if f.startErr != nil {
		return f.startErr
	}
	f.running = true
	return nil
}
func (f *fakeDevice) stop() error                     { f.running = false; return nil }
func (f *fakeDevice) reset() error                    { f.pos = 0; return nil }
func (f *fakeDevice) position() (uint64, bool, error) { return f.pos, true, nil }
func (f *fakeDevice) close()                          {}
func testFormat() pcm.Format {
	return pcm.Format{Kind: pcm.Integer, Rate: 48000, Channels: 2, ChannelMask: 3, ValidBits: 24, ContainerBits: 32}
}
func waitFor(t *testing.T, s *Stream, condition func(Snapshot) bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !condition(s.Snapshot()) {
		if time.Now().After(deadline) {
			t.Fatal("worker stalled", s.Snapshot(), s.Err())
		}
		runtime.Gosched()
	}
}

// Ready is not Playing until native Start succeeds; Pause, Reset and Drain
// preserve intent, reset clock epochs and finish only at presentation.
func TestLifecycle(t *testing.T) {
	f := newFake()
	r, _ := buffer.NewRing(16, 8)
	r.Write(make([]byte, 8*8))
	r.Finish()
	s, err := openWith(context.Background(), Config{Format: testFormat(), Epoch: 1}, r, f)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.Snapshot().State != Ready {
		t.Fatal(s.Snapshot())
	}
	if err = s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s.Snapshot().Presented != 0 || s.Snapshot().Submitted != 4 {
		t.Fatal("prefill is not presentation", s.Snapshot())
	}
	if err = s.Pause(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err = s.Drain(context.Background()); err != ErrPaused {
		t.Fatal("paused drain", err)
	}
	f.events <- true
	if err = s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.events <- true
	waitFor(t, s, func(x Snapshot) bool { return x.Presented == 4 && x.Submitted == 8 })
	f.events <- true
	waitFor(t, s, func(x Snapshot) bool { return x.State == Ended })
	if s.Snapshot().ContentSubmitted != 8 || s.Snapshot().Underruns != 0 {
		t.Fatal(s.Snapshot())
	}
	next, _ := buffer.NewRing(16, 8)
	next.Write(make([]byte, 4*8))
	next.Finish()
	if err = s.Reset(context.Background(), next, 1); err == nil {
		t.Fatal("stale epoch accepted")
	}
	if err = s.Reset(context.Background(), next, 2); err != nil {
		t.Fatal(err)
	}
	if snap := s.Snapshot(); snap.State != Ready || snap.Presented != 0 || snap.Epoch != 2 {
		t.Fatal(snap)
	}
	if err = s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.events <- true
	waitFor(t, s, func(x Snapshot) bool { return x.State == Ended })
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	if err = s.Start(context.Background()); err != ErrClosed {
		t.Fatal(err)
	}
}

// A failed Start cannot publish a playing state or fall back to another backend.
func TestStartFailure(t *testing.T) {
	f := newFake()
	f.startErr = errors.New("exclusive busy")
	r, _ := buffer.NewRing(8, 8)
	r.Write(make([]byte, 64))
	s, err := openWith(context.Background(), Config{Format: testFormat()}, r, f)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.Start(context.Background()); !errors.Is(err, f.startErr) {
		t.Fatal(err)
	}
	<-s.Done()
	if s.Snapshot().State != Failed {
		t.Fatal(s.Snapshot())
	}
}

// A device removal wakes the worker and terminates it instead of simulating
// playback using wall time or waiting forever on the missing device.
func TestDeviceFailure(t *testing.T) {
	f := newFake()
	f.errorOnEvent = errors.New("device invalidated")
	r, _ := buffer.NewRing(8, 8)
	r.Write(make([]byte, 64))
	s, err := openWith(context.Background(), Config{Format: testFormat()}, r, f)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.events <- true
	<-s.Done()
	if !errors.Is(s.Err(), f.errorOnEvent) || s.Snapshot().State != Failed {
		t.Fatal(s.Err())
	}
}

// The exact bytes supplied by Render reach the native packet. A short live
// supply gets legal silence and loses continuity; a finite tail is separate.
func TestSubmissionAndUnderrun(t *testing.T) {
	f := newFake()
	info, _ := f.open(Config{Format: testFormat()})
	s := &Stream{n: f}
	r, _ := buffer.NewRing(8, 8)
	samples := []int32{-8388608, 8388607, -1, 1}
	p, _ := pcm.NewPacker(testFormat())
	wire := make([]byte, 16)
	if err := p.Pack(wire, samples); err != nil {
		t.Fatal(err)
	}
	r.Write(wire)
	var end uint64
	known := false
	if err := s.fill(r, info, &end, &known); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(f.packet[:16], wire) || !bytes.Equal(f.packet[16:], make([]byte, 16)) {
		t.Fatal("native packet differs")
	}
	if snap := s.Snapshot(); snap.Underruns != 1 || snap.UnderrunFrames != 2 || snap.Submitted != 4 || known {
		t.Fatal(snap, known)
	}
	r.Write(wire)
	r.Finish()
	if err := s.fill(r, info, &end, &known); err != nil {
		t.Fatal(err)
	}
	if !known || end != 6 || s.Snapshot().TrailingSilence != 2 || s.Snapshot().Underruns != 1 {
		t.Fatal(end, known, s.Snapshot())
	}
}

// Output packet copies, counters and silence handling allocate no storage.
func TestOutputFillAllocations(t *testing.T) {
	f := newFake()
	info, _ := f.open(Config{Format: testFormat()})
	s := &Stream{n: f}
	r, _ := buffer.NewRing(8, 8)
	b := make([]byte, 32)
	var end uint64
	known := false
	if n := testing.AllocsPerRun(100, func() {
		r.Write(b)
		if err := s.fill(r, info, &end, &known); err != nil {
			panic(err)
		}
	}); n != 0 {
		t.Fatalf("%g allocations", n)
	}
}
