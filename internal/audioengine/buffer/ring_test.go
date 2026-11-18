package buffer

import (
	"encoding/binary"
	"runtime"
	"testing"
)

// A producer lapping the physical ring must never overwrite unread frames.
func TestConcurrentWrap(t *testing.T) {
	r, _ := NewRing(17, 4)
	done := make(chan struct{})
	go func() {
		defer close(done)
		var b [4]byte
		for i := uint32(0); i < 100000; i++ {
			binary.LittleEndian.PutUint32(b[:], i)
			for r.Write(b[:]) == 0 {
				runtime.Gosched()
			}
		}
		r.Finish()
	}()
	var b [28]byte
	want := uint32(0)
	for want < 100000 {
		n := r.Read(b[:])
		for i := range n {
			if got := binary.LittleEndian.Uint32(b[i*4:]); got != want {
				t.Fatalf("got %d want %d", got, want)
			}
			want++
		}
		if n == 0 {
			runtime.Gosched()
		}
	}
	<-done
	if !r.Finished() || r.Buffered() != 0 {
		t.Fatal("not drained")
	}
}

// Reusing bounded output storage must not allocate on steady-state transfers.
func TestRingAllocations(t *testing.T) {
	r, _ := NewRing(1024, 8)
	b := make([]byte, 4096)
	if n := testing.AllocsPerRun(100, func() { r.Write(b); r.Read(b) }); n != 0 {
		t.Fatal(n)
	}
}

// A published decoder slot remains unavailable to its producer until release.
func TestBlockOwnership(t *testing.T) {
	q, _ := NewBlocks(1, 2, 2)
	s := q.Acquire()
	s.Samples[0] = 123
	s.Frames = 1
	q.Publish()
	if q.Acquire() != nil {
		t.Fatal("unreleased slot reused")
	}
	if q.Peek().Samples[0] != 123 {
		t.Fatal("sample changed")
	}
	q.Release()
	if q.Acquire() == nil {
		t.Fatal("slot not returned")
	}
}
