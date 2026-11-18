package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"
	engine "mygo-music/internal/audioengine/controller"
	"mygo-music/internal/audioengine/fixture"
	"mygo-music/internal/audioengine/pcm"
	"mygo-music/internal/music"
	"mygo-music/internal/player"
)

// Releasing the original progress bar seeks a real silent native session once,
// preserving pause, the current song, and the compatible volume preference.
func TestNativeSeekDragHardware(t *testing.T) {
	if os.Getenv("PRISM_NATIVE_SMOKE") != "1" {
		t.Skip("opt-in silent WASAPI UI test")
	}
	for _, mode := range []player.OutputMode{player.OutputExclusive, player.OutputShared} {
		t.Run(string(mode), func(t *testing.T) { nativeSeekDrag(t, mode) })
	}
}
func nativeSeekDrag(t *testing.T, mode player.OutputMode) {
	path := filepath.Join(t.TempDir(), "silent.wav")
	f := pcm.Format{Kind: pcm.Integer, Rate: 48000, Channels: 2, ChannelMask: 3, ValidBits: 16, ContainerBits: 16}
	if err := fixture.WriteWAV(path, f, make([]int32, 20*48000*2)); err != nil {
		t.Fatal(err)
	}
	a := newTestApp()
	defer a.pl.Close()
	fake(a).stream = func(string, string) (*music.Stream, error) { return &music.Stream{URL: music.FileURL(path)}, nil }
	if err := a.pl.SetOutput(player.OutputPreferences{Mode: mode, EndpointID: os.Getenv("PRISM_NATIVE_ENDPOINT")}); err != nil {
		t.Fatal(err)
	}
	a.volume = .3
	a.pl.SetVolume(.3)
	queue := []music.Track{song("a", "Silent seek verification"), song("b", "Next")}
	for i := range queue {
		queue[i].Source = "fake"
	}
	a.pl.PlayQueue(queue, 0)
	a.pl.TogglePlay()
	wait := func(ready func(player.PlaybackSnapshot) bool) player.PlaybackSnapshot {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for {
			s := a.pl.Snapshot()
			if s.Error != "" {
				t.Fatal(s.Error)
			}
			if ready(s) {
				return s
			}
			if time.Now().After(deadline) {
				t.Fatalf("native state timeout: %+v", s)
			}
			time.Sleep(time.Millisecond)
		}
	}
	wait(func(s player.PlaybackSnapshot) bool { return s.Ready && s.AudioOutput.Native.State == engine.Paused })
	tt := ui.NewTester(func(c *ui.Context) {
		ui.Column(c).Gap(10).Children(func() {
			a.scrubber(c, &a.barScrub, ui.RGB(30, 30, 30), ui.RGB(200, 200, 200), 20, ui.Color{}).Width(400)
			a.volumeSlider(c, ui.RGB(200, 200, 200), ui.RGB(30, 30, 30), 180)
		})
	}, 500, 100)
	settled(tt)
	n, ok := tt.AccessByRole(ui.RoleSlider, "Progress")
	if !ok || n.Disabled {
		t.Fatal(n)
	}
	x, y := n.Bounds.X, n.Bounds.Y+n.Bounds.H/2
	tt.Press(x+n.Bounds.W*.25, y)
	tt.Frame()
	tt.Move(x+n.Bounds.W*.6, y-30)
	tt.Frame()
	if a.pl.AudioOutput().Native.RequestedGeneration.Seek != 0 {
		t.Fatal("seek issued before release")
	}
	tt.Release(x+n.Bounds.W*.6, y-30)
	tt.Frame()
	tt.Frame()
	s := wait(func(s player.PlaybackSnapshot) bool {
		return s.Ready && s.AudioOutput.Native.AppliedGeneration.Seek >= 1
	})
	native := s.AudioOutput.Native
	if native.AppliedGeneration.Seek != 1 || s.Playing || native.EffectivePlaying || s.Current.ID != "a" || a.volume != .3 || a.pl.CanAdjustVolume() != (mode == player.OutputShared) {
		t.Fatal(s)
	}
	if s.Position < 11*time.Second || s.Position > 13*time.Second || native.PositionFrames != native.SourceStartFrame || !native.PositionExact || native.ContentSubmittedFrames != 0 {
		t.Fatal(s)
	}
	if mode == player.OutputShared {
		position := s.Position
		v, ok := tt.AccessByRole(ui.RoleSlider, "Volume")
		if !ok || v.Disabled {
			t.Fatal("shared volume unavailable", v)
		}
		tt.ClickAt(v.Bounds.X+v.Bounds.W*.8, v.Bounds.Y+v.Bounds.H/2)
		settled(tt)
		s = wait(func(s player.PlaybackSnapshot) bool {
			return s.AudioOutput.Native.AppliedGeneration.Config > native.AppliedGeneration.Config
		})
		if s.Playing || s.Position != position || s.AudioOutput.Native.AppliedVolume < .62 || s.AudioOutput.Native.AppliedVolume > .66 {
			t.Fatal(s)
		}
		native = s.AudioOutput.Native
	}
	b, _ := json.Marshal(native)
	t.Log(string(b))
	if dir := os.Getenv("MM_FRAMES"); dir != "" {
		a.settingsPage = "audio"
		frame := ui.NewTester(a.settingsView, 780, 1000)
		settled(frame)
		saveFrame(t, dir, string(mode)+"-seek.png", frame.Image())
	}
}
