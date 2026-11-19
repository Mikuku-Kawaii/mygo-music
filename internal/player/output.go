package player

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"time"

	engine "mygo-music/internal/audioengine/controller"
	"mygo-music/internal/audioengine/dsp"
	"mygo-music/internal/i18n"
	"mygo-music/internal/music"
)

type OutputMode string

const (
	OutputCompatible OutputMode = ""
	OutputExclusive  OutputMode = "wasapi-exclusive"
	OutputShared     OutputMode = "wasapi-shared"
	OutputProcessed  OutputMode = "wasapi-processed"
	OutputDoP        OutputMode = "wasapi-dop"
	OutputASIO       OutputMode = "asio-pcm"
)

// OutputPreferences is versioned independently of the upstream settings. The
// empty mode keeps old settings compatible; zero container means source bits.
type OutputPreferences struct {
	Version       int        `json:"version"`
	Mode          OutputMode `json:"mode,omitempty"`
	EndpointID    string     `json:"endpointId,omitempty"`
	ASIODriver    string     `json:"asioDriver,omitempty"`
	ContainerBits int        `json:"containerBits,omitempty"`
	StableBuffer  bool       `json:"stableBuffer,omitempty"`
	DSP           dsp.Config `json:"dsp,omitzero"`
}

func (m OutputMode) SupportsStableBuffer() bool {
	switch m {
	case OutputShared, OutputExclusive, OutputProcessed, OutputDoP:
		return true
	default:
		return false
	}
}

func (o OutputPreferences) useStableBuffer() bool {
	return o.StableBuffer && o.Mode.SupportsStableBuffer()
}

func (o OutputPreferences) Validate() error {
	switch o.Mode {
	case OutputCompatible, OutputExclusive, OutputShared, OutputProcessed, OutputDoP, OutputASIO:
	default:
		return errors.New("unsupported audio output configuration")
	}
	if o.Version < 0 || o.Version > 1 {
		return errors.New("unsupported audio output configuration")
	}
	if o.ContainerBits != 0 && o.ContainerBits != 32 {
		return errors.New("unsupported audio output container")
	}
	if o.Mode != OutputCompatible && !NativeOutputAvailable() {
		return errors.New("WASAPI requires Windows x64")
	}
	return o.DSP.Validate(0)
}

func NativeOutputAvailable() bool { return runtime.GOOS == "windows" && runtime.GOARCH == "amd64" }

// AudioOutput is a value snapshot. Native is confirmed by the engine, not by
// selecting a preference. There is only one application queue: the native
// handle receives a single transport item and never chooses the next song.
type AudioOutput struct {
	InterruptedPlaying bool
	Requested          OutputPreferences
	Native             engine.Snapshot
	Remote             bool
}

// OutputStatus is the settings view's read-only slice of playback state.
// It avoids cloning the library queue and artwork on a meter update.
type OutputStatus struct {
	AudioOutput
	HasTrack, Playing, Ready, Loading bool
	TrackName, Error                  string
	Scanned                           music.AudioProperties
}

func (p *Player) OutputStatus() OutputStatus {
	s := p.controller.latest.Load()
	v := OutputStatus{AudioOutput: s.AudioOutput, Playing: s.Playing, Ready: s.Ready, Loading: s.Loading, Error: s.Error}
	if t := s.Current; t != nil {
		v.HasTrack, v.TrackName, v.Scanned = true, t.Name, t.Audio
	}
	if !s.HasLevels || v.Native.Measurement.Token != p.controller.visual.Token() {
		v.Native.Measurement.Valid = false
	}
	return v
}

type nativeEngine interface {
	SetVolume(context.Context, float64) (*engine.Receipt, error)
	SetQueue(context.Context, []engine.Media, int) (*engine.Receipt, error)
	Play(context.Context) (*engine.Receipt, error)
	Pause(context.Context) (*engine.Receipt, error)
	Seek(context.Context, time.Duration) (*engine.Receipt, error)
	Snapshot() engine.Snapshot
	Changes() <-chan struct{}
	Close() error
}

type leasedNative struct {
	nativeEngine
	once    sync.Once
	release func()
	err     error
}

func (h *leasedNative) Close() error {
	h.once.Do(func() { h.err = h.nativeEngine.Close(); h.release() })
	return h.err
}

func acquireOutput(ctx context.Context, gate chan struct{}) (func(), error) {
	if gate == nil {
		return func() {}, ctx.Err()
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-gate:
		return func() { gate <- struct{}{} }, nil
	}
}

func (p *Player) SetOutput(o OutputPreferences) error {
	if err := o.Validate(); err != nil {
		return err
	}
	o.Version = 1
	var configErr error
	p.command(nil, func(s *playbackState) {
		if s.output == o {
			return
		}
		s.syncNativeBoundary()
		sameOutput := s.output.Mode == o.Mode && s.output.EndpointID == o.EndpointID && s.output.ASIODriver == o.ASIODriver && s.output.ContainerBits == o.ContainerBits
		bufferChanged := s.output.useStableBuffer() != o.useStableBuffer()
		if sameOutput && o.Mode == OutputProcessed {
			if configErr = o.DSP.Validate(s.nativeState.Source.Format.Rate); configErr != nil {
				return
			}
			previous := s.output.DSP
			previous.PreampDB = o.DSP.PreampDB
			if !bufferChanged && previous == o.DSP && s.playing && s.nativeState.EffectivePlaying {
				if leased, ok := s.native.(*leasedNative); ok {
					if h, ok := leased.nativeEngine.(*engine.EngineHandle); ok {
						var receipt *engine.Receipt
						receipt, configErr = h.SetPreamp(context.Background(), o.DSP.PreampDB)
						if configErr != nil {
							return
						}
						s.output, s.nativeCommand = o, receipt.ID()
						s.sync()
						return
					}
				}
			}
		}
		from := time.Duration(0)
		if sameOutput {
			from = s.Position()
		}
		s.output = o
		s.dropAhead()
		if s.at >= 0 && s.at < len(s.order) && (!sameOutput || bufferChanged || o.Mode == OutputProcessed) {
			// The current output keeps its confirmed anchor through a buffer or DSP reconfiguration.
			s.loadSkipping(s.at, 0, stay, false, from)
		}
		s.sync()
	})
	return configErr
}

func (p *Player) AudioOutput() AudioOutput { return p.controller.latest.Load().AudioOutput }
func canSeek(s *PlaybackSnapshot) bool {
	return s.AudioOutput.Requested.Mode == OutputCompatible || s.Ready && !s.Loading && s.Error == "" && s.AudioOutput.Native.Source.ExactSeek
}
func (p *Player) CanSeek() bool { return canSeek(p.controller.latest.Load()) }
func (p *Player) CanAdjustVolume() bool {
	mode := p.AudioOutput().Requested.Mode
	return mode == OutputCompatible || mode == OutputShared || mode == OutputProcessed
}

func waitOutput(ctx context.Context, released <-chan struct{}) error {
	if released == nil {
		return ctx.Err()
	}
	select {
	case <-released:
		return ctx.Err()
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *playbackState) loadNative(ctx context.Context, gen int, src music.Source, t music.Track, quality string, pre *neteaseAhead, skipped, dir int) {
	config, released, gate := p.output, p.outputReleased, p.outputGate
	visual := p.visual
	cache := p.Cache
	initialVolume := p.volume * p.volume
	p.nativeNext = &nativeContinuation{}
	p.prepareNativeNext()
	next := &p.nativeNext.slot
	nativeAfter := p.nativeAfter
	open := p.openNative
	if open == nil {
		open = func(c engine.Config) (nativeEngine, error) { return engine.New(c) }
	}
	work := func() {
		var file *neteaseFile
		defer func() { file.release() }()
		if ctx.Err() != nil {
			if pre != nil {
				pre.discard()
			}
			return
		}
		var info *music.Stream
		var err error
		var mediaFailed, compressed bool
		path := ""
		switch {
		case src == nil:
			err = errNoSource
			mediaFailed = t.Source == neteaseSource
		case config.Mode == OutputDoP && t.Source != "local":
			err = errors.New(i18n.T("DoP needs a local stereo DSF or DFF file"))
		case config.Mode == OutputASIO && config.ASIODriver == "":
			err = errors.New(i18n.T("Select an ASIO driver first"))
		case t.Source == neteaseSource && src.Info().Playback == music.PlaybackStream:
			file, info, err = pre.take(ctx, cache, src, t.ID, config.Mode)
			pre = nil
			mediaFailed = err != nil
			if file != nil {
				path = file.Name()
				compressed = info.Type != "flac"
			}
		case src.Info().Playback == music.PlaybackRemote || config.Mode == OutputProcessed && t.Source != "local":
			err = errors.New(i18n.T("Native output requires a local audio file"))
		default:
			info, err = src.Stream(t.ID, quality)
			if err == nil && info != nil {
				path, _ = music.PathOf(info.URL)
			}
		}
		if pre != nil {
			pre.discard()
		}
		if err == nil && path == "" {
			err = errors.New(i18n.T("Native output requires a local audio file"))
		}
		var h nativeEngine
		var release func()
		if err == nil {
			release, err = acquireOutput(ctx, gate)
		}
		if err == nil {
			err = waitOutput(ctx, released)
		}
		if err == nil && time.Until(nativeAfter) > 0 {
			timer := time.NewTimer(time.Until(nativeAfter))
			select {
			case <-ctx.Done():
				err = ctx.Err()
			case <-timer.C:
			}
			timer.Stop()
		}
		if err == nil {
			asio := ""
			if config.Mode == OutputASIO {
				asio = config.ASIODriver
			}
			h, err = open(engine.Config{Shared: config.Mode == OutputShared, DoP: config.Mode == OutputDoP, ASIO: asio, EndpointID: config.EndpointID, ContainerBits: config.ContainerBits, StableBuffer: config.useStableBuffer(),
				Processing: config.Mode == OutputProcessed, DSP: config.DSP, Next: next, InitialVolume: initialVolume, Analysis: visual})
		}
		if h != nil {
			mediaFile, releaseOutput := file, release
			h = &leasedNative{nativeEngine: h, release: func() {
				mediaFile.release()
				releaseOutput()
			}}
			file, release = nil, nil
		}
		if err == nil {
			_, err = h.SetQueue(ctx, []engine.Media{{Item: engine.Item{Source: t.Source, ID: t.ID, Title: t.Name}, Path: path,
				Gains: nativeGains(src, t.ID, info), DecodeCompressed: compressed}}, 0)
		}
		if release != nil {
			release()
		}
		if err != nil && h != nil {
			h.Close()
			h = nil
		}
		if ctx.Err() != nil {
			if h != nil {
				h.Close()
			}
			return
		}
		apply := func() {
			if gen != p.gen {
				if h != nil {
					go h.Close()
				}
				return
			}
			if err != nil {
				if h != nil {
					go h.Close()
				}
				p.loading = false
				if mediaFailed {
					p.loadNetEaseFailed(err, skipped, dir)
				} else {
					p.loadFailed(t.Source, err, p.at, 0, stay)
				}
				return
			}
			p.native, p.info = h, cloneStream(info)
			p.nativeState, p.nativeCommand, p.nativeEnded = engine.Snapshot{}, 0, false
			p.nativeAsked = false
			p.nativeVolume()
			if p.pending > 0 {
				p.seekNative(p.pending)
			}
			if p.playing {
				p.nativeIntent()
			}
			p.sync()
		}
		if !p.deliver(apply) && h != nil {
			h.Close()
		}
	}
	p.runLoad(work)
}

func (p *playbackState) seekNative(position time.Duration) {
	position = max(0, position)
	if p.native == nil {
		p.pending = position
		return
	}
	p.syncNativeBoundary()
	if c := p.nativeNext; c != nil && !c.slot.Clear() {
		p.adoptNativeBoundary(p.native.Snapshot(), false)
		p.loadSkipping(p.at, 0, stay, p.playing, position)
		return
	}
	if c := p.nativeNext; c != nil {
		c.token, c.stream = 0, nil
	}
	r, err := p.native.Seek(context.Background(), position)
	if err != nil {
		p.err = err.Error()
		p.sync()
		return
	}
	p.seeking, p.seekTo, p.pending = true, position, position
	p.loading, p.nativeEnded = true, false
	p.err = ""
	if r != nil {
		p.nativeCommand = r.ID()
	}
	p.sync()
}

func (p *playbackState) nativeIntent() {
	var receipt *engine.Receipt
	var err error
	if p.playing {
		receipt, err = p.native.Play(context.Background())
	} else {
		receipt, err = p.native.Pause(context.Background())
	}
	if err != nil {
		p.err, p.playing = err.Error(), p.nativeAsked
	} else {
		p.err, p.nativeAsked = "", p.playing
		if receipt != nil {
			p.nativeCommand = receipt.ID()
		}
	}
	p.sync()
}

func (p *playbackState) nativeVolume() {
	if p.native == nil || (p.output.Mode != OutputShared && p.output.Mode != OutputProcessed) || p.nativeState.State == engine.Failed {
		return
	}
	r, err := p.native.SetVolume(context.Background(), p.volume*p.volume)
	if err != nil {
		p.err = err.Error()
	} else if r != nil {
		p.nativeCommand = r.ID()
	}
}

// Called only on the controller, awakened directly by the engine (not UI).
func (p *playbackState) observeNative() {
	if p.native == nil {
		return
	}
	s := p.native.Snapshot()
	if s.RequestedCommand < p.nativeCommand {
		return
	}
	previous := p.nativeState
	advanced := p.adoptNativeBoundary(s, true)
	p.nativeState = s
	if s.State != previous.State || s.EffectivePlaying != previous.EffectivePlaying {
		p.sync()
	}
	p.loading = s.State == engine.Preparing || s.State == engine.Idle
	if s.State != engine.Preparing && s.AppliedCommand >= p.nativeCommand && s.RequestedGeneration.Seek == s.AppliedGeneration.Seek {
		wasSeeking := p.seeking
		p.seeking = false
		p.pending = 0
		if wasSeeking && s.State != engine.Failed {
			p.prepareNativeNext()
		}
	}
	if s.State == engine.Failed {
		p.dropNetEaseAhead()
		if previous.State != engine.Failed {
			p.interruptedPlaying = p.playing
		}
		p.seeking = false
		p.err, p.playing, p.loading = s.Error, false, false
		p.sync()
		return
	}
	if s.EffectivePlaying {
		p.record()
		if !previous.EffectivePlaying || advanced {
			p.refreshAhead()
		}
	}
	if s.State == engine.Ended && p.playing && !p.nativeEnded {
		p.nativeEnded = true
		p.ended()
	}
}

func nativeDuration(frames uint64, rate int) time.Duration {
	if rate <= 0 {
		return 0
	}
	return time.Duration(frames/uint64(rate))*time.Second + time.Duration(frames%uint64(rate))*time.Second/time.Duration(rate)
}
