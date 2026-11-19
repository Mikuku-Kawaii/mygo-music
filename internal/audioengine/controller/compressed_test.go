package controller

import (
	"context"
	"encoding/binary"
	"math"
	"path/filepath"
	"testing"
	"time"

	"mygo-music/internal/audioengine/decode"
	"mygo-music/internal/audioengine/dsp"
	"mygo-music/internal/audioengine/pcm"
	"mygo-music/internal/audioengine/render"
)

// MP3 fallback keeps its decoded samples and rate on the non-DSP WASAPI path.
func TestNativeMP3FallbackPacksDecodedPCM(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	path := filepath.Join("..", "..", "player", "testdata", "seek.mp3")
	d, err := decode.OpenProcessed(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if d != nil {
			d.Close()
		}
	}()
	info := d.Info()
	source := make([]int32, 2*1024)
	const start = 4096
	if err := d.Seek(ctx, start); err != nil {
		t.Fatal(err)
	}
	n, err := d.Read(ctx, source)
	if err != nil || n == 0 {
		t.Fatalf("could not decode the MP3 fixture: %d frames, %v", n, err)
	}
	if err := d.Seek(ctx, start); err != nil {
		t.Fatal(err)
	}
	config := Config{Shared: true, InitialVolume: .25, DSP: dsp.Config{Rate: 48000, PreampDB: -6}}
	out, opt := nativeRenderSettings(config, Media{DecodeCompressed: true}, info)
	if out.Kind != pcm.Integer || out.Rate != info.Format.Rate || out.ValidBits != 16 || out.ContainerBits != 16 ||
		info.NativePrecision || !opt.Shared || !opt.DSP.Bypass {
		t.Fatalf("unexpected PCM format or render settings: %+v, %+v", out, opt)
	}
	p, err := render.StartLive(ctx, d, out, pcm.Generation{}, start, opt)
	if err != nil {
		t.Fatal(err)
	}
	d = nil
	defer func() { cancel(); p.Wait() }()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for p.Ring.Buffered() < n {
		select {
		case <-tick.C:
		case <-p.Done():
			t.Fatalf("renderer stopped before delivering PCM: %v", p.Wait())
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	data := make([]byte, n*out.FrameBytes())
	if got := p.Ring.Read(data); got != n {
		t.Fatalf("received %d of %d frames", got, n)
	}
	var nonzero bool
	for i, x := range source[:n*2] {
		want := int16(math.Float32frombits(uint32(x)) * 32768)
		nonzero = nonzero || want != 0
		if got := int16(binary.LittleEndian.Uint16(data[i*2:])); got != want {
			t.Fatalf("decoded sample %d changed from %d to %d", i, want, got)
		}
	}
	if !nonzero {
		t.Fatal("the decoded sample range was silent")
	}
}
