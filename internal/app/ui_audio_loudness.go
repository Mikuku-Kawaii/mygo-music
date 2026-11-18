package app

import (
	"context"
	"path/filepath"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"
	"mygo-music/internal/audioengine/analysis"
)

type loudnessState struct {
	run    uint64
	cancel context.CancelFunc
	result analysis.ProgramLoudness
	file   string
	err    string
}

var pickLoudnessFile = func(parent *mygo.Window) ([]string, error) {
	return mygo.Dialog.Open(mygo.OpenDialogOptions{Parent: parent, Title: T("Measure Audio File…")})
}

func (a *app) loudnessSection(c *ui.Context, p palette) {
	s := &a.audioSettings.loudness
	ui.Column(c.Key("loudness")).Padding(12, 16).Gap(8).Children(func() {
		ui.Text(c, T("Loudness and True Peak")).FontSize(14).FontWeight(600)
		ui.Text(c, T("Measure a local file before playback processing.")).FontSize(12).TextColor(p.muted).LineHeight(1.4)
		if s.cancel != nil {
			if ui.Button(c.Key("cancel-loudness"), T("Cancel Measurement")).Clicked() {
				a.cancelLoudness()
			}
			ui.Text(c, T("Measuring…")).FontSize(12).TextColor(p.accent)
		} else if ui.Button(c.Key("measure-loudness"), T("Measure Audio File…")).Clicked() {
			a.measureLoudness()
		}
		if s.err != "" {
			ui.Text(c, s.err).FontSize(12).TextColor(p.muted).LineHeight(1.4)
		}
		if s.file != "" {
			ui.Text(c, s.file).FontSize(13).FontWeight(500)
			r := s.result
			loudness := T("No gated loudness value")
			if r.IntegratedLUFS != nil {
				loudness = Tf("%.2f LUFS integrated", *r.IntegratedLUFS)
			}
			peak := T("Digital silence")
			if r.TruePeakDBTP != nil {
				peak = Tf("%.2f dBTP true peak · %.2f dBFS sample peak", *r.TruePeakDBTP, *r.SamplePeakDBFS)
			}
			ui.Text(c, loudness).FontSize(12).TextColor(p.muted)
			ui.Text(c, peak).FontSize(12).TextColor(p.muted)
		}
	})
}

func (a *app) cancelLoudness() {
	s := &a.audioSettings.loudness
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
		s.run++
	}
}

func (a *app) measureLoudness() {
	s := &a.audioSettings.loudness
	if s.cancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.run++
	s.cancel, s.file, s.err = cancel, "", ""
	run, parent, pick := s.run, a.settingsWin, pickLoudnessFile
	go func() {
		defer cancel()
		paths, err := pick(parent)
		if ctx.Err() != nil {
			return
		}
		var result analysis.ProgramLoudness
		var name string
		if err == nil && len(paths) == 1 {
			result, err = analysis.ScanLoudness(ctx, paths[0])
			name = filepath.Base(paths[0])
		}
		a.post(func() {
			if s.run != run {
				return
			}
			s.cancel = nil
			if err != nil {
				s.err = Tf("Measurement failed (%s)", err.Error())
				return
			}
			s.file, s.result = name, result
		})
	}()
}
