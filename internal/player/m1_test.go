package player

import (
	"encoding/json"
	engine "mygo-music/internal/audioengine/controller"
	"mygo-music/internal/music"
	"testing"
	"time"
)

// Legacy absence migrates to compatible output; supported preferences survive
// round trips and unknown settings cannot replace the active configuration.
func TestM1OutputPreferencesMigration(t *testing.T) {
	if !NativeOutputAvailable() {
		t.Skip("Windows x64")
	}
	p, _ := queueNative(t)
	for _, raw := range []string{`{}`, `{"mode":"wasapi-exclusive"}`, `{"version":1,"mode":"wasapi-shared","endpointId":"fixed","containerBits":32}`} {
		var o OutputPreferences
		if err := json.Unmarshal([]byte(raw), &o); err != nil {
			t.Fatal(err)
		}
		if err := p.SetOutput(o); err != nil {
			t.Fatal(err)
		}
		o.Version = 1
		if got := p.AudioOutput().Requested; got != o {
			t.Fatal(got, o)
		}
		b, _ := json.Marshal(o)
		var round OutputPreferences
		json.Unmarshal(b, &round)
		if round != o {
			t.Fatal(round, o)
		}
	}
	before := p.AudioOutput().Requested
	for _, o := range []OutputPreferences{{Version: 2}, {Mode: "unknown"}, {ContainerBits: 24}, {Version: -1}} {
		if p.SetOutput(o) == nil || p.AudioOutput().Requested != before {
			t.Fatal("invalid preferences applied", o)
		}
	}
}

// A device failure preserves the last confirmed location and former intent;
// retry and a serialized restart prepare that position without starting audio.
func TestM1RecoverySession(t *testing.T) {
	p, opened := queueNative(t)
	p.PlayQueue(controllerQueue("a", "b"), 0)
	h := <-opened
	awaitPlayback(t, p, func(s PlaybackSnapshot) bool { return s.AudioOutput.Native.EffectivePlaying })
	h.change(func(s *engine.Snapshot) { s.PositionFrames = 24000 })
	awaitPlayback(t, p, func(s PlaybackSnapshot) bool { return s.Position == 500*time.Millisecond })
	h.change(func(s *engine.Snapshot) {
		s.State = engine.Failed
		s.Error = "device invalidated"
		s.ErrorKind = "device-lost"
		s.EffectivePlaying = false
	})
	s := awaitPlayback(t, p, func(s PlaybackSnapshot) bool { return s.Error != "" })
	if s.Playing || !s.AudioOutput.InterruptedPlaying || s.Position != 500*time.Millisecond {
		t.Fatal(s)
	}
	kept, ok := p.Session()
	if !ok || !kept.Paused || kept.Version != 1 || kept.Position != 500 {
		t.Fatal(kept)
	}
	p.TogglePlay()
	next := <-opened
	s = awaitPlayback(t, p, func(s PlaybackSnapshot) bool {
		return s.Ready && s.Error == "" && s.AudioOutput.Native.AppliedGeneration.Seek == 1
	})
	if s.Playing || s.Position != 500*time.Millisecond || !h.closed.Load() {
		t.Fatal(s)
	}
	next.mu.Lock()
	plays := next.plays
	next.mu.Unlock()
	if plays != 0 {
		t.Fatal("retry resumed automatically")
	}
	b, err := json.Marshal(kept)
	if err != nil {
		t.Fatal(err)
	}
	var restored Session
	if err = json.Unmarshal(b, &restored); err != nil {
		t.Fatal(err)
	}
	other, otherOpened := queueNative(t)
	other.Restore(restored)
	<-otherOpened
	s = awaitPlayback(t, other, func(s PlaybackSnapshot) bool { return s.Ready && s.AudioOutput.Native.AppliedGeneration.Seek == 1 })
	if s.Playing || s.Position != 500*time.Millisecond || s.Current.ID != "a" {
		t.Fatal(s)
	}
	for _, bad := range []Session{{Version: 2, Queue: kept.Queue, Order: kept.Order, At: 0}, {Queue: kept.Queue, Order: []int{0, 0}, At: 0}, {Queue: kept.Queue, Order: kept.Order, At: 0, Position: -1}, {Queue: kept.Queue, Order: kept.Order, At: 0, Repeat: -1}} {
		before := other.Snapshot().SessionGeneration
		other.Restore(bad)
		if other.Snapshot().SessionGeneration != before {
			t.Fatal("invalid session replaced playback")
		}
	}
}

type completionSource struct {
	controllerSource
	completed chan time.Duration
}

func (s *completionSource) Completed(_ music.Track, p time.Duration) { s.completed <- p }

// Started, paused and manually skipped tracks aren't completed; a native Ended
// records once, and Close drains accepted history writes before returning.
func TestM1CompletionBoundary(t *testing.T) {
	src := &completionSource{controllerSource: controllerSource{stream: func(string) (*music.Stream, error) { return &music.Stream{URL: music.FileURL("silent.wav")}, nil }}, completed: make(chan time.Duration, 8)}
	p, opened := nativePlayer(t, src)
	p.PlayQueue(controllerQueue("a", "b", "c"), 0)
	old := <-opened
	awaitPlayback(t, p, func(s PlaybackSnapshot) bool { return s.Ready })
	p.Next()
	current := <-opened
	awaitPlayback(t, p, func(s PlaybackSnapshot) bool { return s.Current.ID == "b" && s.AudioOutput.Native.EffectivePlaying })
	if len(src.completed) != 0 {
		t.Fatal("recorded manual skip")
	}
	nativeEnd(old)
	nativeEnd(current)
	awaitPlayback(t, p, func(s PlaybackSnapshot) bool { return s.Current.ID == "c" && s.Ready })
	for range 3 {
		nativeEnd(current)
	}
	p.Close()
	if len(src.completed) != 1 || <-src.completed != time.Second {
		t.Fatal("wrong completion boundary/count")
	}
}
