//go:build windows && amd64

package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/egoist/mygo"
	"mygo-music/internal/audioengine/fixture"
	"mygo-music/internal/audioengine/pcm"
	"mygo-music/internal/music"
	"mygo-music/internal/player"
)

// Closing to the tray and relaunching the app preserve the device and release it on quit.
func TestDesktopRelaunchDuringPlayback(t *testing.T) {
	root := os.Getenv("PRISM_DESKTOP_RUNTIME")
	endpoint := os.Getenv("PRISM_NATIVE_ENDPOINT")
	if root == "" || endpoint == "" {
		t.Skip("set PRISM_DESKTOP_RUNTIME to a short local directory and PRISM_NATIVE_ENDPOINT to a test endpoint")
	}
	if os.Getenv("PRISM_DESKTOP_SECOND_INSTANCE") == "1" {
		var opened bool
		shotHook = func(*app) {
			opened = true
			go mygo.App.Quit()
		}
		onMainThread(func() {
			if err := Run(); err != nil {
				t.Error(err)
			}
		})
		if opened {
			t.Fatal("second instance created a window")
		}
		return
	}
	runtimeDir, err := os.MkdirTemp(root, "m4-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(runtimeDir) })
	// MyGo's socket needs a short path. Use a private directory for both
	// processes, including when this is an unpackaged test executable.
	if !filepath.IsAbs(runtimeDir) || len(runtimeDir) > 68 {
		t.Fatal("desktop runtime directory must be absolute and no more than 68 characters long")
	}
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	file := filepath.Join(t.TempDir(), "silent.wav")
	f := pcm.Format{Kind: pcm.Integer, Rate: 48000, Channels: 2, ChannelMask: 3, ValidBits: 16, ContainerBits: 16}
	if err := fixture.WriteWAV(file, f, make([]int32, 30*f.Rate*f.Channels)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	result := make(chan error, 1)
	var primary *app
	shotHook = func(a *app) {
		primary = a
		go func() {
			defer mygo.App.Quit()
			result <- checkDesktopPlayback(ctx, a, endpoint, file)
		}()
	}
	t.Cleanup(func() { shotHook = nil })
	onMainThread(func() { err = Run() })
	if err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatal("desktop playback check did not finish")
	}
	if !primary.pl.Snapshot().Closed {
		t.Fatal("application quit before closing playback")
	}
	if _, showing := mygo.NowPlaying.Info(); showing {
		t.Fatal("media session survived application quit")
	}
}

func checkDesktopPlayback(ctx context.Context, a *app, endpoint, file string) error {
	var err error
	mygo.RunOnMain(func() {
		if a.tray == nil {
			err = errors.New("Windows tray was not created")
			return
		}
		source := newFakeSource("local")
		source.stream = func(string, string) (*music.Stream, error) { return &music.Stream{URL: music.FileURL(file)}, nil }
		a.pl.Source = func(string) music.Source { return source }
		a.pl.SetVolume(0)
		err = a.pl.SetOutput(player.OutputPreferences{Mode: player.OutputShared, EndpointID: endpoint})
		a.pl.PlayQueue([]music.Track{{ID: "fixture", Source: "local", Name: "Silent desktop fixture"}}, 0)
	})
	if err != nil {
		return err
	}
	wait := func(expected string, ready func() bool) error {
		tick := time.NewTicker(15 * time.Millisecond)
		defer tick.Stop()
		for !ready() {
			select {
			case <-ctx.Done():
				return fmt.Errorf("waiting for %s: %w", expected, ctx.Err())
			case <-tick.C:
			}
		}
		return nil
	}
	if err := wait("playing audio, media session and presented PCM measurement", func() bool {
		info, active := mygo.NowPlaying.Info()
		return active && info.State == mygo.PlaybackPlaying && a.pl.OutputStatus().Native.Measurement.Valid
	}); err != nil {
		return err
	}
	start := a.pl.Snapshot()
	mygo.RunOnMain(a.win.Close)
	if err := wait("continued audio with the main window hidden", func() bool {
		return !a.win.IsVisible() && !a.pl.OutputStatus().Native.Measurement.Valid &&
			a.pl.Position() > start.Position+200*time.Millisecond
	}); err != nil {
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	second := exec.CommandContext(ctx, executable, "-test.run=^TestDesktopRelaunchDuringPlayback$")
	second.Env = append(os.Environ(), "PRISM_DESKTOP_SECOND_INSTANCE=1")
	if data, err := second.CombinedOutput(); err != nil {
		return fmt.Errorf("second launch: %w (%s)", err, data)
	}
	if err := wait("a new measurement after the second launch restores the window", func() bool {
		return a.win.IsVisible() && a.pl.OutputStatus().Native.Measurement.Valid
	}); err != nil {
		return err
	}
	for i, display := range mygo.Screen.Displays() {
		area := display.WorkArea
		width, height := min(area.Width, 960), min(area.Height, 620)
		beforeMove := a.pl.Position()
		a.win.SetBounds(mygo.Rectangle{X: area.X + (area.Width-width)/2, Y: area.Y + (area.Height-height)/2,
			Width: width, Height: height})
		if err := wait("continued playback after changing display and window size", func() bool {
			return mygo.Screen.DisplayMatching(a.win.Bounds()).ID == display.ID &&
				a.pl.Position() > beforeMove+100*time.Millisecond
		}); err != nil {
			return err
		}
		fmt.Printf("display %d, scale %.2f, continued audio after move and resize\n", i+1, display.ScaleFactor)
	}
	got := a.pl.Snapshot()
	if !got.Playing || got.SessionGeneration != start.SessionGeneration ||
		got.AudioOutput.Native.OutputStats.Epoch != start.AudioOutput.Native.OutputStats.Epoch ||
		got.AudioOutput.Native.Underruns != 0 || got.Position < start.Position {
		return errors.New("window lifecycle changed the playback session or lost frames")
	}
	fmt.Printf("desktop lifecycle preserved epoch %d through hide and relaunch (%s → %s)\n",
		got.AudioOutput.Native.OutputStats.Epoch, start.Position, got.Position)
	return nil
}
