package app

import (
	"math"
	"strconv"
	"strings"

	"github.com/egoist/mygo/ui"
	engine "mygo-music/internal/audioengine/controller"
	"mygo-music/internal/audioengine/dsp"
	"mygo-music/internal/player"
)

type dspPreset struct {
	Version int
	Name    string
	Config  dsp.Config
}

type dspEditor struct {
	initial                     bool
	draft                       dsp.Config
	importText, presetName, err string
	applying                    bool
	showGraphic                 bool
	pending, previous           player.OutputPreferences
	responseRate                int
	responseConfig              dsp.Config
	response                    [64]float64
	responseErr                 string
}

func (a *app) dspSettings(c *ui.Context, p palette) {
	e := &a.audioSettings.dsp
	s := a.pl.OutputStatus()
	if !e.initial {
		e.initial = true
		e.draft = s.AudioOutput.Requested.DSP
	}
	ui.Column(c.Key("dsp")).Padding(16).Gap(10).Children(func() {
		ui.Text(c, T("Digital Signal Processing")).FontSize(14).FontWeight(600)
		ui.Text(c, T("Filter, sample-rate and limiter changes prepare the stream paused at the current position.")).FontSize(12).TextColor(p.muted)
		ui.Column(c).Disabled(e.applying).Gap(8).Children(func() {
			dspSwitch(c, "dsp-bypass", T("Bypass filters and dynamics"), &e.draft.Bypass)
			dspNumber(c, "preamp", T("Preamp (dB)"), &e.draft.PreampDB, -30, 18, .1)
			dspChoice(c, "dsp-rate", T("Output Sample Rate"), []string{T("Source Rate"), "44.1 kHz", "48 kHz", "88.2 kHz", "96 kHz", "176.4 kHz", "192 kHz"},
				[]int{0, 44100, 48000, 88200, 96000, 176400, 192000}, &e.draft.Rate)
			dspChoice(c, "dsp-replaygain", T("ReplayGain"), []string{T("Off"), T("Track"), T("Album")}, []string{"", "track", "album"}, &e.draft.ReplayGain)
			if e.draft.ReplayGain == "album" {
				dspSwitch(c, "dsp-album-fallback", T("Use track gain when album gain is missing"), &e.draft.AlbumFallback)
			}
			dspSwitch(c, "dsp-limiter", T("Linked sample-peak limiter (−1 dBFS)"), &e.draft.Limit)
			dspSwitch(c, "dsp-dither", T("TPDF dither on final quantization"), &e.draft.Dither)
			e.drawResponse(c, p, s.AudioOutput.Native.Source.Format.Rate)
			label := T("Show Advanced DSP")
			if e.showGraphic {
				label = T("Hide Advanced DSP")
			}
			if ui.Button(c.Key("graphic-toggle"), label).Clicked() {
				e.showGraphic = !e.showGraphic
			}
			if e.showGraphic {
				dspSwitch(c, "dsp-swap", T("Swap Left and Right"), &e.draft.SwapChannels)
				dspNumber(c, "dsp-balance", T("Balance (−1 L to +1 R)"), &e.draft.Balance, -1, 1, .05)
				dspNumber(c, "dsp-crossfeed", T("Crossfeed (0–0.5)"), &e.draft.Crossfeed, 0, .5, .05)
				ui.Text(c, T("Graphic Equalizer")).FontSize(12).FontWeight(600)
				for i, hz := range dsp.GraphicFrequencies() {
					dspNumber(c, "graphic-"+strconv.Itoa(i), Tf("%g Hz (dB)", hz), &e.draft.Graphic[i], -12, 12, .5)
				}
			}
			a.dspFilters(c, e)
			ui.Text(c, T("AutoEQ Text")).FontSize(12).FontWeight(600)
			ui.TextArea(c.Key("autoeq-text"), &e.importText).Label(T("AutoEQ Text")).Height(92)
			if ui.Button(c.Key("autoeq-import"), T("Import AutoEQ")).Clicked() {
				draft, err := dsp.ParseAutoEQ(e.importText, e.draft)
				e.err = ""
				if err != nil {
					e.err = err.Error()
				} else {
					e.draft = draft
				}
			}
			a.dspPresets(c, e)
			ui.Row(c).Gap(8).Wrap().Children(func() {
				if ui.Button(c.Key("dsp-reset"), T("Discard Changes")).Disabled(e.draft == s.AudioOutput.Requested.DSP).Clicked() {
					e.draft, e.err = s.AudioOutput.Requested.DSP, ""
				}
				if ui.PrimaryButton(c.Key("dsp-apply"), T("Apply DSP")).Disabled(e.draft == s.AudioOutput.Requested.DSP).Clicked() {
					a.applyDSP()
				}
			})
		})
		status := T("Preview")
		switch {
		case e.applying:
			status = T("Applying DSP…")
		case e.draft == s.AudioOutput.Native.DSP && s.Ready && !s.Loading && s.Error == "" &&
			s.AudioOutput.Native.Processing && s.AudioOutput.Native.State != engine.Ended:
			status = T("DSP Applied")
		case e.draft == a.settings.AudioOutput.DSP:
			status = T("DSP Saved")
		}
		ui.Text(c, status).FontSize(12).TextColor(p.accent)
		if e.err != "" {
			ui.Text(c, e.err).FontSize(12).TextColor(p.muted).LineHeight(1.4)
		}
	})
}

func dspNumber(c *ui.Context, key, label string, value *float64, lo, hi, step float64) {
	ui.Row(c.Key(key)).Gap(8).AlignItems(ui.Center).Children(func() {
		ui.Text(c, label).FontSize(12).Grow(1).Shrink(1).MinWidth(0)
		ui.NumberInput(c, value, lo, hi, step).Width(108).Label(label)
	})
}

func dspSwitch(c *ui.Context, key, label string, value *bool) {
	ui.Row(c.Key(key)).Gap(8).AlignItems(ui.Center).Children(func() {
		ui.Text(c, label).FontSize(12).Grow(1).Shrink(1).MinWidth(0)
		ui.Switch(c, value).Label(label)
	})
}

func dspChoice[T comparable](c *ui.Context, key, name string, labels []string, values []T, value *T) {
	at := 0
	for i, v := range values {
		if *value == v {
			at = i
			break
		}
	}
	ui.Row(c.Key(key)).Gap(8).AlignItems(ui.Center).Children(func() {
		ui.Text(c, name).FontSize(12).Grow(1).Shrink(1).MinWidth(0)
		if i, changed := choice(c, key, name, labels, at); changed {
			*value = values[i]
		}
	})
}

func (a *app) dspFilters(c *ui.Context, e *dspEditor) {
	free := -1
	for i := range e.draft.Filters {
		f := &e.draft.Filters[i]
		if *f == (dsp.Filter{}) {
			if free < 0 {
				free = i
			}
			continue
		}
		ui.Column(c.Key("peq-" + strconv.Itoa(i))).Padding(8).Gap(6).Children(func() {
			ui.Row(c).Wrap().Gap(8).AlignItems(ui.Center).Children(func() {
				label := Tf("Filter %d", i+1)
				ui.Text(c, label).FontSize(12).FontWeight(600)
				ui.Switch(c.Key("enabled"), &f.Enabled).Label(label)
				ui.Box(c).Grow(1)
				if ui.Button(c.Key("remove"), T("Remove")).Label(Tf("Remove filter %d", i+1)).Clicked() {
					*f = dsp.Filter{}
				}
			})
			dspChoice(c, "filter-type", T("Filter Type"), []string{T("Peak"), T("Low Shelf"), T("High Shelf")},
				[]dsp.FilterKind{dsp.Peak, dsp.LowShelf, dsp.HighShelf}, &f.Kind)
			dspNumber(c, "frequency", T("Frequency (Hz)"), &f.Hz, 10, 96000, 1)
			dspNumber(c, "gain", T("Gain (dB)"), &f.DB, -24, 24, .1)
			dspNumber(c, "quality", T("Q"), &f.Q, .1, 20, .01)
		})
	}
	if ui.Button(c.Key("new-filter"), T("Add PEQ Filter")).Disabled(free < 0).Clicked() && free >= 0 {
		e.draft.Filters[free] = dsp.Filter{Enabled: true, Kind: dsp.Peak, Hz: 1000, Q: 1}
	}
}

func (e *dspEditor) drawResponse(c *ui.Context, p palette, rate int) {
	ui.Text(c, T("PEQ Response")).FontSize(12).FontWeight(600)
	if rate == 0 {
		ui.Text(c, T("Open a track to preview the response at its decoded sample rate.")).FontSize(12).TextColor(p.muted)
		return
	}
	low, high := 20.0, min(20000.0, float64(rate)/2)
	if e.responseRate != rate || e.responseConfig != e.draft {
		e.responseRate, e.responseConfig, e.responseErr = rate, e.draft, ""
		for i := range e.response {
			f := low * math.Pow(high/low, float64(i)/float64(len(e.response)-1))
			v, err := e.draft.Response(rate, min(f, high))
			if err != nil {
				e.responseErr = err.Error()
				break
			}
			e.response[i] = v
		}
	}
	if e.responseErr != "" {
		ui.Text(c, e.responseErr).TextColor(p.muted).FontSize(12)
		return
	}
	response := e.response
	span := 12.0
	for _, v := range response {
		span = max(span, math.Ceil(math.Abs(v)/6)*6)
	}
	ui.Box(c.Key("response-curve")).Height(88).Draw(func(draw *ui.Painter, r ui.Rect) {
		mid := r.Y + r.H/2
		draw.Line(r.X, mid, r.X+r.W, mid, 1, p.hairline)
		y := func(v float64) float32 { return mid - float32(v/span)*(r.H/2-3) }
		step := r.W / float32(len(response)-1)
		for i := 1; i < len(response); i++ {
			draw.Line(r.X+float32(i-1)*step, y(response[i-1]), r.X+float32(i)*step, y(response[i]), 1.5, p.accent)
		}
	})
	ui.Text(c, Tf("20 Hz – %g kHz · ±%g dB · source %g kHz", high/1000, span, float64(rate)/1000)).FontSize(11).TextColor(p.muted)
}

func (a *app) dspPresets(c *ui.Context, e *dspEditor) {
	presets := a.settings.DSPPresets
	names := []string{T("Load DSP Preset"), T("Flat")}
	var valid []dspPreset
	for _, p := range presets {
		if p.Version == 1 && p.Config.Validate(0) == nil {
			names = append(names, p.Name)
			valid = append(valid, p)
		}
	}
	if i, changed := choice(c, "dsp-presets", T("Load DSP Preset"), names, 0); changed {
		if i == 1 {
			e.draft = dsp.Config{}
		} else if i > 1 {
			e.draft, e.presetName = valid[i-2].Config, valid[i-2].Name
		}
		e.err = ""
	}
	ui.Row(c).Gap(6).Wrap().Children(func() {
		ui.TextInput(c.Key("preset-name"), &e.presetName).Label(T("Preset Name")).Placeholder(T("Preset Name")).MaxWidth(250).MinWidth(120)
		name := strings.TrimSpace(e.presetName)
		if ui.Button(c.Key("preset-save"), T("Save Preset")).Disabled(name == "").Clicked() {
			if err := e.draft.Validate(0); err != nil {
				e.err = err.Error()
				return
			}
			next := dspPreset{Version: 1, Name: name, Config: e.draft}
			found := false
			for i, p := range a.settings.DSPPresets {
				if p.Name == name {
					a.settings.DSPPresets[i] = next
					found = true
					break
				}
			}
			if !found {
				a.settings.DSPPresets = append(a.settings.DSPPresets, next)
			}
			e.err = ""
			if err := a.settings.save(); err != nil {
				e.err = err.Error()
			}
		}
	})
}

func (a *app) applyDSP() {
	e := &a.audioSettings.dsp
	e.previous = a.pl.AudioOutput().Requested
	next := e.previous
	next.DSP = e.draft
	e.err = ""
	if err := a.pl.SetOutput(next); err != nil {
		e.err = err.Error()
		return
	}
	e.pending, e.applying = a.pl.AudioOutput().Requested, true
	a.finishDSPApply()
}

func (a *app) finishDSPApply() {
	e := &a.audioSettings.dsp
	if !e.applying {
		return
	}
	s := a.pl.OutputStatus()
	if s.AudioOutput.Requested != e.pending {
		e.applying = false
		return
	}
	if s.Error != "" {
		e.err, e.applying = s.Error, false
		if err := a.pl.SetOutput(e.previous); err != nil {
			e.err += "\n" + err.Error()
		}
		return
	}
	finished := s.AudioOutput.Native.State == engine.Ended && !s.Playing
	if !finished && s.HasTrack && (!s.Ready || s.Loading || s.AudioOutput.Native.DSP != e.pending.DSP) {
		return
	}
	e.applying = false
	if err := a.saveAudioPreferences(e.pending); err != nil {
		e.err = err.Error()
	}
}
