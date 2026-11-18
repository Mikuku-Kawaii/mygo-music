package app

import (
	"context"
	"encoding/json"
	"image/color"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"
	"mygo-music/internal/audioengine/diagnostics"
	"mygo-music/internal/audioengine/fixture"
	"mygo-music/internal/audioengine/output"
	"mygo-music/internal/audioengine/pcm"
	"mygo-music/internal/i18n"
	"mygo-music/internal/music"
	"mygo-music/internal/player"
)

// Output mode/device selections reach the controller, preserve ordinary gain
// preferences, pause playback, and persist IDs rather than device labels.
func TestAudioOutputSelections(t *testing.T) {
	if !player.NativeOutputAvailable() {
		t.Skip("Windows x64")
	}
	a := newTestApp()
	a.settings.path = filepath.Join(t.TempDir(), "settings.json")
	a.settingsPage = "audio"
	a.audioSettings.devices = []output.Device{{ID: "first", Name: "USB DAC"}, {ID: "second", Name: "USB DAC"}}
	a.pl.Cue([]music.Track{song("1", "song", "artist")})
	a.pl.Hold(12 * time.Second)
	a.volume = .23
	a.settings.Normalize = true
	tt := ui.NewTester(a.settingsView, 780, 1000)
	settled(tt)
	choose := func(shown, next string) {
		t.Helper()
		if err := tt.Click(shown); err != nil {
			t.Fatal(err)
		}
		tt.Frame()
		if err := tt.Click(next); err != nil {
			t.Fatal(err)
		}
		settled(tt)
	}
	choose("MyGo Compatible", "WASAPI Exclusive")
	if a.pl.Playing() || a.pl.CanSeek() || a.pl.Position() != 0 || a.volume != .23 || !a.settings.Normalize {
		t.Fatal(a.pl.Snapshot())
	}
	choose("System Default", "USB DAC · 2")
	choose("Match Source Bits", "32-bit Container")
	o := a.pl.AudioOutput().Requested
	if o.EndpointID != "second" || o.ContainerBits != 32 || o.Version != 1 {
		t.Fatal(o)
	}
	b, err := os.ReadFile(a.settings.path)
	if err != nil {
		t.Fatal(err)
	}
	var saved settings
	if err = json.Unmarshal(b, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.AudioOutput != o || !saved.Normalize {
		t.Fatal(saved.AudioOutput)
	}
	choose("WASAPI Exclusive", "WASAPI Shared")
	choose("Standard Buffer", "High-load Buffer")
	if a.pl.Playing() || !a.pl.CanAdjustVolume() || a.pl.AudioOutput().Requested.Mode != player.OutputShared || a.volume != .23 {
		t.Fatal("shared selection lost pause or volume")
	}
	choose("WASAPI Shared", "MyGo Compatible")
	if a.pl.Playing() || !a.pl.CanAdjustVolume() || a.volume != .23 || !a.settings.AudioOutput.StableBuffer {
		t.Fatal("return to compatible lost pause or gain preference")
	}
}

// Choosing ASIO or DoP saves its requested device without reporting it as opened.
func TestAudioAdvancedOutputSelections(t *testing.T) {
	if !player.NativeOutputAvailable() {
		t.Skip("Windows x64")
	}
	a := newTestApp()
	a.settings.path = filepath.Join(t.TempDir(), "settings.json")
	a.settingsPage = "audio"
	a.audioSettings.drivers = []output.ASIODriver{{ID: "asio-id", Name: "Test ASIO"}}
	a.audioSettings.devices = []output.Device{{ID: "dop-id", Name: "Test DAC"}}
	tt := ui.NewTester(a.settingsView, 780, 1000)
	settled(tt)
	choose := func(shown, next string) {
		t.Helper()
		if err := tt.Click(shown); err != nil {
			t.Fatal(err)
		}
		tt.Frame()
		if err := tt.Click(next); err != nil {
			t.Fatal(err)
		}
		settled(tt)
	}
	choose("MyGo Compatible", "ASIO PCM")
	choose("Select an ASIO driver first", "Test ASIO · 1")
	choose("ASIO PCM", "WASAPI DoP")
	choose("Select an output device", "Test DAC · 1")
	o := a.pl.AudioOutput()
	if o.Requested.Mode != player.OutputDoP || o.Requested.EndpointID != "dop-id" || o.Requested.ASIODriver != "asio-id" ||
		o.Native.Output.Format.Rate != 0 || a.pl.CanAdjustVolume() || a.pl.Playing() || a.settings.AudioOutput != o.Requested {
		t.Fatal(o)
	}
}

// A completed local file measurement updates the native settings without issuing a playback command.
func TestAudioLoudnessMeasurement(t *testing.T) {
	a := newTestApp()
	path := filepath.Join(t.TempDir(), "silent.wav")
	f := pcm.Format{Kind: pcm.Integer, Rate: 48000, Channels: 2, ChannelMask: 3, ValidBits: 16, ContainerBits: 16}
	if err := fixture.WriteWAV(path, f, make([]int32, 48000)); err != nil {
		t.Fatal(err)
	}
	old := pickLoudnessFile
	defer func() { pickLoudnessFile = old }()
	pickLoudnessFile = func(*mygo.Window) ([]string, error) { return []string{path}, nil }
	a.settingsPage, a.settingsQuery = "audio", "Loudness and True Peak"
	posted := posts(a)
	before := a.pl.Snapshot()
	tt := ui.NewTester(a.settingsView, 640, 420)
	tt.SetScale(2)
	settled(tt)
	if err := tt.Click("Measure Audio File…"); err != nil {
		t.Fatal(err)
	}
	until(t, posted, func() bool { return a.audioSettings.loudness.cancel == nil })
	s := a.audioSettings.loudness
	if s.file != "silent.wav" || s.err != "" || s.result.Frames != 24000 || s.result.IntegratedLUFS != nil || s.result.TruePeakDBTP != nil {
		t.Fatal(s)
	}
	after := a.pl.Snapshot()
	if before.EventSeq != after.EventSeq || before.SessionGeneration != after.SessionGeneration {
		t.Fatal("loudness measurement changed playback")
	}
}

// Native progress is disabled until its decoder is ready; returning to
// compatible mode restores seeking without changing the volume preference.
func TestAudioOutputProgressAndVolume(t *testing.T) {
	if !player.NativeOutputAvailable() {
		t.Skip("Windows x64")
	}
	a := newTestApp()
	a.pl.Cue([]music.Track{song("1", "song", "artist")})
	a.setAudioOutput(player.OutputPreferences{Mode: player.OutputExclusive})
	a.volume = .3
	tt := ui.NewTester(func(c *ui.Context) {
		ui.Column(c).Fill().Children(func() {
			a.scrubber(c, &a.barScrub, ui.RGB(30, 30, 30), ui.RGB(200, 200, 200), 20, ui.Color{}).Width(400)
			a.volumeSlider(c, ui.RGB(200, 200, 200), ui.RGB(30, 30, 30), 180)
		})
	}, 500, 200)
	settled(tt)
	n, ok := tt.AccessByRole(ui.RoleSlider, "Progress")
	if !ok || !n.Disabled {
		t.Fatal(n)
	}
	tt.ClickAt(n.Bounds.X+n.Bounds.W/2, n.Bounds.Y+n.Bounds.H/2)
	tt.Key(0, ui.KeyRight)
	if a.pl.Position() != 0 || a.volume != .3 || a.barScrub.active {
		t.Fatal("unsupported control changed playback")
	}
	a.setAudioOutput(player.OutputPreferences{})
	settled(tt)
	n, ok = tt.AccessByRole(ui.RoleSlider, "Progress")
	if !ok || n.Disabled {
		t.Fatal(n)
	}
	tt.ClickAt(n.Bounds.X+n.Bounds.W/2, n.Bounds.Y+n.Bounds.H/2)
	tt.Frame()
	if a.pl.Position() <= 0 {
		t.Fatal("compatible seek did not return")
	}
}

// Rebuilds and a second click cannot start another device enumeration; an
// answer only touches app state when its main-thread post is applied.
func TestAudioOutputRefresh(t *testing.T) {
	if !player.NativeOutputAvailable() {
		t.Skip("Windows x64")
	}
	a := newTestApp()
	post := posts(a)
	old := enumerateAudioDevices
	started, release := make(chan struct{}, 2), make(chan struct{})
	enumerateAudioDevices = func(context.Context) ([]output.Device, error) {
		started <- struct{}{}
		<-release
		return []output.Device{{ID: "fixed", Name: "DAC"}}, nil
	}
	defer func() { enumerateAudioDevices = old }()
	a.settingsPage = "audio"
	a.setAudioOutput(player.OutputPreferences{Mode: player.OutputExclusive})
	tt := ui.NewTester(a.settingsView, 780, 1000)
	settled(tt)
	if err := tt.Click("Refresh"); err != nil {
		t.Fatal(err)
	}
	<-started
	a.refreshAudioDevices()
	settled(tt)
	settled(tt)
	if len(started) != 0 || len(a.audioSettings.devices) != 0 {
		t.Fatal("render launched I/O or worker wrote UI state")
	}
	close(release)
	runPost(t, post)
	if a.audioSettings.busy || len(a.audioSettings.devices) != 1 || a.audioSettings.devices[0].ID != "fixed" {
		t.Fatal(a.audioSettings)
	}
}

// The audio settings thumb stays below the title bar and still drags the page.
func TestAudioOutputScrollbarBelowTitle(t *testing.T) {
	if !player.NativeOutputAvailable() {
		t.Skip("Windows x64")
	}
	a := newTestApp()
	a.settingsPage = "audio"
	a.setAudioOutput(player.OutputPreferences{Mode: player.OutputProcessed})
	const width, height = 640, 420
	var titleHeight float32
	tt := ui.NewTester(func(c *ui.Context) {
		titleHeight = topBar(c)
		a.settingsView(c)
	}, width, height)
	tt.SetDark(true)
	tt.Move(width-6, 100)
	settled(tt)

	img := tt.Image()
	scale := float32(img.Bounds().Dx()) / width
	pixel := func(x, y float32) color.Color {
		return img.At(int(x*scale), int(y*scale))
	}
	if pixel(width-6, titleHeight/2) != pixel(width-20, titleHeight/2) {
		t.Fatal("scrollbar is painted in the title bar")
	}
	x, y := float32(width-6), titleHeight+14
	tt.Press(x, y)
	tt.Move(x, y+90)
	tt.Release(x, y+90)
	settled(tt)
	if !a.settingsUnderTop {
		t.Fatal("dragging the inset scrollbar did not scroll the page")
	}
}

// Resizing at another DPI keeps diagnostic export reachable and preserves the current playback session.
func TestAudioDiagnosticsExportAtDifferentDPI(t *testing.T) {
	if !player.NativeOutputAvailable() {
		t.Skip("Windows x64")
	}
	a := newTestApp()
	path := filepath.Join(t.TempDir(), "audio.json")
	if err := os.WriteFile(path, []byte("previous diagnostic"), 0o600); err != nil {
		t.Fatal(err)
	}
	a.setAudioOutput(player.OutputPreferences{Mode: player.OutputProcessed, EndpointID: "saved-dac"})
	a.settingsPage, a.settingsQuery = "audio", "Playback Diagnostics"
	before := a.pl.Snapshot()
	posted := posts(a)
	old := pickAudioDiagnostics
	defer func() { pickAudioDiagnostics = old }()
	pickAudioDiagnostics = func(_ *mygo.Window, _ string) (string, error) { return path, nil }
	tt := ui.NewTester(a.settingsView, 880, 640)
	tt.SetTitleBar(ui.TitleBar{Height: 34, Right: 46})
	tt.SetSize(640, 420)
	for _, scale := range []float32{1, 1.5, 2} {
		tt.SetScale(scale)
		tt.SetDark(scale != 1)
		settled(tt)
		n, ok := tt.AccessByRole(ui.RoleButton, "Export Diagnostics…")
		if !ok || n.Disabled || n.Bounds.Y < 34 || n.Bounds.Y+n.Bounds.H > 420 || n.Bounds.X+n.Bounds.W > 640 {
			t.Fatalf("diagnostic export outside the settings view at %gx: %+v", scale, n)
		}
	}
	if err := tt.Click("Export Diagnostics…"); err != nil {
		t.Fatal(err)
	}
	until(t, posted, func() bool { return !a.audioSettings.exporting })
	if a.audioSettings.exportResult != T("Diagnostics exported") {
		t.Fatal(a.audioSettings.exportResult)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var r diagnostics.Report
	if err := json.Unmarshal(data, &r); err != nil {
		t.Fatal(err)
	}
	if r.Version != 1 || r.RequestedMode != string(player.OutputProcessed) || r.RequestedEndpoint == "" ||
		r.RequestedEndpoint == "saved-dac" || r.Native != nil || r.EffectiveMode != "" || r.Phase != "idle" {
		t.Fatal("diagnostic reported an unopened device as active", r)
	}
	after := a.pl.Snapshot()
	if after.EventSeq != before.EventSeq || after.Playing != before.Playing || after.SessionGeneration != before.SessionGeneration {
		t.Fatal("DPI or exporting changed playback")
	}
}

// With MM_FRAMES set, inspect the real native widgets in both themes and at
// the minimum window size, without inventing confirmed device formats.
func TestAudioOutputFrames(t *testing.T) {
	dir := framesDir(t)
	if !player.NativeOutputAvailable() {
		t.Skip("Windows x64")
	}
	defer i18n.SetLanguage(i18n.English)
	i18n.SetLanguage(i18n.Chinese)
	a := newTestApp()
	a.settingsPage = "audio"
	a.setAudioOutput(player.OutputPreferences{Mode: player.OutputShared})
	for _, v := range []struct {
		name string
		w, h int
		dark bool
	}{{"audio-light", 780, 900, false}, {"audio-dark", 780, 900, true}, {"audio-compact", 640, 420, false}} {
		tt := ui.NewTester(a.settingsView, v.w, v.h)
		tt.SetDark(v.dark)
		settled(tt)
		saveFrame(t, dir, v.name+".png", tt.Image())
	}
	a.settingsQuery = T("Playback Diagnostics")
	diagnostics := ui.NewTester(a.settingsView, 640, 420)
	diagnostics.SetScale(2)
	diagnostics.SetDark(true)
	diagnostics.SetTitleBar(ui.TitleBar{Height: 34, Right: 46})
	settled(diagnostics)
	saveFrame(t, dir, "audio-diagnostics-200.png", diagnostics.Image())
	a.settingsQuery = T("Loudness and True Peak")
	settled(diagnostics)
	saveFrame(t, dir, "audio-loudness-200.png", diagnostics.Image())
	a.settingsQuery = ""
	a.audioSettings.drivers = []output.ASIODriver{{ID: "realtek", Name: "Realtek ASIO"}}
	a.setAudioOutput(player.OutputPreferences{Mode: player.OutputASIO, ASIODriver: "realtek"})
	asio := ui.NewTester(a.settingsView, 640, 520)
	asio.SetScale(1.5)
	asio.SetDark(true)
	asio.SetTitleBar(ui.TitleBar{Height: 34, Right: 46})
	settled(asio)
	saveFrame(t, dir, "audio-asio-150.png", asio.Image())
	a.settingsPage = "general"
	a.pl.Cue(framesTracks(a))
	tt := ui.NewTester(a.view, 880, 580)
	settled(tt)
	saveFrame(t, dir, "player-compact.png", tt.Image())
}

// The extra output entry stays clickable at the minimum main-window width;
// its first layout had pushed it beyond the right edge of the window.
func TestAudioOutputEntryAtMinimumWidth(t *testing.T) {
	a := newTestApp()
	a.audioSettings.busy = true // no device enumeration needed for this input check
	a.pl.Cue(framesTracks(a))
	tt := ui.NewTester(a.view, 880, 580)
	settled(tt)
	n, ok := tt.AccessByRole(ui.RoleButton, "Audio Output")
	if !ok || n.Bounds.X+n.Bounds.W > 880 {
		t.Fatalf("output entry outside window: %+v", n)
	}
	tt.ClickAt(n.Bounds.X+n.Bounds.W/2, n.Bounds.Y+n.Bounds.H/2)
	if a.settingsPage != "audio" {
		t.Fatal("output entry did not open settings")
	}
}
