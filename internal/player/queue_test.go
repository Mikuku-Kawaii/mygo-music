package player

import (
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	engine "mygo-music/internal/audioengine/controller"
	"mygo-music/internal/music"
)

func queueNative(t *testing.T) (*Player, chan *testNative) {
	return nativePlayer(t, &controllerSource{stream: func(string) (*music.Stream, error) {
		return &music.Stream{URL: music.FileURL("silent.wav")}, nil
	}})
}

func nativeEnd(h *testNative) {
	h.change(func(s *engine.Snapshot) {
		s.State, s.EffectivePlaying = engine.Ended, false
		s.PositionFrames = uint64(s.Source.Frames)
	})
}

// Queue edits change their own revision, while selecting, seeking and pausing
// do not. Shuffle preserves the selected occurrence even for duplicate IDs.
func TestQueueRevisionAndDuplicateShuffle(t *testing.T) {
	p, opened := queueNative(t)
	q := controllerQueue("same", "b", "same", "d")
	q[0].Name, q[2].Name = "first copy", "second copy"
	p.PlayQueue(q, 2)
	<-opened
	s := awaitPlayback(t, p, func(s PlaybackSnapshot) bool { return s.Ready })
	revision, session := s.QueueRevision, s.SessionGeneration
	if revision == 0 {
		t.Fatal("missing queue revision")
	}
	p.TogglePlay()
	p.Seek(200 * time.Millisecond)
	awaitPlayback(t, p, func(s PlaybackSnapshot) bool { return s.AudioOutput.Native.AppliedGeneration.Seek == 1 })
	if p.Snapshot().QueueRevision != revision {
		t.Fatal("transport command changed queue plan")
	}
	p.ToggleShuffle()
	s = p.Snapshot()
	if !s.Shuffle || s.Order[s.At] != 2 || s.Current.Name != "second copy" || s.SessionGeneration != session || s.Playing || s.QueueRevision != revision+1 {
		t.Fatal(s)
	}
	order := slices.Clone(s.Order)
	slices.Sort(order)
	if !slices.Equal(order, []int{0, 1, 2, 3}) {
		t.Fatal(order)
	}
	p.ToggleShuffle()
	s = p.Snapshot()
	if s.At != 2 || !slices.Equal(s.Order, []int{0, 1, 2, 3}) || s.Current.Name != "second copy" || s.SessionGeneration != session {
		t.Fatal(s)
	}
	p.CycleRepeat()
	if p.Snapshot().QueueRevision != revision+3 {
		t.Fatal(p.Snapshot())
	}
	p.Next()
	awaitPlayback(t, p, func(s PlaybackSnapshot) bool { return s.Ready && s.Current.ID == "d" })
	if p.Snapshot().QueueRevision != revision+3 {
		t.Fatal("selection changed queue contents revision")
	}
	p.PlayQueue(q, 0)
	if p.Snapshot().QueueRevision != revision+4 {
		t.Fatal("replacement did not invalidate plan")
	}
	before := p.Snapshot().QueueRevision
	p.PlayQueue(nil, 0)
	p.PlayShuffled(nil)
	p.Jump(999)
	if p.Snapshot().QueueRevision != before || p.Shuffle() {
		t.Fatal("no-op changed queue")
	}
}

// Native end follows Off/All/One for one and multiple tracks, and an obsolete
// or duplicate Ended report cannot restart or skip the new selection.
func TestQueueNativeRepeatMatrix(t *testing.T) {
	for _, count := range []int{1, 3} {
		for _, mode := range []RepeatMode{RepeatOff, RepeatAll, RepeatOne} {
			t.Run(fmt.Sprintf("%d/%d", count, mode), func(t *testing.T) {
				p, opened := queueNative(t)
				q := controllerQueue("a", "b", "c")[:count]
				p.PlayQueue(q, count-1)
				h := <-opened
				for range int(mode) {
					p.CycleRepeat()
				}
				before := awaitPlayback(t, p, func(s PlaybackSnapshot) bool { return s.AudioOutput.Native.EffectivePlaying })
				nativeEnd(h)
				var s PlaybackSnapshot
				if mode == RepeatOff {
					s = awaitPlayback(t, p, func(s PlaybackSnapshot) bool { return !s.Playing && s.AudioOutput.Native.State == engine.Ended })
					if s.SessionGeneration != before.SessionGeneration || s.At != count-1 {
						t.Fatal(s)
					}
				} else {
					s = awaitPlayback(t, p, func(s PlaybackSnapshot) bool {
						return s.SessionGeneration == before.SessionGeneration+1 && s.AudioOutput.Native.EffectivePlaying
					})
					wantAt := 0
					if mode == RepeatOne {
						wantAt = count - 1
					}
					if s.At != wantAt || !h.closed.Load() {
						t.Fatal(s)
					}
					<-opened
				}
				for range 3 {
					nativeEnd(h)
				}
				p.controller.call(func(st *playbackState) { st.observeNative() })
				after := p.Snapshot()
				if after.SessionGeneration != s.SessionGeneration || after.At != s.At || after.QueueRevision != before.QueueRevision {
					t.Fatal(after)
				}
				// Manual next/previous ignore RepeatOne and retain MyGo wrap behavior.
				p.Next()
				next := (s.At + 1) % count
				awaitPlayback(t, p, func(s PlaybackSnapshot) bool { return s.Ready && s.At == next })
				p.Prev()
				awaitPlayback(t, p, func(v PlaybackSnapshot) bool { return v.Ready && v.At == s.At })
			})
		}
	}
}

// Rapid A→B→C followed by Pause only applies C; sources returning A/B later
// cannot open another native transport or advance the queue without a UI pump.
func TestQueueRapidSelectionKeepsLatestPause(t *testing.T) {
	started, finished := make(chan string, 2), make(chan string, 2)
	release := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	p, opened := nativePlayer(t, &controllerSource{stream: func(id string) (*music.Stream, error) {
		if id != "c" {
			started <- id
			<-release
			finished <- id
		}
		return &music.Stream{URL: music.FileURL("silent.wav")}, nil
	}})
	p.PlayQueue(controllerQueue("a", "b", "c"), 0)
	if <-started != "a" {
		t.Fatal("wrong first selection")
	}
	p.Next()
	if <-started != "b" {
		t.Fatal("wrong second selection")
	}
	p.Next()
	p.TogglePlay()
	once.Do(func() { close(release) })
	<-finished
	<-finished
	s := awaitPlayback(t, p, func(s PlaybackSnapshot) bool { return s.Ready && s.Current.ID == "c" })
	h := <-opened
	if s.Playing || s.AudioOutput.Native.EffectivePlaying || s.SessionGeneration != 3 || s.QueueRevision != 1 {
		t.Fatal(s)
	}
	p.controller.call(func(st *playbackState) { st.observeNative() })
	if len(opened) != 0 {
		t.Fatal("obsolete selection opened transport")
	}
	p.TogglePlay()
	awaitPlayback(t, p, func(s PlaybackSnapshot) bool { return s.AudioOutput.Native.EffectivePlaying })
	p.Jump(1)
	p.TogglePlay()
	nativeEnd(h)
	s = awaitPlayback(t, p, func(s PlaybackSnapshot) bool { return s.Ready && s.Current.ID == "b" })
	if s.Playing || s.At != 1 || s.SessionGeneration != 4 {
		t.Fatal(s)
	}
}

// A source failure after Shuffle used the pre-shuffle index and could select
// the failed song again. It must continue from the same occurrence's new slot.
func TestQueueFailureAfterShuffle(t *testing.T) {
	url := controllerFile(t, 48000*20)
	entered, release := make(chan struct{}, 1), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	p, _ := controlledPlayer(t, &controllerSource{stream: func(id string) (*music.Stream, error) {
		if id == "bad" {
			select {
			case entered <- struct{}{}:
			default:
			}
			<-release
			return nil, music.ErrUnavailable
		}
		return &music.Stream{URL: url, Type: "wav"}, nil
	}})
	p.PlayQueue(controllerQueue("a", "b", "bad", "d"), 2)
	<-entered
	p.ToggleShuffle()
	p.TogglePlay()
	s := p.Snapshot()
	want := s.Queue[s.Order[1]].ID
	if s.At != 0 || s.Current.ID != "bad" {
		t.Fatal(s)
	}
	once.Do(func() { close(release) })
	s = awaitPlayback(t, p, func(s PlaybackSnapshot) bool { return s.Ready })
	if s.Current.ID != want || s.At != 1 || s.Playing || s.SessionGeneration != 2 {
		t.Fatal(s)
	}
}

// An order/policy change retires old prefetch even when the same source ID
// appears again; a late answer cannot be reused under a new queue revision.
func TestQueueRevisionRetiresPrefetch(t *testing.T) {
	p := newPlaybackState(func(func()) {})
	p.Cue(controllerQueue("a", "b"))
	done := make(chan struct{})
	a := &ahead{source: "test", id: "b", quality: "q", queueRevision: p.queueRevision, done: done}
	p.ahead = a
	p.CycleRepeat()
	if p.ahead != nil {
		t.Fatal("stale prefetch retained")
	}
	close(done)
	p.ahead = a // simulate a stale plan being offered by an adapter
	if p.takeAhead(controllerQueue("b")[0], "q") != nil {
		t.Fatal("stale revision reused")
	}
	p.dropAhead()
	rev := p.queueRevision
	p.appendTracks(controllerQueue("a", "b"))
	if p.queueRevision != rev {
		t.Fatal("duplicate append changed plan")
	}
	p.appendTracks(controllerQueue("c"))
	if p.queueRevision != rev+1 || len(p.queue) != 3 {
		t.Fatal("append did not revise plan")
	}
}

// Shuffle traverses each queue occurrence once in the published order, stops
// at its end with repeat off, and never reshuffles on a natural boundary.
func TestQueueShuffledTraversal(t *testing.T) {
	p, opened := queueNative(t)
	p.PlayShuffled(controllerQueue("a", "b", "c", "d", "e"))
	s := awaitPlayback(t, p, func(s PlaybackSnapshot) bool { return s.Ready })
	order, revision := slices.Clone(s.Order), s.QueueRevision
	seen := make(map[int]bool)
	for i, k := range order {
		h := <-opened
		s = awaitPlayback(t, p, func(s PlaybackSnapshot) bool { return s.At == i && s.AudioOutput.Native.EffectivePlaying })
		if seen[k] || s.Current.ID != s.Queue[k].ID || !slices.Equal(s.Order, order) || s.QueueRevision != revision {
			t.Fatal(s)
		}
		seen[k] = true
		nativeEnd(h)
	}
	s = awaitPlayback(t, p, func(s PlaybackSnapshot) bool { return !s.Playing })
	if s.At != 4 || s.SessionGeneration != 5 || len(seen) != 5 {
		t.Fatal(s)
	}
}

// Silent hardware confirms RepeatOne reopens the same selection and RepeatAll
// wraps the last song using actual device Ended, with no UI dispatcher running.
func TestQueueHardware(t *testing.T) {
	if os.Getenv("PRISM_NATIVE_SMOKE") != "1" {
		t.Skip("opt-in silent WASAPI queue test")
	}
	url := controllerFile(t, 48000*2)
	p, _ := controlledPlayer(t, &controllerSource{stream: func(string) (*music.Stream, error) { return &music.Stream{URL: url}, nil }})
	if err := p.SetOutput(OutputPreferences{Mode: OutputExclusive, EndpointID: os.Getenv("PRISM_NATIVE_ENDPOINT")}); err != nil {
		t.Fatal(err)
	}
	p.PlayQueue(controllerQueue("a", "b"), 1)
	p.CycleRepeat()
	p.CycleRepeat()
	s := awaitPlayback(t, p, func(s PlaybackSnapshot) bool {
		return s.Error != "" || s.SessionGeneration >= 2 && s.AudioOutput.Native.EffectivePlaying
	})
	if s.Error != "" || s.SessionGeneration != 2 || s.Current.ID != "b" || s.AudioOutput.Native.Underruns != 0 {
		t.Fatal(s)
	}
	p.TogglePlay()
	awaitPlayback(t, p, func(s PlaybackSnapshot) bool { return s.AudioOutput.Native.State == engine.Paused })
	p.CycleRepeat() // off
	p.CycleRepeat() // all
	p.TogglePlay()
	s = awaitPlayback(t, p, func(s PlaybackSnapshot) bool {
		return s.Error != "" || s.Current.ID == "a" && s.AudioOutput.Native.EffectivePlaying
	})
	if s.Error != "" || s.SessionGeneration != 3 || s.AudioOutput.Native.Underruns != 0 {
		t.Fatal(s)
	}
	p.TogglePlay()
	s = awaitPlayback(t, p, func(s PlaybackSnapshot) bool { return s.AudioOutput.Native.State == engine.Paused })
	if s.Playing || s.AudioOutput.Native.EffectivePlaying {
		t.Fatal(s)
	}
	b, _ := json.Marshal(struct {
		Session, Queue uint64
		At             int
		Native         engine.Snapshot
	}{s.SessionGeneration, s.QueueRevision, s.At, s.AudioOutput.Native})
	t.Log(string(b))
}
