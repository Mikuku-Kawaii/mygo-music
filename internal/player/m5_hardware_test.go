//go:build windows && amd64

package player

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	engine "mygo-music/internal/audioengine/controller"
	"mygo-music/internal/audioengine/dsd"
	"mygo-music/internal/audioengine/dsp"
	"mygo-music/internal/audioengine/fixture"
	"mygo-music/internal/audioengine/pcm"
	"mygo-music/internal/local"
	"mygo-music/internal/music"
)

// The library submits both DSD rates through the selected Windows path and
// preserves pause while preparing the exact remaining frames after seek.
func TestM5DSDPlaybackHardware(t *testing.T) {
	endpoint := os.Getenv("PRISM_M5_WASAPI_ENDPOINT")
	if endpoint == "" {
		t.Skip("opt-in silent DSD device test")
	}
	mode := OutputProcessed
	if os.Getenv("PRISM_M5_DOP") == "1" {
		mode = OutputDoP
	}
	dir := t.TempDir()
	for _, rate := range []int{2822400, 5644800} {
		path := filepath.Join(dir, fmt.Sprintf("dsd%d.dsf", rate/44100))
		if err := fixture.WriteDSF(path, rate, rate, func(int) (byte, byte) { return dsd.Idle, dsd.Idle }); err != nil {
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
	queue, err := src.Search("dsd", music.SearchTracks, 0)
	if err != nil || len(queue.Tracks) != 2 {
		t.Fatal(queue, err)
	}
	for _, track := range queue.Tracks {
		t.Run(track.Name, func(t *testing.T) {
			if track.Audio.DSDRate == 0 || track.Audio.Frames != int64(track.Audio.DSDRate) {
				t.Fatal(track.Audio)
			}
			p, _ := controlledPlayer(t, src)
			defer p.Close()
			p.SetVolume(0)
			cfg := dsp.Config{Rate: 48000, SwapChannels: true, Crossfeed: .1, Balance: -.2}
			cfg.Graphic[5] = 2
			if err := p.SetOutput(OutputPreferences{Mode: mode, EndpointID: endpoint, DSP: cfg}); err != nil {
				t.Fatal(err)
			}
			p.PlayQueue([]music.Track{track}, 0)
			start := awaitPlayback(t, p, func(s PlaybackSnapshot) bool { return s.Ready && s.AudioOutput.Native.EffectivePlaying })
			n := start.AudioOutput.Native
			sourceRate, outputRate := 88200, 48000
			if mode == OutputDoP {
				sourceRate = track.Audio.DSDRate / 16
				outputRate = sourceRate
				if !n.Output.DoP || n.Processing || n.Output.Format.ValidBits != 24 || n.Measurement.Valid || p.CanAdjustVolume() || n.AppliedVolume != 1 {
					t.Fatal(n)
				}
			} else if !n.Processing || n.Source.Format.Kind != pcm.Float64 || n.DSP != cfg {
				t.Fatal(n)
			}
			if n.Source.DSDRate != track.Audio.DSDRate || n.Source.Format.Rate != sourceRate || n.Output.Format.Rate != outputRate {
				t.Fatal(n)
			}
			p.TogglePlay()
			awaitPlayback(t, p, func(s PlaybackSnapshot) bool { return s.Ready && !s.Playing && !s.Loading })
			p.Seek(250 * time.Millisecond)
			seek := awaitPlayback(t, p, func(s PlaybackSnapshot) bool {
				return s.Ready && !s.Loading && s.AudioOutput.Native.SourceStartFrame == uint64(sourceRate/4)
			})
			if seek.Playing {
				t.Fatal("DSD seek resumed playback")
			}
			p.TogglePlay()
			end := awaitPlayback(t, p, func(s PlaybackSnapshot) bool { return !s.Playing && s.AudioOutput.Native.State == engine.Ended })
			n = end.AudioOutput.Native
			if n.PositionFrames != uint64(sourceRate) || n.ContentSubmittedFrames != uint64(outputRate*3/4) || n.Underruns != 0 || n.OutputStats.ClockInaccurate != 0 {
				t.Fatal(n)
			}
			t.Logf("%s DSD%d: %d Hz decode, %d frames submitted at %d Hz after seek, %d lookahead frames, %d underruns", mode, n.Source.DSDRate/44100, n.Source.Format.Rate, n.ContentSubmittedFrames, n.Output.Format.Rate, n.LatencyFrames, n.Underruns)
		})
	}
}
