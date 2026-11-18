// Package render joins prepared native PCM blocks and packs the output ring.
// It does not own the application's queue or touch UI/plugin/device APIs.
package render

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"mygo-music/internal/audioengine/buffer"
	"mygo-music/internal/audioengine/decode"
	"mygo-music/internal/audioengine/pcm"
)

// Pipeline is the M0 two-decoder harness. It consumes one or two already-open
// decoders of the same native format. Close cancels and joins every worker.
// Application session/queue ownership remains a future controller concern.
type Pipeline struct {
	Ring                                 *buffer.Ring
	Format                               pcm.Format
	done                                 chan struct{}
	cancel                               context.CancelFunc
	err                                  error // written before done closes
	boundary                             atomic.Pointer[Boundary]
	readyNext                            atomic.Pointer[prepared]
	volume, appliedVolume                atomic.Pointer[gainUpdate]
	latency, tail                        int
	opened                               time.Time // live pipeline telemetry, outside the sample loop
	lastWrite, decodeWait, maxDecodeWait atomic.Int64
	currentPCM                           atomic.Int64
}

func Start(ctx context.Context, ds []decode.Decoder, out pcm.Format, g pcm.Generation) (*Pipeline, error) {
	return StartAt(ctx, ds, out, g, make([]int64, len(ds)))
}

// StartAt owns already-positioned decoders on success. starts identifies each
// decoder's current source frame, so blocks retain truthful source positions.
func StartAt(ctx context.Context, ds []decode.Decoder, out pcm.Format, g pcm.Generation, starts []int64) (*Pipeline, error) {
	if len(ds) < 1 || len(ds) > 2 {
		return nil, errors.New("render: expected current and optional next decoder")
	}
	if len(starts) != len(ds) {
		return nil, errors.New("render: missing source anchors")
	}
	for i, start := range starts {
		if start < 0 || ds[i].Info().Frames >= 0 && start > ds[i].Info().Frames {
			return nil, errors.New("render: invalid source anchor")
		}
	}
	pack, err := pcm.NewPacker(out)
	if err != nil {
		return nil, err
	}
	for _, d := range ds {
		if !d.Info().NativePrecision || !out.NativeEqual(d.Info().Format) {
			return nil, errors.New("render: strict native format conflict")
		}
	}
	ring, err := buffer.NewRing((out.Rate*80+999)/1000, out.FrameBytes())
	if err != nil {
		return nil, err
	}
	qs := make([]*buffer.Blocks, len(ds))
	for i := range qs {
		// Two seconds each, limited to 32 MiB per queue (64 MiB total).
		blocks := min((out.Rate*2+2047)/2048, (32<<20)/(2048*out.Channels*4))
		qs[i], err = buffer.NewBlocks(blocks, 2048, out.Channels)
		if err != nil {
			return nil, err
		}
	}
	ctx, cancel := context.WithCancel(ctx)
	p := &Pipeline{Ring: ring, Format: out, done: make(chan struct{}), cancel: cancel}
	var workers sync.WaitGroup
	for i, d := range ds {
		workers.Add(1)
		start := starts[i]
		go func() { defer workers.Done(); defer d.Close(); decodeInto(ctx, d, qs[i], g, uint64(i+1), start) }()
	}
	go func() {
		defer close(p.done)
		defer workers.Wait()
		defer cancel()
		tick := time.NewTicker(time.Millisecond)
		defer tick.Stop()
		scratch := make([]byte, 2048*out.FrameBytes())
		for _, q := range qs {
			for {
				if err := ctx.Err(); err != nil {
					p.err = err
					return
				}
				s := q.Peek()
				if s == nil {
					select {
					case <-ctx.Done():
						p.err = ctx.Err()
						return
					case <-tick.C:
					}
					continue
				}
				if s.Generation != g {
					q.Release()
					continue
				}
				if s.Err != nil {
					p.err = s.Err
					q.Release()
					return
				}
				if err := pack.Pack(scratch[:s.Frames*out.FrameBytes()], s.Samples[:s.Frames*out.Channels]); err != nil {
					p.err = err
					q.Release()
					return
				}
				for at := 0; at < s.Frames; {
					at += ring.Write(scratch[at*out.FrameBytes() : s.Frames*out.FrameBytes()])
					if at < s.Frames {
						select {
						case <-ctx.Done():
							p.err = ctx.Err()
							return
						case <-tick.C:
						}
					}
				}
				end := s.EOS
				q.Release()
				if end {
					break
				}
			}
		}
		ring.Finish()
	}()
	return p, nil
}
func decodeInto(ctx context.Context, d decode.Decoder, q *buffer.Blocks, g pcm.Generation, source uint64, pos int64) {
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	floating, _ := d.(decode.Float64Decoder)
	for {
		if ctx.Err() != nil {
			return
		}
		s := q.Acquire()
		if s == nil {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
			continue
		}
		var n int
		var err error
		if floating != nil {
			n, err = floating.ReadFloat64(ctx, s.Float64)
		} else {
			n, err = d.Read(ctx, s.Samples)
		}
		if errors.Is(err, pcm.ErrWouldBlock) && n == 0 {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
			continue
		}
		if errors.Is(err, pcm.ErrWouldBlock) {
			err = nil
		}
		if n == 0 && err == nil {
			err = io.ErrNoProgress
		}
		s.Generation = g
		s.SourceToken = source
		s.SourceFrame = pos
		s.Frames = n
		pos += int64(n)
		if err == io.EOF {
			s.EOS = true
		} else {
			s.Err = err
		}
		q.Publish()
		if err != nil {
			return
		}
	}
}
func (p *Pipeline) Done() <-chan struct{} { return p.done }
func (p *Pipeline) Wait() error           { <-p.done; return p.err }
func (p *Pipeline) Close() error          { p.cancel(); return p.Wait() }
