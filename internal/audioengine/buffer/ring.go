// Package buffer contains preallocated single-producer/single-consumer queues.
package buffer

import (
	"errors"
	"sync/atomic"
)

// Ring transfers whole packed PCM frames. Only Render calls Write/Finish;
// only Output calls Read. Neither side waits for the other. Once published,
// bytes remain consumer-owned until Read copies them and advances the cursor.
// Reset requires both workers to have stopped.
type Ring struct {
	data                 []byte
	frameBytes, capacity uint64
	write                atomic.Uint64
	read                 atomic.Uint64
	finished             atomic.Bool
}

func NewRing(frames, frameBytes int) (*Ring, error) {
	if frames < 1 || frameBytes < 1 || frameBytes > 64 || frames > (4<<20)/frameBytes {
		return nil, errors.New("buffer: output ring exceeds 4 MiB budget")
	}
	return &Ring{data: make([]byte, frames*frameBytes), frameBytes: uint64(frameBytes), capacity: uint64(frames)}, nil
}
func (r *Ring) Capacity() int   { return int(r.capacity) }
func (r *Ring) FrameBytes() int { return int(r.frameBytes) }

// Buffered is diagnostic only; concurrent observations may be stale.
func (r *Ring) Buffered() int {
	a := r.read.Load()
	b := r.write.Load()
	return int(min(b-a, r.capacity))
}
func (r *Ring) Finished() bool { return r.finished.Load() }
func (r *Ring) Finish()        { r.finished.Store(true) }
func (r *Ring) Reset()         { r.read.Store(0); r.write.Store(0); r.finished.Store(false) }
func (r *Ring) Write(src []byte) int {
	if len(src)%int(r.frameBytes) != 0 || r.finished.Load() {
		return 0
	}
	w := r.write.Load()
	free := r.capacity - (w - r.read.Load())
	n := min(uint64(len(src))/r.frameBytes, free)
	first := min(n, r.capacity-w%r.capacity)
	off := (w % r.capacity) * r.frameBytes
	copy(r.data[off:off+first*r.frameBytes], src[:first*r.frameBytes])
	copy(r.data[:(n-first)*r.frameBytes], src[first*r.frameBytes:n*r.frameBytes])
	r.write.Store(w + n)
	return int(n)
}
func (r *Ring) Read(dst []byte) int {
	if len(dst)%int(r.frameBytes) != 0 {
		return 0
	}
	a := r.read.Load()
	n := min(uint64(len(dst))/r.frameBytes, r.write.Load()-a)
	first := min(n, r.capacity-a%r.capacity)
	off := (a % r.capacity) * r.frameBytes
	copy(dst[:first*r.frameBytes], r.data[off:off+first*r.frameBytes])
	copy(dst[first*r.frameBytes:n*r.frameBytes], r.data[:(n-first)*r.frameBytes])
	r.read.Store(a + n)
	return int(n)
}
