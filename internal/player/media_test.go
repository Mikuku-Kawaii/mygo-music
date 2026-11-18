package player

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/egoist/mygo"
	"mygo-music/internal/audioengine/analysis"
	engine "mygo-music/internal/audioengine/controller"
	"mygo-music/internal/music"
)

func mediaFixture(id string) (*music.Stream, error) {
	return &music.Stream{URL: music.FileURL(id + ".wav")}, nil
}

func TestSystemMediaUsesPresentedSession(t *testing.T) {
	p, opened := nativePlayer(t, &controllerSource{stream: mediaFixture})
	p.BindSystemMedia()
	p.PlayQueue(controllerQueue("a", "b"), 0)
	a := <-opened
	awaitPlayback(t, p, func(s PlaybackSnapshot) bool { return s.Ready && s.AudioOutput.Native.EffectivePlaying })
	p.apply()
	info, ok := mygo.NowPlaying.Info()
	if !ok || info.Title != "a" || info.State != mygo.PlaybackPlaying || info.Commands&mygo.MediaSeek == 0 {
		t.Fatal("missing confirmed Now Playing information", info)
	}

	p.mediaCommand(mygo.MediaPause, 0)
	p.mediaCommand(mygo.MediaPause, 0)
	awaitPlayback(t, p, func(s PlaybackSnapshot) bool {
		return !s.Playing && s.AudioOutput.Native.State == engine.Paused
	})
	p.mediaCommand(mygo.MediaPlay, 0)
	p.mediaCommand(mygo.MediaPlay, 0)
	p.mediaCommand(mygo.MediaSeek, 100*time.Millisecond)
	awaitPlayback(t, p, func(s PlaybackSnapshot) bool {
		return s.AudioOutput.Native.AppliedGeneration.Seek == 1 && s.Playing && s.AudioOutput.Native.EffectivePlaying
	})
	a.mu.Lock()
	plays := a.plays
	a.mu.Unlock()
	if plays != 2 {
		t.Fatal("duplicate system media commands toggled playback", plays)
	}

	a.change(func(s *engine.Snapshot) { s.State, s.EffectivePlaying = engine.Ended, false })
	awaitPlayback(t, p, func(s PlaybackSnapshot) bool {
		return s.Current.ID == "b" && s.AudioOutput.Native.EffectivePlaying
	})
	b := <-opened
	p.mediaCommand(mygo.MediaSeek, 250*time.Millisecond) // still showing A in the shell
	p.mediaCommand(mygo.MediaPause, 0)
	awaitPlayback(t, p, func(s PlaybackSnapshot) bool {
		return !s.Playing && s.AudioOutput.Native.State == engine.Paused
	})
	if b.Snapshot().RequestedGeneration.Seek != 0 {
		t.Fatal("system seek from the previous song reached the new song")
	}

	p.apply()
	p.mediaCommand(mygo.MediaSeek, 500*time.Millisecond)
	awaitPlayback(t, p, func(s PlaybackSnapshot) bool {
		return s.Ready && s.Position == 500*time.Millisecond && s.AudioOutput.Native.AppliedGeneration.Seek == 1
	})
	p.apply()
	info, ok = mygo.NowPlaying.Info()
	if !ok || info.Title != "b" || info.Position != 500*time.Millisecond || info.State != mygo.PlaybackPaused {
		t.Fatal("Now Playing lost the confirmed paused seek", info)
	}
	p.Close()
	if _, ok := mygo.NowPlaying.Info(); ok || !b.closed.Load() {
		t.Fatal("close left the shell session or device active")
	}
}

func TestVisualizationVisibilityLeavesCurrentOutput(t *testing.T) {
	p, opened := nativePlayer(t, &controllerSource{stream: mediaFixture})
	p.SetVisualizationVisible(true)
	p.PlayQueue(controllerQueue("a"), 0)
	h := <-opened
	playing := awaitPlayback(t, p, func(s PlaybackSnapshot) bool { return s.AudioOutput.Native.EffectivePlaying })
	samples := func() {
		h.change(func(s *engine.Snapshot) {
			s.Measurement = analysis.Measurement{Valid: true, Token: p.controller.visual.Token()}
			s.Measurement.Bands[0] = .6
		})
	}
	samples()
	awaitPlayback(t, p, func(s PlaybackSnapshot) bool { return s.HasLevels })
	p.SetVisualizationVisible(false)
	if _, ok := p.Levels(); ok || p.OutputStatus().Native.Measurement.Valid {
		t.Fatal("hidden view retained a visible measurement")
	}
	p.SetVisualizationVisible(true)
	if _, ok := p.Levels(); ok || p.OutputStatus().Native.Measurement.Valid {
		t.Fatal("reopened view exposed a previous subscription")
	}
	samples()
	got := awaitPlayback(t, p, func(s PlaybackSnapshot) bool { return s.HasLevels && p.OutputStatus().Native.Measurement.Valid })
	if got.Current.ID != "a" || got.SessionGeneration != playing.SessionGeneration || !got.Playing || h.closed.Load() {
		t.Fatal("changing visualization visibility affected playback", got)
	}
}

type openingNative struct {
	*testNative
	entered, canceled chan struct{}
}

func (h *openingNative) SetQueue(ctx context.Context, _ []engine.Media, _ int) (*engine.Receipt, error) {
	close(h.entered)
	<-ctx.Done()
	close(h.canceled)
	return nil, ctx.Err()
}

func TestQuitWaitsForOpeningNativeLease(t *testing.T) {
	p, _ := nativePlayer(t, &controllerSource{stream: mediaFixture})
	stopped := make(chan struct{})
	release := sync.OnceFunc(func() { close(stopped) })
	defer release()
	h := &openingNative{testNative: newTestNative(), entered: make(chan struct{}), canceled: make(chan struct{})}
	h.closeWait = stopped
	p.controller.call(func(s *playbackState) {
		s.openNative = func(engine.Config) (nativeEngine, error) { return h, nil }
	})
	p.PlayQueue(controllerQueue("a"), 0)
	select {
	case <-h.entered:
	case <-time.After(time.Second):
		t.Fatal("no native preparation")
	}
	quit := make(chan struct{})
	go func() {
		p.Close()
		close(quit)
	}()
	select {
	case <-h.canceled:
	case <-time.After(time.Second):
		t.Fatal("quit did not cancel native preparation")
	}
	select {
	case <-quit:
		t.Fatal("quit returned before releasing a device being opened")
	case <-time.After(20 * time.Millisecond):
	}
	release()
	select {
	case <-quit:
	case <-time.After(time.Second):
		t.Fatal("quit waited after device release")
	}
	if !p.Snapshot().Closed || !h.closed.Load() {
		t.Fatal("controller returned with native output still open")
	}
}
