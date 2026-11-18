// Package analysis measures copies of PCM committed by the output device
// thread. Only measurements selected by the presentation cursor leave it.
package analysis

import (
	"context"
	"math/bits"
	"sync"
	"sync/atomic"
	"time"

	"mygo-music/internal/audioengine/pcm"
)

const (
	Bands          = 24
	packetSlots    = 8
	maxPacketBytes = 128 << 10
	resultSlots    = 16
)

// Subscription belongs to the application. Odd tokens enable sampling. A
// visibility change invalidates queued copies and already published results.
type Subscription struct{ token atomic.Uint64 }

func (s *Subscription) SetEnabled(enabled bool) {
	for {
		old := s.token.Load()
		if (old&1 != 0) == enabled || s.token.CompareAndSwap(old, old+1) {
			return
		}
	}
}

func (s *Subscription) Token() uint64 { return s.token.Load() }

// Measurement describes stereo PCM submitted by the application. Through is
// exclusive in device frames. ContentStart excludes inserted output silence.
type Measurement struct {
	Valid                        bool
	Token, Epoch                 uint64
	First, Through, ContentStart uint64
	Peak, RMS                    [2]float64
	Bands                        [Bands]float32
}

type packet struct {
	token, epoch, first, content uint64
	data                         []byte
	length                       int
}

// Tap has one producer (Output) and one consumer (the analysis goroutine).
// The slot pool and analysis working storage are fixed for an opened stream.
type Tap struct {
	sub                 *Subscription
	packets             [packetSlots]packet
	head, tail, dropped atomic.Uint64
	done                chan struct{}
	cancel              context.CancelFunc
	worker              *meter
	results             [resultSlots]Measurement
	resultMu            sync.Mutex // analysis worker and control readers only
	next                int
}

func New(sub *Subscription, format pcm.Format, packetFrames int) *Tap {
	if sub == nil || format.Validate() != nil || format.Kind != pcm.Integer || format.Channels != 2 || packetFrames <= 0 {
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	t := &Tap{sub: sub, cancel: cancel, done: make(chan struct{}), worker: newMeter(format)}
	size := min(packetFrames, maxPacketBytes/format.FrameBytes()) * format.FrameBytes()
	for i := range t.packets {
		t.packets[i].data = make([]byte, size)
	}
	go t.run(ctx)
	return t
}

// Stage copies PCM while the caller owns the device buffer. Commit publishes
// it only if ReleaseBuffer succeeded. Neither operation waits or allocates.
func (t *Tap) Stage(data []byte, epoch, first, content uint64) bool {
	token := t.sub.Token()
	if token&1 == 0 || len(data) == 0 {
		return false
	}
	tail := t.tail.Load()
	p := &t.packets[tail%packetSlots]
	if tail-t.head.Load() >= packetSlots || len(data) > len(p.data) {
		t.dropped.Add(1)
		return false
	}
	copy(p.data, data)
	p.token, p.epoch, p.first, p.content, p.length = token, epoch, first, content, len(data)
	return true
}

func (t *Tap) Commit()         { t.tail.Add(1) }
func (t *Tap) Dropped() uint64 { return t.dropped.Load() }

func (t *Tap) Close() {
	t.cancel()
	<-t.done
}

// At selects a completed window at or immediately behind the cursor. Whole
// windows on the previous track, before a seek, or behind an old subscription
// are excluded. Missing measurements remain distinguishable from silence.
func (t *Tap) At(epoch, cursor, contentStart uint64) Measurement {
	token := t.sub.Token()
	if token&1 == 0 {
		return Measurement{}
	}
	t.resultMu.Lock()
	defer t.resultMu.Unlock()
	for i := range len(t.results) {
		m := t.results[(t.next+len(t.results)-1-i)%len(t.results)]
		if m.Valid && m.Epoch == epoch && m.Token == token && m.Through <= cursor &&
			m.ContentStart >= contentStart && cursor-m.Through <= uint64(2*len(t.worker.window)) {
			return m
		}
	}
	return Measurement{}
}

func (t *Tap) run(ctx context.Context) {
	defer close(t.done)
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		token := t.sub.Token()
		head, tail := t.head.Load(), t.tail.Load()
		for ; head < tail; head++ {
			p := &t.packets[head%packetSlots]
			if p.token == token {
				t.worker.feed(p, t.publish)
			}
			t.head.Store(head + 1)
		}
		delay := 5 * time.Millisecond
		if token&1 == 0 {
			delay = 30 * time.Millisecond
		}
		timer.Reset(delay)
	}
}

func (t *Tap) publish(m Measurement) {
	t.resultMu.Lock()
	t.results[t.next] = m
	t.next = (t.next + 1) % len(t.results)
	t.resultMu.Unlock()
}

func windowSize(rate int) int {
	// Roughly 20–40 ms of full-rate PCM. No decimation or aliasing stage.
	return 1 << bits.Len(uint(max(64, (rate+49)/50)-1))
}
