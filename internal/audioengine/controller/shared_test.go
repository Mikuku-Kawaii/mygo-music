package controller

import (
	"context"
	"encoding/json"
	"mygo-music/internal/audioengine/fixture"
	"mygo-music/internal/audioengine/pcm"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// A shared device accepts source-rate PCM and reports the Windows mix format;
// presentation still maps to source frames through the stream's clock frequency.
func TestSharedHardware(t *testing.T) {
	if os.Getenv("PRISM_NATIVE_SMOKE") != "1" {
		t.Skip("opt-in silent shared WASAPI test")
	}
	for _, rate := range []int{44100, 48000, 96000} {
		t.Run(strconv.Itoa(rate), func(t *testing.T) {
			ctx := context.Background()
			f := pcm.Format{Kind: pcm.Integer, Rate: rate, Channels: 2, ChannelMask: 3, ValidBits: 24, ContainerBits: 24}
			path := filepath.Join(t.TempDir(), "shared.wav")
			if err := fixture.WriteWAV(path, f, make([]int32, rate)); err != nil {
				t.Fatal(err)
			}
			h, err := New(Config{Shared: true})
			if err != nil {
				t.Fatal(err)
			}
			defer h.Close()
			r, e := h.SetQueue(ctx, []Media{{Item: Item{Source: "local", ID: "shared"}, Path: path}}, 0)
			await(t, r, e)
			r, e = h.SetVolume(ctx, .25)
			await(t, r, e)
			r, e = h.Play(ctx)
			await(t, r, e)
			s := waitSnapshot(t, h, func(s Snapshot) bool { return s.State == Ended || s.State == Failed })
			if s.State != Ended || !s.Output.Shared || s.Output.MixFormat.Rate == 0 || s.Output.Format.Rate != rate || s.PositionFrames != uint64(rate/2) || s.Underruns != 0 {
				t.Fatal(s)
			}
			b, _ := json.Marshal(s)
			t.Log(string(b))
			r, e = h.Seek(ctx, 100*time.Millisecond)
			await(t, r, e)
		})
	}
}
