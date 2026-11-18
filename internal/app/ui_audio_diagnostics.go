package app

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"

	"mygo-music/internal/audioengine/diagnostics"
	"mygo-music/internal/player"
)

func dbLevel(value float64) string {
	if value <= 0 {
		return "−∞"
	}
	return fmt.Sprintf("%.1f", 20*math.Log10(value))
}

func (a *app) audioDiagnostics(c *ui.Context, p palette) {
	s := a.pl.OutputStatus()
	n := s.Native
	ui.Column(c.Key("output-diagnostics")).Padding(12, 16).Gap(8).Children(func() {
		ui.Text(c, T("Playback Diagnostics")).FontSize(14).FontWeight(600)
		if o := n.OutputStats; o.Epoch != 0 {
			ui.Text(c, Tf("Device period %.1f ms · ring %d / %d frames · lowest %d",
				n.Output.Period.Seconds()*1000, o.RingBuffered, o.RingCapacity, o.RingLowWater)).FontSize(12).TextColor(p.muted)
			ui.Text(c, Tf("%d underruns · %d missing frames · longest packet service %.1f ms",
				o.Underruns, o.UnderrunFrames, o.MaxService.Seconds()*1000)).FontSize(12).TextColor(p.muted)
			if n.ParameterDelay > 0 {
				text := Tf("Last gain confirmation %.1f ms", n.ParameterDelay.Seconds()*1000)
				if n.Processing {
					text = Tf("Last DSP update queued in %.1f ms", n.ParameterDelay.Seconds()*1000)
				}
				ui.Text(c, text).FontSize(12).TextColor(p.muted)
			}
		}
		ui.Text(c, T("Export a JSON report of playback and runtime measurements.")).FontSize(12).TextColor(p.muted).LineHeight(1.4)
		if ui.Button(c.Key("export-diagnostics"), T("Export Diagnostics…")).Disabled(a.audioSettings.exporting).Clicked() {
			endpoint := s.Requested.EndpointID
			if s.Requested.Mode == player.OutputASIO {
				endpoint = s.Requested.ASIODriver
			}
			a.exportAudioDiagnostics(diagnostics.Playback{
				Mode: string(s.Requested.Mode), Endpoint: endpoint, Remote: s.Remote,
				Ready: s.Ready, Loading: s.Loading, Playing: s.Playing, Failed: s.Error != "", Native: s.Native,
			})
		}
		if msg := a.audioSettings.exportResult; msg != "" {
			ui.Text(c, msg).FontSize(12).TextColor(p.muted).LineHeight(1.4)
		}
	})
}

var pickAudioDiagnostics = func(parent *mygo.Window, name string) (string, error) {
	dir, _ := mygo.App.Path(mygo.PathDownloads)
	return mygo.Dialog.Save(mygo.SaveDialogOptions{
		Parent: parent, Title: T("Export Diagnostics…"), DefaultPath: filepath.Join(dir, name),
		CreateDirectories: true,
	})
}

func (a *app) exportAudioDiagnostics(playback diagnostics.Playback) {
	if a.audioSettings.exporting {
		return
	}
	a.audioSettings.exporting, a.audioSettings.exportResult = true, ""
	parent, pick := a.settingsWin, pickAudioDiagnostics
	go func() {
		report := diagnostics.Capture(playback)
		path, err := pick(parent, "mygo-audio-"+report.CapturedUTC.Format("20060102-150405")+".json")
		if err == nil && path != "" {
			var data []byte
			data, err = json.MarshalIndent(report, "", "  ")
			if err == nil {
				err = writeDiagnosticFile(path, data)
			}
		}
		a.post(func() {
			message := ""
			switch {
			case err != nil:
				message = Tf("Diagnostics export failed (%s)", err.Error())
			case path != "":
				message = T("Diagnostics exported")
			}
			a.audioSettings.exporting, a.audioSettings.exportResult = false, message
		})
	}()
}

func writeDiagnosticFile(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".mygo-audio-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, err = f.Write(data)
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), path)
}
