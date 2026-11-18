package render

import (
	"context"
	"io"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
	"time"

	"mygo-music/internal/audioengine/decode"
	"mygo-music/internal/audioengine/fixture"
	"mygo-music/internal/audioengine/pcm"
)

// Independent current/next decoders concatenate at a non-block-aligned boundary
// without zero insertion, duplicate frames, UI callbacks or float conversion.
func TestTwoDecoderContinuity(t *testing.T) {
	t.Run("WAV32-edge-sequence", func(t *testing.T) { testContinuity(t, 32, false) })
	t.Run("FLAC24-continuous-wave", func(t *testing.T) { testContinuity(t, 24, true) })
}
func testContinuity(t *testing.T, valid int, useFLAC bool) {
	f := pcm.Format{Kind: pcm.Integer, Rate: 48000, Channels: 2, ChannelMask: 3, ValidBits: valid, ContainerBits: 32}
	samples := fixture.Samples
	write := fixture.WriteWAV
	ext := ".wav"
	if useFLAC {
		samples = fixture.Wave
		write = fixture.WriteFLAC
		ext = ".flac"
	}
	dir := t.TempDir()
	var ds []decode.Decoder
	for i, n := range []int{2053, 4099} {
		start := 0
		if i == 1 {
			start = 2053
		}
		path := filepath.Join(dir, string(rune('a'+i))+ext)
		if err := write(path, f, samples(valid, start, n)); err != nil {
			t.Fatal(err)
		}
		d, err := decode.Open(context.Background(), path)
		if err != nil {
			t.Fatal(err)
		}
		ds = append(ds, d)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	p, err := Start(ctx, ds, f, pcm.Generation{Session: 1, Format: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	var wire []byte
	b := make([]byte, 113*f.FrameBytes())
	for {
		n := p.Ring.Read(b)
		wire = append(wire, b[:n*f.FrameBytes()]...)
		if p.Ring.Finished() && p.Ring.Buffered() == 0 {
			break
		}
		if ctx.Err() != nil {
			t.Fatal("pipeline stalled")
		}
		runtime.Gosched()
	}
	if err = p.Wait(); err != nil {
		t.Fatal(err)
	}
	back := make([]int32, len(wire)/4)
	pack, _ := pcm.NewPacker(f)
	if err = pack.Unpack(back, wire); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(back, samples(valid, 0, 6152)) {
		t.Fatal("boundary samples differ")
	}
}

type stalled struct {
	closed bool
	f      pcm.Format
}

func (d *stalled) Info() decode.Info {
	return decode.Info{Format: d.f, NativePrecision: true, Frames: -1}
}
func (d *stalled) Read(context.Context, []int32) (int, error) { return 0, pcm.ErrWouldBlock }
func (d *stalled) Seek(context.Context, int64) error          { return io.EOF }
func (d *stalled) Close() error                               { d.closed = true; return nil }

// Cancel wakes queues that cannot progress and joins their owning decoders.
func TestCancelStarvedPipeline(t *testing.T) {
	f := pcm.Format{Kind: pcm.Integer, Rate: 48000, Channels: 2, ChannelMask: 3, ValidBits: 16, ContainerBits: 16}
	d := &stalled{f: f}
	p, err := Start(context.Background(), []decode.Decoder{d}, f, pcm.Generation{})
	if err != nil {
		t.Fatal(err)
	}
	if err = p.Close(); err != context.Canceled {
		t.Fatal(err)
	}
	if !d.closed {
		t.Fatal("decoder leaked")
	}
}
