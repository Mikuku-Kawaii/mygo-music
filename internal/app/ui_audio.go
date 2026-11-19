package app

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/egoist/mygo/ui"
	engine "mygo-music/internal/audioengine/controller"
	"mygo-music/internal/audioengine/output"
	"mygo-music/internal/audioengine/pcm"
	"mygo-music/internal/player"
)

type audioSettingsState struct {
	devices      []output.Device
	drivers      []output.ASIODriver
	loaded       bool
	busy         bool
	err          string
	dsp          dspEditor
	exporting    bool
	exportResult string
	loudness     loudnessState
}

var enumerateAudioDevices = func(ctx context.Context) ([]output.Device, error) { return output.Probe(ctx, nil) }

func (a *app) openAudioSettings() {
	a.settingsPage, a.settingsQuery = "audio", ""
	a.refreshAudioDevices()
	a.openSettings()
}

func (a *app) refreshAudioDevices() {
	if a.audioSettings.busy || !player.NativeOutputAvailable() {
		return
	}
	a.audioSettings.busy, a.audioSettings.err = true, ""
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		devices, err := enumerateAudioDevices(ctx)
		drivers, driverErr := output.ListASIO()
		a.post(func() {
			a.audioSettings.busy = false
			if driverErr == nil {
				a.audioSettings.drivers = drivers
			}
			if err != nil {
				a.audioSettings.err = err.Error()
				return
			}
			a.audioSettings.devices = devices
			a.audioSettings.loaded = true
		})
	}()
}

func (a *app) setAudioOutput(o player.OutputPreferences) {
	if a.audioSettings.dsp.applying {
		// Changing the device or mode supersedes an unconfirmed DSP draft.
		o.DSP = a.settings.AudioOutput.DSP
	}
	if err := a.pl.SetOutput(o); err != nil {
		a.audioSettings.err = err.Error()
		return
	}
	a.audioSettings.dsp.applying = false
	a.volumeOpen = false
	a.barScrub, a.npScrub = scrubState{}, scrubState{}
	if err := a.saveAudioPreferences(a.pl.AudioOutput().Requested); err != nil {
		a.audioSettings.err = err.Error()
	} else {
		a.audioSettings.err = ""
	}
	a.redraw()
}

func (a *app) saveAudioPreferences(o player.OutputPreferences) error {
	next := a.settings
	next.AudioOutput = o
	if err := next.save(); err != nil {
		return err
	}
	a.settings.AudioOutput = o
	return nil
}

func (a *app) audioSettingsSection(c *ui.Context, p palette, row settingsRow, block settingsBlock) {
	o := a.pl.AudioOutput()
	exclusive := o.Requested.Mode == player.OutputExclusive
	processed := o.Requested.Mode == player.OutputProcessed
	dop := o.Requested.Mode == player.OutputDoP
	asio := o.Requested.Mode == player.OutputASIO
	native := o.Requested.Mode != player.OutputCompatible
	row(T("Output Mode"), T("Changing output pauses playback and returns to the start of the song. Press Play when ready."), func() {
		modes := []player.OutputMode{player.OutputCompatible, player.OutputExclusive, player.OutputShared, player.OutputProcessed, player.OutputDoP, player.OutputASIO}
		at := 0
		for i, mode := range modes {
			if mode == o.Requested.Mode {
				at = i
				break
			}
		}
		labels := []string{N("MyGo Compatible")}
		if player.NativeOutputAvailable() {
			labels = append(labels, N("WASAPI Exclusive"), N("WASAPI Shared"), N("WASAPI Processed"), N("WASAPI DoP"), N("ASIO PCM"))
		}
		if i, ok := choice(c, "output-mode", T("Output Mode"), labels, at); ok {
			cfg := o.Requested
			cfg.Mode = modes[i]
			a.setAudioOutput(cfg)
		}
	})
	if native {
		if exclusive || dop || asio {
			block(T("Unity gain"), func() {
				ui.Column(c.Key("unity")).Padding(12, 16).Gap(4).Background(p.accent.Alpha(0.06)).Children(func() {
					ui.Text(c, T("Unity gain · adjust volume on your device")).FontSize(13).FontWeight(600).TextColor(p.accent)
					ui.Text(c, T("Software volume, loudness normalization and fades are bypassed. Their compatible-mode settings are kept. Check your amplifier volume before pressing Play.")).FontSize(12).TextColor(p.muted).LineHeight(1.4)
				})
			})
		} else if !processed {
			block(T("WASAPI Shared"), func() {
				ui.Text(c, T("Source-rate PCM is submitted to Windows. System volume, sample-rate conversion and channel mixing may apply. Loudness normalization is not applied.")).Padding(12, 16).FontSize(12).TextColor(p.muted).LineHeight(1.4)
			})
		}
		if asio {
			row(T("ASIO Driver"), T("Choose an installed 64-bit driver. Its first two output channels carry stereo PCM."), func() {
				a.asioDriverChoice(c, o.Requested)
			})
		} else {
			hint := T("Device loss or a change of the default device pauses playback. Retry prepares the last confirmed position; press Play to resume.")
			if dop {
				hint = T("Select a DAC that supports DoP at 176.4 or 352.8 kHz.")
			}
			row(T("Output Device"), hint, func() {
				ids, names := []string{""}, []string{T("System Default")}
				if dop {
					names[0] = T("Select an output device")
				}
				chosen := names[0]
				found := o.Requested.EndpointID == ""
				for i, d := range a.audioSettings.devices {
					name := fmt.Sprintf("%s · %d", d.Name, i+1)
					ids, names = append(ids, d.ID), append(names, name)
					if d.ID == o.Requested.EndpointID {
						chosen, found = name, true
					}
				}
				if !found {
					chosen = T("Saved Device")
					if a.audioSettings.loaded {
						chosen = T("Saved device unavailable")
					}
					ids, names = append(ids, o.Requested.EndpointID), append(names, chosen)
				}
				if ui.Select(c.Key("output-device"), &chosen, names).Width(170).Label(T("Output Device")).Changed() {
					for i, name := range names {
						if chosen == name {
							cfg := o.Requested
							cfg.EndpointID = ids[i]
							a.setAudioOutput(cfg)
							break
						}
					}
				}
			})
		}
		if !processed && !asio {
			row(T("Integer Container"), T("Source precision is preserved. Choose 32-bit packing only if your device requires it; unsupported formats stop with an error."), func() {
				at := 0
				if o.Requested.ContainerBits == 32 {
					at = 1
				}
				if i, ok := choice(c, "output-container", T("Integer Container"), []string{N("Match Source Bits"), N("32-bit Container")}, at); ok {
					cfg := o.Requested
					cfg.ContainerBits = 0
					if i == 1 {
						cfg.ContainerBits = 32
					}
					a.setAudioOutput(cfg)
				}
			})
		}
		if o.Requested.Mode.SupportsStableBuffer() {
			row(T("Playback Buffer"), T("High-load buffering keeps more audio ready. Changing it prepares the current position while paused."), func() {
				at := 0
				if o.Requested.StableBuffer {
					at = 1
				}
				if i, ok := choice(c, "playback-buffer", T("Playback Buffer"), []string{N("Standard Buffer"), N("High-load Buffer")}, at); ok {
					cfg := o.Requested
					cfg.StableBuffer = i == 1
					a.setAudioOutput(cfg)
				}
			})
		}
		formatHint := T("Local stereo WAV and FLAC. Compatible tracks can play through the same device stream.")
		if processed {
			formatHint = T("Local WAV, FLAC, MP3, Vorbis, Ogg Opus, ALAC, supported AAC and stereo DSF/DFF. Output uses 32-bit PCM.")
		}
		if dop {
			formatHint = T("Stereo DSD64 or DSD128 from DSF and uncompressed DFF. Each track opens a new DoP stream.")
		}
		if asio {
			formatHint = T("Local integer stereo WAV and FLAC at the source rate.")
		}
		if !dop && !asio {
			formatHint += " " + T("NetEase playback prefers lossless audio and can use a lower quality.")
		}
		row(T("Available Devices"), formatHint, func() {
			label := T("Refresh")
			if a.audioSettings.busy {
				label = T("Refreshing…")
			}
			if ui.Button(c.Key("refresh-audio"), label).Disabled(a.audioSettings.busy).Clicked() {
				a.refreshAudioDevices()
			}
		})
	}
	if processed {
		block(T("Digital Signal Processing"), func() { a.dspSettings(c, p) })
	}
	block(T("Signal Path"), func() { a.audioSignalPath(c, p) })
	block(T("Playback Diagnostics"), func() { a.audioDiagnostics(c, p) })
	block(T("Loudness and True Peak"), func() { a.loudnessSection(c, p) })
	if a.audioSettings.err != "" {
		block(a.audioSettings.err, func() { ui.Text(c, a.audioSettings.err).Padding(12, 16).FontSize(12).TextColor(p.muted) })
	}
}

func (a *app) asioDriverChoice(c *ui.Context, prefs player.OutputPreferences) {
	ids, labels := []string{""}, []string{T("Select an ASIO driver first")}
	at := 0
	for i, driver := range a.audioSettings.drivers {
		ids = append(ids, driver.ID)
		labels = append(labels, fmt.Sprintf("%s · %d", driver.Name, i+1))
		if prefs.ASIODriver == driver.ID {
			at = i + 1
		}
	}
	if at == 0 && prefs.ASIODriver != "" {
		at = len(ids)
		ids, labels = append(ids, prefs.ASIODriver), append(labels, T("Saved device unavailable"))
	}
	value := labels[at]
	if ui.Select(c.Key("asio-driver"), &value, labels).Width(170).Label(T("ASIO Driver")).Changed() {
		for i, label := range labels {
			if value == label {
				prefs.ASIODriver = ids[i]
				a.setAudioOutput(prefs)
				break
			}
		}
	}
}

func pcmDescription(f pcm.Format) string {
	if f.Rate == 0 {
		return T("Not confirmed")
	}
	if f.Kind == pcm.Float32 || f.Kind == pcm.Float64 {
		return Tf("%g kHz · %d channels · %d-bit float", float64(f.Rate)/1000, f.Channels, f.ContainerBits)
	}
	return Tf("%g kHz · %d channels · %d-bit / %d-bit container", float64(f.Rate)/1000, f.Channels, f.ValidBits, f.ContainerBits)
}

func (a *app) audioSignalPath(c *ui.Context, p palette) {
	s := a.pl.OutputStatus()
	n := s.AudioOutput.Native
	exclusive := s.AudioOutput.Requested.Mode == player.OutputExclusive
	processed := s.AudioOutput.Requested.Mode == player.OutputProcessed
	dop := s.AudioOutput.Requested.Mode == player.OutputDoP
	asio := s.AudioOutput.Requested.Mode == player.OutputASIO
	native := s.AudioOutput.Requested.Mode != player.OutputCompatible
	status := T("Paused")
	switch {
	case s.Error != "":
		status = T("Output Error")
	case s.Loading:
		status = T("Preparing…")
		if n.RequestedGeneration.Seek != n.AppliedGeneration.Seek {
			status = T("Seeking…")
		}
	case !s.HasTrack:
		status = T("No song selected")
	case native && n.EffectivePlaying || !native && s.Playing:
		status = T("Playing")
	case native && n.State == engine.Ended:
		status = T("Playback ended")
	}
	ui.Column(c.Key("signal-path")).Padding(16).Gap(12).Children(func() {
		ui.Row(c).AlignItems(ui.Center).Gap(10).Children(func() {
			ui.Column(c).Grow(1).Shrink(1).MinWidth(0).Gap(4).Children(func() {
				ui.Text(c, T("Signal Path")).FontSize(14).FontWeight(600)
				ui.Text(c, status).FontSize(12).TextColor(p.accent)
			})
			label := T("Play")
			if s.Playing {
				label = T("Pause")
			} else if s.Error != "" {
				label = T("Retry")
			}
			if ui.Button(c.Key("audio-play"), label).Disabled(!s.HasTrack).Clicked() {
				a.pl.TogglePlay()
			}
		})
		if s.HasTrack {
			ui.Text(c, s.TrackName).FontSize(13).FontWeight(500)
		}
		if s.Error != "" {
			ui.Text(c, s.Error).FontSize(12).TextColor(p.muted).LineHeight(1.4)
			if native {
				ui.Text(c, T("Retry prepares the last confirmed position while paused. Press Play again when ready.")).FontSize(12).TextColor(p.muted).LineHeight(1.4)
			}
			if n.ErrorKind == "device-busy" {
				ui.Text(c, T("The device is in use. Stop other audio applications, then retry or choose another device.")).FontSize(12).TextColor(p.muted)
			} else if n.ErrorKind == "format-unsupported" {
				ui.Text(c, T("The device does not support this exact format. Try a different integer container or return to compatible output.")).FontSize(12).TextColor(p.muted)
			}
			label := T("Use Compatible Output")
			mode := player.OutputCompatible
			if dop || s.Scanned.DSDRate != 0 || s.Scanned.Container == "Ogg Opus" {
				label, mode = T("Use WASAPI Processed"), player.OutputProcessed
			}
			if s.AudioOutput.Requested.Mode != mode && (native || mode == player.OutputProcessed) && ui.Button(c.Key("return-compatible"), label).Clicked() {
				cfg := s.AudioOutput.Requested
				cfg.Mode = mode
				a.setAudioOutput(cfg)
			}
		}
		detail := func(key, value string) {
			ui.Column(c).Gap(3).Children(func() {
				ui.Text(c, key).FontSize(11.5).TextColor(p.muted)
				ui.Text(c, value).FontSize(12.5).LineHeight(1.4)
			})
		}
		if native {
			if a := s.Scanned; a.Known {
				switch {
				case a.DSDRate != 0:
					detail(T("Source DSD"), fmt.Sprintf("DSD%d · %s", a.DSDRate/44100, strings.ToUpper(a.Container)))
				case a.ValidBits > 0:
					detail(T("Scanned PCM"), Tf("%g kHz · %d channels · %d source bits", float64(a.Rate)/1000, a.Channels, a.ValidBits))
				}
			}
			decoded := pcmDescription(n.Source.Format)
			if n.Source.Container != "" {
				decoded = strings.ToUpper(n.Source.Container) + " · " + decoded
			}
			if !dop {
				detail(T("Decoded PCM"), decoded)
			}
			converted := !processed && (n.Source.Format.Kind == pcm.Float32 || n.Source.Format.Kind == pcm.Float64)
			switch {
			case dop:
				detail(T("Processing"), T("DSD bits → DoP carrier → WASAPI exclusive"))
			case asio:
				path := T("Native integer PCM → lossless packing → ASIO driver")
				if converted {
					path = T("Decoded audio → source-rate integer PCM → ASIO driver")
				}
				detail(T("Processing"), path)
				detail(T("ASIO Channel Format"), pcmDescription(n.Output.DriverFormat))
			case exclusive:
				path := T("Native integer PCM → lossless packing → WASAPI exclusive. No resampling or software gain.")
				if converted {
					path = T("Decoded audio → source-rate integer PCM → WASAPI exclusive")
				}
				detail(T("Processing"), path)
			case processed:
				path := T("Decoded PCM → float64 DSP → integer PCM → WASAPI exclusive")
				if n.Source.DSDRate != 0 {
					path = T("DSD → low-pass decimation → float64 DSP → WASAPI exclusive")
				}
				detail(T("Processing"), path)
				if n.Output.Format.Rate > 0 {
					detail(T("DSP lookahead"), Tf("%d output frames · %d drain frames", n.LatencyFrames, n.TailFrames))
					if !n.DSP.Bypass {
						filters := 0
						for _, f := range n.DSP.Filters {
							if f.Enabled {
								filters++
							}
						}
						detail(T("Before Resampling"), Tf("%.3f× preamp/ReplayGain · %d PEQ filters", n.TrackGain, filters))
						bands := 0
						for _, gain := range n.DSP.Graphic {
							if gain != 0 {
								bands++
							}
						}
						if bands > 0 {
							detail(T("Graphic EQ"), Tf("%d adjusted bands", bands))
						}
						if n.DSP.SwapChannels || n.DSP.Crossfeed != 0 || n.DSP.Balance != 0 {
							stereo := Tf("Balance %.2f · crossfeed %.2f", n.DSP.Balance, n.DSP.Crossfeed)
							if n.DSP.SwapChannels {
								stereo = T("Channels swapped") + " · " + stereo
							}
							detail(T("Stereo Processing"), stereo)
						}
						var stages []string
						if n.DSP.Limit {
							stages = append(stages, T("Sample-peak limiter"))
						}
						if n.DSP.Dither {
							stages = append(stages, T("TPDF enabled for quantization"))
						}
						if len(stages) > 0 {
							detail(T("Final Processing"), strings.Join(stages, " · "))
						}
					}
					detail(T("Applied Gain"), Tf("%.3f · config revision %d", n.AppliedVolume, n.AppliedGeneration.Config))
				}
			default:
				path := T("Native integer PCM → Windows stream volume → system sample-rate/channel conversion → shared mixer. This is not bit-perfect output.")
				if converted {
					path = T("Decoded audio → source-rate integer PCM → Windows shared mixer")
				}
				detail(T("Processing"), path)
				detail(T("Windows Mix Format"), pcmDescription(n.Output.MixFormat))
				gain := T("Not confirmed")
				if n.Output.Format.Rate > 0 {
					gain = Tf("%.3f · config revision %d", n.AppliedVolume, n.AppliedGeneration.Config)
				}
				detail(T("Applied Gain"), gain)
			}
			label := T("Negotiated Output")
			if dop {
				label = T("Negotiated DoP Carrier")
			}
			detail(label, pcmDescription(n.Output.Format))
			if n.Output.EndpointID != "" {
				name := n.Output.EndpointID
				for _, d := range a.audioSettings.devices {
					if d.ID == name {
						name = d.Name
						break
					}
				}
				for _, d := range a.audioSettings.drivers {
					if d.ID == name {
						name = d.Name
						break
					}
				}
				detail(T("Opened Device"), name)
			}
			if n.Output.Format.Rate > 0 {
				if n.Output.StableBuffer {
					detail(T("Playback Buffer"), Tf("High-load Buffer · %.0f ms device buffer", float64(n.Output.BufferFrames)*1000/float64(n.Output.Format.Rate)))
				}
				position := T("Device clock · exact frame mapping")
				if asio {
					position = T("ASIO sample clock · adjusted for output latency")
				} else if n.Processing && n.Source.Format.Rate != n.Output.Format.Rate && n.Underruns == 0 {
					position = T("Device clock · resampled frame mapping")
				} else if !n.PositionExact {
					position = T("Device clock · timing degraded")
				}
				detail(T("Presentation"), Tf("%s · source frame %d · epoch submitted %d frames · %d underruns", position, n.PositionFrames, n.ContentSubmittedFrames, n.Underruns))
				if n.RequestedGeneration.Seek > 0 {
					detail(T("Seek Location"), Tf("Target %.3f s · confirmed start frame %d · applied seek %d", n.SeekTarget.Seconds(), n.SourceStartFrame, n.AppliedGeneration.Seek))
				}
				if dop {
					detail(T("Submitted PCM Levels"), T("DoP carries DSD bits. PCM measurement is unavailable."))
				} else if n.Measurement.Valid {
					detail(T("Submitted PCM Levels"), Tf("Peak L/R %s / %s dBFS · RMS L/R %s / %s dBFS",
						dbLevel(n.Measurement.Peak[0]), dbLevel(n.Measurement.Peak[1]),
						dbLevel(n.Measurement.RMS[0]), dbLevel(n.Measurement.RMS[1])))
				} else {
					detail(T("Submitted PCM Levels"), T("No presented PCM measurement"))
				}
			}
			if n.Output.Format.Rate == 0 {
				detail(T("Verification"), T("Output has not been opened"))
			} else if dop {
				detail(T("Verification"), T("Windows accepted the carrier. Confirm DSD recognition on the DAC."))
			} else if processed {
				detail(T("Verification"), T("Processed PCM. Device-side processing is unknown."))
			} else if exclusive || asio {
				detail(T("Verification"), T("Device-side processing is unknown. Exclusive mode alone does not prove bit-perfect output; this session has no sample comparison or digital loopback verification."))
			} else {
				detail(T("Verification"), T("Windows mixing and volume are active. Device-side processing and digital loopback have not been verified."))
			}
			if exclusive && !s.Loading && s.Error == "" && n.Source.NativePrecision && n.Source.Trimmed && n.Source.Format.NativeEqual(n.Output.Format) && n.Underruns == 0 && n.PositionExact {
				detail(T("Software Conditions"), T("Native integer precision, source rate, channel layout and unity gain confirmed. Sample comparison and digital loopback remain separate checks."))
			}
		} else if s.AudioOutput.Remote {
			detail(T("Active Output"), T("Playback is controlled by the source on another device. Its PCM format and signal path are not available here."))
		} else {
			detail(T("Active Output"), T("MyGo compatible output · software volume and optional loudness normalization"))
			detail(T("Processing"), T("MyGo float32 mixer · 48 kHz stereo. System mixing may apply; this is not the native PCM path."))
			detail(T("Source Precision"), T("Not confirmed by the compatible decoder. A lossless source badge describes the file, not the output."))
		}
	})
}

func (a *app) audioOutputButton(c *ui.Context, color ui.Color) {
	if iconButton(c.Key("audio-output"), iconPlug, 16, color, T("Audio Output")).Clicked() {
		a.openAudioSettings()
	}
}
