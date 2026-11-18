package player

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	engine "mygo-music/internal/audioengine/controller"
	"mygo-music/internal/audioengine/fixture"
	"mygo-music/internal/audioengine/pcm"
	"mygo-music/internal/local"
	"mygo-music/internal/music"
)

// The application advances and records presented local occurrences without
// consuming UI posts, including a same-file repeat on the existing stream.
func TestM2StrictLocalGaplessHardware(t *testing.T) {
	if os.Getenv("PRISM_NATIVE_SMOKE") != "1" {
		t.Skip("opt-in silent WASAPI integration")
	}
	for _, mode := range []OutputMode{OutputExclusive, OutputShared} {
		t.Run(string(mode), func(t *testing.T) {
			dir := t.TempDir()
			f := pcm.Format{Kind: pcm.Integer, Rate: 48000, Channels: 2, ChannelMask: 3, ValidBits: 16, ContainerBits: 16}
			const frames = 7531
			for i := range 3 {
				b := make([]int32, frames*2)
				if i == 1 {
					if err := fixture.WriteFLAC(filepath.Join(dir, "gapless-b.flac"), f, b); err != nil {
						t.Fatal(err)
					}
				} else if err := fixture.WriteWAV(filepath.Join(dir, fmt.Sprintf("gapless-%c.wav", 'a'+i)), f, b); err != nil {
					t.Fatal(err)
				}
			}
			src := local.New(t.TempDir())
			src.AddFolders(dir)
			deadline := time.Now().Add(5 * time.Second)
			for src.Scanning() {
				if time.Now().After(deadline) {
					t.Fatal("local scan did not finish")
				}
				time.Sleep(time.Millisecond)
			}
			queue, err := src.Search("gapless", music.SearchTracks, 0)
			if err != nil || len(queue.Tracks) != 3 {
				t.Fatal(queue, err)
			}
			p, _ := controlledPlayer(t, src)
			if err := p.SetOutput(OutputPreferences{Mode: mode}); err != nil {
				t.Fatal(err)
			}
			p.PlayQueue(queue.Tracks, 0)
			start := awaitPlayback(t, p, func(s PlaybackSnapshot) bool { return s.Ready && s.AudioOutput.Native.EffectivePlaying })
			s := awaitPlayback(t, p, func(s PlaybackSnapshot) bool { return !s.Playing && s.AudioOutput.Native.State == engine.Ended })
			n := s.AudioOutput.Native
			if s.At != 2 || n.ContinuationToken == 0 || n.Underruns != 0 || n.ContentSubmittedFrames != 3*frames ||
				n.AppliedGeneration.Format != start.AudioOutput.Native.AppliedGeneration.Format || n.PositionFrames != frames {
				t.Fatal(s.At, n)
			}
			t.Logf("3 tracks: epoch %d, %d frames, %d underruns", n.AppliedGeneration.Format, n.ContentSubmittedFrames, n.Underruns)
			p.Close()
			if len(src.Completions()) != 3 {
				t.Fatal(src.Completions())
			}

			p, _ = controlledPlayer(t, src)
			if err := p.SetOutput(OutputPreferences{Mode: mode}); err != nil {
				t.Fatal(err)
			}
			p.CycleRepeat()
			p.CycleRepeat()
			p.PlayQueue(queue.Tracks, 1)
			s = awaitPlayback(t, p, func(s PlaybackSnapshot) bool { return s.AudioOutput.Native.ContinuationToken >= 3 })
			n = s.AudioOutput.Native
			if s.At != 1 || n.Underruns != 0 || n.ContentSubmittedFrames < 3*frames || !s.Playing {
				t.Fatal(s.At, n)
			}
			p.TogglePlay()
			paused := awaitPlayback(t, p, func(s PlaybackSnapshot) bool { return s.Ready && !s.Loading && !s.Playing })
			p.Seek(0)
			s = awaitPlayback(t, p, func(s PlaybackSnapshot) bool { return s.Ready && !s.Loading && s.Position == 0 })
			if s.Playing || s.At != paused.At || s.AudioOutput.Native.ContinuationToken != 0 {
				t.Fatal(s.At, s.AudioOutput.Native)
			}
			t.Logf("repeat and paused seek: %d same-track continuations before reset", n.ContinuationToken)
		})
	}
}
