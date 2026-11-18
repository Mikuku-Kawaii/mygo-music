package render

import (
	"context"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"mygo-music/internal/audioengine/decode"
	"mygo-music/internal/audioengine/dsp"
	"mygo-music/internal/audioengine/fixture"
	"mygo-music/internal/audioengine/pcm"
)

// The application renderer submits the exact split sequence, including
// 32-bit LSBs and the non-block-aligned WAV/FLAC boundary.
func TestLiveStrictContinuationSamples(t *testing.T) {
	for _, bits := range []int{24, 32} {
		f := pcm.Format{Kind: pcm.Integer, Rate: 48000, Channels: 2, ChannelMask: 3, ValidBits: bits, ContainerBits: 32}
		dir := t.TempDir()
		first, second := filepath.Join(dir, "A.wav"), filepath.Join(dir, "B.wav")
		if err := fixture.WriteWAV(first, f, fixture.Samples(bits, 0, 5217)); err != nil {
			t.Fatal(err)
		}
		if bits == 24 {
			second = filepath.Join(dir, "B.flac")
			if err := fixture.WriteFLAC(second, f, fixture.Samples(bits, 5217, 4019)); err != nil {
				t.Fatal(err)
			}
		} else if err := fixture.WriteWAV(second, f, fixture.Samples(bits, 5217, 4019)); err != nil {
			t.Fatal(err)
		}
		slot := new(NextSlot)
		slot.Offer(NextMedia{Token: 9, Path: second})
		got, b := rendered(t, first, f, LiveOptions{Next: slot})
		if b == nil || b.Media.Token != 9 || b.OutputFrame != 5217 || !slices.Equal(got, fixture.Samples(bits, 0, 9236)) {
			t.Fatal(bits, len(got), b)
		}
	}
}

// An obsolete plan is canceled while opening. The successor can be
// replaced without joining stale decoded PCM into the current epoch.
func TestLiveCancelPreRead(t *testing.T) {
	dir := t.TempDir()
	f := pcm.Format{Kind: pcm.Integer, Rate: 48000, Channels: 2, ChannelMask: 3, ValidBits: 16, ContainerBits: 16}
	first, second := filepath.Join(dir, "A.wav"), filepath.Join(dir, "B.wav")
	if err := fixture.WriteWAV(first, f, fixture.Samples(16, 0, 10011)); err != nil {
		t.Fatal(err)
	}
	if err := fixture.WriteWAV(second, f, fixture.Samples(16, 10011, 4131)); err != nil {
		t.Fatal(err)
	}
	slot := new(NextSlot)
	opening, canceled := make(chan struct{}), make(chan struct{})
	slot.Offer(NextMedia{Token: 1, Resolve: func(ctx context.Context) (string, dsp.Gains, error) {
		close(opening)
		<-ctx.Done()
		close(canceled)
		return "", dsp.Gains{}, ctx.Err()
	}})
	p, ctx := livePipe(t, first, f, LiveOptions{Next: slot})
	select {
	case <-opening:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if !slot.Offer(NextMedia{Token: 2, Path: second}) {
		t.Fatal("obsolete offer committed")
	}
	select {
	case <-canceled:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	waitLiveNext(t, ctx, p, 2)
	result := collect(t, ctx, p)
	b := p.Boundary()
	if b == nil || b.Media.Token != 2 || !slices.Equal(result, fixture.Samples(16, 0, 14142)) {
		t.Fatal(b, len(result))
	}
}

// Source-rate DSP preserves filter/SRC/limiter state at a same-rate split;
// fixed-output DSP compensates each new rate without adding blank frames.
func TestLiveDSPContinuation(t *testing.T) {
	dir := t.TempDir()
	f := pcm.Format{Kind: pcm.Integer, Rate: 96000, Channels: 2, ChannelMask: 3, ValidBits: 24, ContainerBits: 24}
	out := pcm.Format{Kind: pcm.Integer, Rate: 48000, Channels: 2, ChannelMask: 3, ValidBits: 32, ContainerBits: 32}
	a, b, whole := filepath.Join(dir, "a.wav"), filepath.Join(dir, "b.wav"), filepath.Join(dir, "whole.wav")
	const first, second = 14601, 4153
	if err := fixture.WriteWAV(a, f, fixture.Wave(24, 0, first)); err != nil {
		t.Fatal(err)
	}
	if err := fixture.WriteWAV(b, f, fixture.Wave(24, first, second)); err != nil {
		t.Fatal(err)
	}
	if err := fixture.WriteWAV(whole, f, fixture.Wave(24, 0, first+second)); err != nil {
		t.Fatal(err)
	}
	c := dsp.Config{Rate: 48000, Limit: true, PreampDB: -3}
	c.Filters[0] = dsp.Filter{Kind: dsp.Peak, Enabled: true, Hz: 2200, DB: 6, Q: .7}
	opts := LiveOptions{Processing: true, DSP: c, Volume: .8}
	ref, _ := rendered(t, whole, out, opts)
	slot := new(NextSlot)
	slot.Offer(NextMedia{Token: 3, Path: b})
	opts.Next = slot
	got, at := rendered(t, a, out, opts)
	if at == nil || at.OutputFrame != (first+1)/2 || !slices.Equal(got, ref) {
		t.Fatal("DSP state reset or boundary miscount", at, len(got), len(ref))
	}
	f.Rate = 44100
	b = filepath.Join(dir, "44100.wav")
	if err := fixture.WriteWAV(b, f, fixture.Wave(24, 0, second)); err != nil {
		t.Fatal(err)
	}
	opts.Next = nil
	ar, _ := rendered(t, a, out, opts)
	br, _ := rendered(t, b, out, opts)
	slot = new(NextSlot)
	slot.Offer(NextMedia{Token: 4, Path: b})
	opts.Next = slot
	got, at = rendered(t, a, out, opts)
	if at == nil || at.OutputFrame != uint64(len(ar)/2) || !slices.Equal(got, append(ar, br...)) {
		t.Fatal("fixed format boundary altered duration or inserted a gap", at, len(got))
	}
}

func livePipe(t *testing.T, path string, out pcm.Format, opts LiveOptions) (*Pipeline, context.Context) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	d, err := decode.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	p, err := StartLive(ctx, d, out, pcm.Generation{Session: 1, Format: 1}, 0, opts)
	if err != nil {
		d.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	return p, ctx
}
func rendered(t *testing.T, path string, out pcm.Format, opt LiveOptions) ([]int32, *Boundary) {
	t.Helper()
	p, ctx := livePipe(t, path, out, opt)
	if opt.Next != nil {
		waitLiveNext(t, ctx, p, opt.Next.plan.Load().media.Token)
	}
	result := collect(t, ctx, p)
	return result, p.Boundary()
}

func waitLiveNext(t *testing.T, ctx context.Context, p *Pipeline, token uint64) {
	t.Helper()
	// The first file fills the ring. Open and prepare the successor before
	// draining it so the stitch assertion is independent of test host speed.
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		ready := p.readyNext.Load()
		if ready != nil && ready.request.media.Token == token && ready.blocks.Buffered() > 0 {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatal("successor wasn't prepared", ctx.Err())
		case <-tick.C:
		}
	}
}
func collect(t *testing.T, ctx context.Context, p *Pipeline) []int32 {
	t.Helper()
	fb := p.Format.FrameBytes()
	buf := make([]byte, 311*fb)
	var wire []byte
	for {
		n := p.Ring.Read(buf)
		wire = append(wire, buf[:n*fb]...)
		if p.Ring.Finished() && p.Ring.Buffered() == 0 {
			break
		}
		if ctx.Err() != nil {
			t.Fatal(ctx.Err())
		}
		if n == 0 {
			time.Sleep(time.Millisecond)
		}
	}
	if err := p.Wait(); err != nil {
		t.Fatal(err)
	}
	result := make([]int32, len(wire)/fb*2)
	pack, _ := pcm.NewPacker(p.Format)
	if err := pack.Unpack(result, wire); err != nil {
		t.Fatal(err)
	}
	return result
}
