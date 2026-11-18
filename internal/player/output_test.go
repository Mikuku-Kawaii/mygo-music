package player

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	engine "mygo-music/internal/audioengine/controller"
	"mygo-music/internal/audioengine/decode"
	"mygo-music/internal/audioengine/fixture"
	"mygo-music/internal/audioengine/output"
	"mygo-music/internal/audioengine/pcm"
	"mygo-music/internal/music"
)

type testNative struct {
	mu        sync.Mutex
	s         engine.Snapshot
	media     engine.Media
	wake      chan struct{}
	plays     int
	closed    atomic.Bool
	closeWait <-chan struct{}
}

func newTestNative() *testNative { return &testNative{wake: make(chan struct{}, 1)} }
func (h *testNative) change(fn func(*engine.Snapshot)) {
	h.mu.Lock()
	fn(&h.s)
	h.s.EventSeq++
	h.mu.Unlock()
	select {
	case h.wake <- struct{}{}:
	default:
	}
}
func (h *testNative) SetQueue(_ context.Context, q []engine.Media, i int) (*engine.Receipt, error) {
	if len(q) != 1 || i != 0 {
		panic("application queue leaked into transport")
	}
	h.change(func(s *engine.Snapshot) {
		h.media = q[0]
		f := pcm.Format{Kind: pcm.Integer, Rate: 48000, Channels: 2, ChannelMask: 3, ValidBits: 16, ContainerBits: 16}
		s.State, s.Current, s.Selected = engine.Paused, q[0].Item, q[0].Item
		s.Source = decode.Info{Format: f, Frames: 48000, Container: "wav", NativePrecision: true, ExactSeek: true}
		s.Output = output.Opened{EndpointID: "test-device", Format: f}
	})
	return nil, nil
}
func (h *testNative) Play(context.Context) (*engine.Receipt, error) {
	h.change(func(s *engine.Snapshot) {
		h.plays++
		s.State, s.DesiredPlaying, s.EffectivePlaying = engine.Playing, true, true
	})
	return nil, nil
}
func (h *testNative) Pause(context.Context) (*engine.Receipt, error) {
	h.change(func(s *engine.Snapshot) { s.State, s.DesiredPlaying, s.EffectivePlaying = engine.Paused, false, false })
	return nil, nil
}
func (h *testNative) SetVolume(context.Context, float64) (*engine.Receipt, error) { return nil, nil }
func (h *testNative) Seek(_ context.Context, position time.Duration) (*engine.Receipt, error) {
	h.change(func(s *engine.Snapshot) {
		s.SeekTarget = position
		s.RequestedGeneration.Seek++
		s.RequestedGeneration.Format++
		s.AppliedGeneration = s.RequestedGeneration
		s.SourceStartFrame = uint64(min(int64(position)*int64(s.Source.Format.Rate)/int64(time.Second), s.Source.Frames))
		s.PositionFrames = s.SourceStartFrame
		s.State = engine.Paused
		if s.DesiredPlaying {
			s.State = engine.Playing
		}
	})
	return nil, nil
}
func (h *testNative) Snapshot() engine.Snapshot { h.mu.Lock(); defer h.mu.Unlock(); return h.s }
func (h *testNative) Changes() <-chan struct{}  { return h.wake }
func (h *testNative) Close() error {
	if h.closeWait != nil {
		<-h.closeWait
	}
	h.closed.Store(true)
	return nil
}

func nativePlayer(t *testing.T, src music.Source) (*Player, chan *testNative) {
	t.Helper()
	if !NativeOutputAvailable() {
		t.Skip("Windows x64 output mode")
	}
	p, _ := controlledPlayer(t, src)
	opened := make(chan *testNative, 10)
	p.controller.call(func(s *playbackState) {
		s.compatibleIdleWait = 0 // the injected transport never opens a shared sink
		s.openNative = func(engine.Config) (nativeEngine, error) {
			h := newTestNative()
			opened <- h
			return h, nil
		}
	})
	if err := p.SetOutput(OutputPreferences{Mode: OutputExclusive}); err != nil {
		t.Fatal(err)
	}
	return p, opened
}

// Changing the WASAPI buffer policy reopens at the last confirmed frame and stays paused.
func TestNativeBufferChangeKeepsPosition(t *testing.T) {
	p, opened := nativePlayer(t, &controllerSource{stream: func(string) (*music.Stream, error) {
		return &music.Stream{URL: music.FileURL("silent.wav")}, nil
	}})
	p.PlayQueue(controllerQueue("a"), 0)
	first := awaitPlayback(t, p, func(s PlaybackSnapshot) bool { return s.AudioOutput.Native.EffectivePlaying })
	h := <-opened
	h.change(func(s *engine.Snapshot) { s.PositionFrames = 12000 })
	const anchor = 250 * time.Millisecond
	awaitPlayback(t, p, func(s PlaybackSnapshot) bool { return s.Position == anchor })
	prefs := first.AudioOutput.Requested
	prefs.StableBuffer = true
	if err := p.SetOutput(prefs); err != nil {
		t.Fatal(err)
	}
	got := awaitPlayback(t, p, func(s PlaybackSnapshot) bool {
		return s.SessionGeneration > first.SessionGeneration && s.AudioOutput.Native.SourceStartFrame == 12000 && !s.Loading
	})
	if got.Playing || !got.Ready || got.Position != anchor || !got.AudioOutput.Requested.StableBuffer || !h.closed.Load() {
		t.Fatal(got)
	}
}

// The sole application queue advances on confirmed native end without a UI
// callback; repeating one opens a fresh transport and pausing survives it.
func TestNativeQueueWithoutUI(t *testing.T) {
	p, opened := nativePlayer(t, &controllerSource{stream: func(string) (*music.Stream, error) { return &music.Stream{URL: music.FileURL("silent.wav")}, nil }})
	p.PlayQueue(controllerQueue("a", "b"), 0)
	a := <-opened
	awaitPlayback(t, p, func(s PlaybackSnapshot) bool { return s.AudioOutput.Native.EffectivePlaying })
	a.change(func(s *engine.Snapshot) { s.State, s.EffectivePlaying = engine.Ended, false; s.PositionFrames = 48000 })
	awaitPlayback(t, p, func(s PlaybackSnapshot) bool { return s.Current.ID == "b" && s.AudioOutput.Native.EffectivePlaying })
	b := <-opened
	if !a.closed.Load() {
		t.Fatal("next transport overlapped old output")
	}
	p.CycleRepeat()
	p.CycleRepeat()
	b.change(func(s *engine.Snapshot) { s.State, s.EffectivePlaying = engine.Ended, false })
	awaitPlayback(t, p, func(s PlaybackSnapshot) bool {
		return s.SessionGeneration == 3 && s.AudioOutput.Native.EffectivePlaying
	})
	<-opened
	p.TogglePlay()
	s := awaitPlayback(t, p, func(s PlaybackSnapshot) bool { return !s.Playing && s.AudioOutput.Native.State == engine.Paused })
	if len(s.Queue) != 2 || s.At != 1 || s.Backend != "wasapi-exclusive" || !p.CanSeek() || p.CanAdjustVolume() {
		t.Fatal(s)
	}
	var mixed bool
	p.controller.call(func(s *playbackState) { mixed = s.out != nil })
	if mixed {
		t.Fatal("native PCM entered MyGo mixer")
	}
}

// Mode changes pause immediately but wait off the controller for the old
// device to close before preparing the compatible output.
func TestNativeModeSwitchReleasesBeforeCompatible(t *testing.T) {
	url := controllerFile(t, 48000)
	p, opened := nativePlayer(t, &controllerSource{stream: func(string) (*music.Stream, error) { return &music.Stream{URL: url}, nil }})
	p.PlayQueue(controllerQueue("a"), 0)
	h := <-opened
	awaitPlayback(t, p, func(s PlaybackSnapshot) bool { return s.AudioOutput.Native.EffectivePlaying })
	closed := make(chan struct{})
	h.closeWait = closed
	if err := p.SetOutput(OutputPreferences{}); err != nil {
		t.Fatal(err)
	}
	s := p.Snapshot()
	if s.Playing || s.Ready || s.Backend != "mygo-compatible" {
		close(closed)
		t.Fatal(s)
	}
	// Commands remain responsive even while the old device's close is blocked.
	p.SetVolume(.2)
	close(closed)
	s = awaitPlayback(t, p, func(s PlaybackSnapshot) bool { return s.Ready })
	if s.Playing || !h.closed.Load() || !p.CanSeek() {
		t.Fatal(s)
	}
	if err := p.SetOutput(OutputPreferences{Mode: OutputExclusive}); err != nil {
		t.Fatal(err)
	}
	s = awaitPlayback(t, p, func(s PlaybackSnapshot) bool { return s.Ready })
	next := <-opened
	next.mu.Lock()
	plays := next.plays
	next.mu.Unlock()
	if s.Playing || plays != 0 {
		t.Fatal("mode switch started unity-gain playback")
	}
}

// A source or device unsupported by strict mode stays selected with an error,
// never skipping the track or silently falling back to the mixer.
func TestNativeFailureAndRetry(t *testing.T) {
	var local atomic.Bool
	p, opened := nativePlayer(t, &controllerSource{stream: func(string) (*music.Stream, error) {
		url := "https://example.invalid/audio.flac"
		if local.Load() {
			url = music.FileURL("silent.wav")
		}
		return &music.Stream{URL: url}, nil
	}})
	p.PlayQueue(controllerQueue("a", "b"), 0)
	s := awaitPlayback(t, p, func(s PlaybackSnapshot) bool { return s.Error != "" })
	if s.Current.ID != "a" || s.Playing || s.Ready || len(opened) != 0 {
		t.Fatal(s)
	}
	local.Store(true)
	p.TogglePlay()
	h := <-opened
	s = awaitPlayback(t, p, func(s PlaybackSnapshot) bool { return s.Ready && s.Error == "" })
	if s.Playing {
		t.Fatal("retry must prepare paused")
	}
	p.TogglePlay()
	awaitPlayback(t, p, func(s PlaybackSnapshot) bool { return s.AudioOutput.Native.EffectivePlaying })
	h.change(func(s *engine.Snapshot) { s.State, s.Error, s.EffectivePlaying = engine.Failed, "device lost", false })
	s = awaitPlayback(t, p, func(s PlaybackSnapshot) bool { return s.Error == "device lost" })
	if s.Playing || s.Current.ID != "a" || s.Ready {
		t.Fatal(s)
	}
	p.TogglePlay()
	awaitPlayback(t, p, func(s PlaybackSnapshot) bool { return s.Ready && s.Error == "" })
	if !h.closed.Load() {
		t.Fatal("retry retained failed device")
	}
}

// Real hardware smoke uses only silence and is opt-in. It exercises the same
// application facade as the UI, including pause, clock, natural WAV→FLAC next.
func TestNativeHardwarePlayback(t *testing.T) {
	if os.Getenv("PRISM_NATIVE_SMOKE") != "1" {
		t.Skip("set PRISM_NATIVE_SMOKE=1 for silent WASAPI verification")
	}
	dir := t.TempDir()
	f := pcm.Format{Kind: pcm.Integer, Rate: 48000, Channels: 2, ChannelMask: 3, ValidBits: 16, ContainerBits: 16}
	for _, ext := range []string{"wav", "flac"} {
		path := filepath.Join(dir, "silent."+ext)
		var err error
		if ext == "wav" {
			err = fixture.WriteWAV(path, f, make([]int32, 96000))
		} else {
			err = fixture.WriteFLAC(path, f, make([]int32, 96000))
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	p, _ := controlledPlayer(t, &controllerSource{stream: func(id string) (*music.Stream, error) {
		return &music.Stream{URL: music.FileURL(filepath.Join(dir, "silent."+id))}, nil
	}})
	p.PlayQueue(controllerQueue("wav", "flac"), 0)
	awaitPlayback(t, p, func(s PlaybackSnapshot) bool { return s.Ready })
	if err := p.SetOutput(OutputPreferences{Mode: OutputExclusive, EndpointID: os.Getenv("PRISM_NATIVE_ENDPOINT")}); err != nil {
		t.Fatal(err)
	}
	s := awaitPlaybackFor(t, p, 10*time.Second, func(s PlaybackSnapshot) bool { return s.Ready || s.Error != "" })
	if s.Error != "" || s.Playing || s.AudioOutput.Native.EffectivePlaying {
		t.Fatal(s)
	}
	p.TogglePlay()
	awaitPlayback(t, p, func(s PlaybackSnapshot) bool { return s.AudioOutput.Native.PositionFrames > 0 })
	p.TogglePlay()
	s = awaitPlayback(t, p, func(s PlaybackSnapshot) bool { return s.AudioOutput.Native.State == engine.Paused })
	if s.Playing || s.AudioOutput.Native.EffectivePlaying {
		t.Fatal(s)
	}
	p.TogglePlay()
	s = awaitPlayback(t, p, func(s PlaybackSnapshot) bool {
		return s.Current.ID == "flac" && s.AudioOutput.Native.State == engine.Ended
	})
	n := s.AudioOutput.Native
	if s.Playing || n.PositionFrames != 48000 || n.Underruns != 0 || n.Output.Format.ValidBits != 16 || n.Output.Format.ContainerBits != 16 {
		t.Fatal(s)
	}
	b, _ := json.Marshal(n)
	t.Log(string(b))
}

// An ended native slot awaiting a radio response must pause the queue's intent,
// not restart the ended song when the listener presses Pause.
func TestNativeRadioWaitKeepsPause(t *testing.T) {
	p, opened := nativePlayer(t, &controllerSource{stream: func(string) (*music.Stream, error) { return &music.Stream{URL: music.FileURL("silent.wav")}, nil }})
	feed := make(chan func([]music.Track), 4)
	p.PlayRadio("station", controllerQueue("a"), func(got func([]music.Track)) { feed <- got })
	h := <-opened
	got := <-feed
	awaitPlayback(t, p, func(s PlaybackSnapshot) bool { return s.AudioOutput.Native.EffectivePlaying })
	h.change(func(s *engine.Snapshot) { s.State, s.EffectivePlaying = engine.Ended, false })
	awaitPlayback(t, p, func(s PlaybackSnapshot) bool { return s.Loading && s.AudioOutput.Native.State == engine.Ended })
	p.TogglePlay()
	if p.Playing() {
		t.Fatal("pause restarted ended radio slot")
	}
	got(controllerQueue("b"))
	s := awaitPlayback(t, p, func(s PlaybackSnapshot) bool { return s.Current.ID == "b" && s.Ready })
	if s.Playing || s.AudioOutput.Native.EffectivePlaying {
		t.Fatal(s)
	}
}

// Seek issued while source resolution is pending is applied once the native
// handle arrives, without resuming a paused session or mutating the queue.
func TestNativeSeekBeforeReady(t *testing.T) {
	release := make(chan struct{})
	p, opened := nativePlayer(t, &controllerSource{stream: func(string) (*music.Stream, error) {
		<-release
		return &music.Stream{URL: music.FileURL("silent.wav")}, nil
	}})
	p.PlayQueue(controllerQueue("a", "b"), 0)
	p.Seek(500 * time.Millisecond)
	p.TogglePlay()
	if p.Position() != 0 || p.Playing() {
		t.Fatal("unconfirmed target published as position")
	}
	close(release)
	<-opened
	s := awaitPlayback(t, p, func(s PlaybackSnapshot) bool { return s.Ready && s.AudioOutput.Native.AppliedGeneration.Seek == 1 })
	if s.Playing || s.Position != 500*time.Millisecond || s.Current.ID != "a" || !p.CanSeek() || p.CanAdjustVolume() {
		t.Fatal(s)
	}
	p.TogglePlay()
	p.Seek(250 * time.Millisecond)
	s = awaitPlayback(t, p, func(s PlaybackSnapshot) bool { return s.Ready && s.AudioOutput.Native.AppliedGeneration.Seek == 2 })
	if !s.Playing || !s.AudioOutput.Native.EffectivePlaying || s.Position != 250*time.Millisecond || s.Current.ID != "a" {
		t.Fatal(s)
	}
}
