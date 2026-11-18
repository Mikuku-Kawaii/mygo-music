package buffer

import (
	"errors"
	"mygo-music/internal/audioengine/pcm"
	"sync/atomic"
)

// Blocks transfers decoder storage to Render without a pool or channel.
// Producer: Acquire -> fill -> Publish. Consumer: Peek -> read -> Release.
// Each side may have only one outstanding lease; holding a slice after release
// is forbidden. No reset while workers run. Error/EOF live in the final slot.
type Blocks struct {
	slots       []Slot
	write, read atomic.Uint64
}
type Slot struct {
	pcm.Block
	Float64 []float64
	Err     error
}

func NewBlocks(count, frames, channels int) (*Blocks, error) {
	return newBlocks(count, frames, channels, 4)
}

func NewFloat64Blocks(count, frames, channels int) (*Blocks, error) {
	return newBlocks(count, frames, channels, 8)
}

func newBlocks(count, frames, channels, bytes int) (*Blocks, error) {
	if count < 1 || frames < 1 || channels < 1 || channels > 8 || frames > (32<<20)/bytes/channels || count > (32<<20)/bytes/channels/frames {
		return nil, errors.New("buffer: decoder queue exceeds 32 MiB budget")
	}
	q := &Blocks{slots: make([]Slot, count)}
	for i := range q.slots {
		if bytes == 8 {
			q.slots[i].Float64 = make([]float64, frames*channels)
		} else {
			q.slots[i].Samples = make([]int32, frames*channels)
		}
	}
	return q, nil
}
func (q *Blocks) Acquire() *Slot {
	w := q.write.Load()
	if w-q.read.Load() == uint64(len(q.slots)) {
		return nil
	}
	s := &q.slots[w%uint64(len(q.slots))]
	s.Err = nil
	s.Frames = 0
	s.EOS = false
	s.Discontinuity = false
	return s
}
func (q *Blocks) Publish() { q.write.Add(1) }

func (q *Blocks) PCMBytes() int {
	return len(q.slots) * (len(q.slots[0].Samples)*4 + len(q.slots[0].Float64)*8)
}

func (q *Blocks) Buffered() int {
	read := q.read.Load()
	return int(min(q.write.Load()-read, uint64(len(q.slots))))
}

func (q *Blocks) Peek() *Slot {
	a := q.read.Load()
	if a == q.write.Load() {
		return nil
	}
	return &q.slots[a%uint64(len(q.slots))]
}
func (q *Blocks) Release() { q.read.Add(1) }
