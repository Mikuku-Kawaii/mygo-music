package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/egoist/mygo/ui"
	"mygo-music/internal/music"
	"mygo-music/internal/player"
)

// Text import edits the preview only. Applying writes the validated
// configuration and a later malformed import retains that preset.
func TestDSPSettingsEditing(t *testing.T) {
	if !player.NativeOutputAvailable() {
		t.Skip("Windows x64")
	}
	a := newTestApp()
	a.settings.path = filepath.Join(t.TempDir(), "settings.json")
	a.setAudioOutput(player.OutputPreferences{Mode: player.OutputProcessed})
	v := ui.NewTester(func(c *ui.Context) {
		a.dspSettings(c, paletteFor(c.Theme()))
	}, 560, 1130)
	settled(v)
	click := func(label string) {
		t.Helper()
		if err := v.Click(label); err != nil {
			t.Fatal(err)
		}
		settled(v)
	}
	field, ok := v.AccessByRole(ui.RoleTextField, "AutoEQ Text")
	if !ok {
		t.Fatal("AutoEQ text entry is unavailable")
	}
	v.ClickAt(field.Bounds.X+field.Bounds.W/2, field.Bounds.Y+field.Bounds.H/2)
	v.Type("Preamp: -5 dB\nFilter 1: ON LSC Fc 100 Hz Gain 2 dB Q 0.7")
	settled(v)
	click("Import AutoEQ")
	if a.audioSettings.dsp.draft.PreampDB != -5 || a.pl.AudioOutput().Requested.DSP.PreampDB != 0 {
		t.Fatalf("import preview %g, active %g, error %q", a.audioSettings.dsp.draft.PreampDB, a.pl.AudioOutput().Requested.DSP.PreampDB, a.audioSettings.dsp.err)
	}
	click("Apply DSP")
	b, err := os.ReadFile(a.settings.path)
	if err != nil {
		t.Fatal(err)
	}
	var saved settings
	if err := json.Unmarshal(b, &saved); err != nil || saved.AudioOutput.DSP.PreampDB != -5 ||
		saved.AudioOutput.DSP.Filters[0].Hz != 100 || a.pl.AudioOutput().Requested != saved.AudioOutput {
		t.Fatal(saved.AudioOutput, err)
	}
	a.audioSettings.dsp.importText = "Preamp: NaN dB"
	v.Frame()
	click("Import AutoEQ")
	if a.audioSettings.dsp.err == "" || a.audioSettings.dsp.draft != saved.AudioOutput.DSP {
		t.Fatal("malformed import replaced the draft")
	}
	if os.Getenv("MM_FRAMES") != "" {
		a.audioSettings.dsp.err = ""
		a.audioSettings.dsp.importText = "Preamp: -5 dB\nFilter 1: ON LSC Fc 100 Hz Gain 2 dB Q 0.7"
		v.Frame()
		settled(v)
		saveFrame(t, framesDir(t), "dsp-settings.png", v.Image())
	}
}

// Changing modes abandons an unconfirmed DSP request while retaining the saved preference and draft.
func TestOutputSwitchSupersedesPendingDSP(t *testing.T) {
	if !player.NativeOutputAvailable() {
		t.Skip("Windows x64")
	}
	a := newTestApp()
	a.settings.path = filepath.Join(t.TempDir(), "settings.json")
	a.setAudioOutput(player.OutputPreferences{Mode: player.OutputProcessed})
	// With no selected track, this is saved as the next stream's preference.
	editor := &a.audioSettings.dsp
	editor.draft.PreampDB = -3
	a.applyDSP()
	saved := a.settings.AudioOutput.DSP
	a.pl.Source = func(string) music.Source { return fake(a) }
	local := song("1", "song", "artist")
	local.Source = "local"
	a.pl.Cue([]music.Track{local})
	editor.draft.PreampDB = -6
	a.applyDSP()
	if !editor.applying || a.settings.AudioOutput.DSP != saved {
		t.Fatal("pending DSP edit was saved before the device confirmed it")
	}
	o := a.pl.AudioOutput().Requested
	o.Mode = player.OutputShared
	a.setAudioOutput(o)
	if editor.applying || a.pl.AudioOutput().Requested.DSP != saved || a.settings.AudioOutput.DSP != saved ||
		editor.draft.PreampDB != -6 {
		t.Fatal("switching mode lost the distinction between saved settings and draft")
	}
}
