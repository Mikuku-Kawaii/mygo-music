package player

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"mygo-music/internal/lyrics"
	"mygo-music/internal/music"
)

type controllerSource struct {
	nothing
	stream  func(string) (*music.Stream, error)
	request func(string, string) (*music.Stream, error)
}

func (s *controllerSource) Stream(id, quality string) (*music.Stream, error) {
	if s.request != nil {
		return s.request(id, quality)
	}
	return s.stream(id)
}

// A changed UI quality is captured before natural next; no later UI callback
// is needed, and the old quality's prepared source is not silently reused.
func TestControllerRefreshQualityBeforeNaturalNext(t *testing.T) {
	url := controllerFile(t, 48000*5)
	p, _ := controlledPlayer(t, &controllerSource{request: func(id, quality string) (*music.Stream, error) {
		return &music.Stream{URL: url, Type: "wav", Quality: quality}, nil
	}})
	quality := "low"
	p.Quality = func() string { return quality }
	p.PlayQueue(controllerQueue("a", "b"), 0)
	awaitPlayback(t, p, func(s PlaybackSnapshot) bool { return s.Ready })
	quality = "high"
	p.RefreshConfiguration()
	p.Seek(5 * time.Second)
	s := awaitPlayback(t, p, func(s PlaybackSnapshot) bool { return s.Current.ID == "b" && s.Ready })
	if s.Stream.Quality != "high" || !s.Playing {
		t.Fatal(s)
	}
}

func controllerFile(t *testing.T, frames int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "silent.wav")
	if err := os.WriteFile(path, wavFile(1, 2, 16, 48000, make([]byte, frames*4)), 0600); err != nil {
		t.Fatal(err)
	}
	return music.FileURL(path)
}
func controlledPlayer(t *testing.T, src music.Source) (*Player, chan func()) {
	t.Helper()
	ui := make(chan func(), 4)
	p := New(func(fn func()) { ui <- fn })
	p.Source = func(string) music.Source { return src }
	p.SetVolume(0)
	t.Cleanup(p.Close)
	return p, ui
}
func awaitPlayback(t *testing.T, p *Player, want func(PlaybackSnapshot) bool) PlaybackSnapshot {
	return awaitPlaybackFor(t, p, 5*time.Second, want)
}

func awaitPlaybackFor(t *testing.T, p *Player, timeout time.Duration, want func(PlaybackSnapshot) bool) PlaybackSnapshot {
	t.Helper()
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		s := p.Snapshot()
		if want(s) {
			return s
		}
		select {
		case <-tick.C:
		case <-deadline.C:
			t.Fatalf("playback did not settle: %+v", s)
		}
	}
}
func controllerQueue(ids ...string) []music.Track {
	var q []music.Track
	for _, id := range ids {
		q = append(q, music.Track{ID: id, Source: "test", Name: id})
	}
	return q
}

// Real finite decoder/output completion advances twice without executing any
// UI post; only the latest immutable notification is waiting for that UI.
func TestControllerNaturalNextWithoutUI(t *testing.T) {
	url := controllerFile(t, 4800)
	var asked atomic.Int32
	src := &controllerSource{stream: func(string) (*music.Stream, error) {
		asked.Add(1)
		return &music.Stream{URL: url, Type: "wav", Headers: map[string]string{"Authorization": "private"}}, nil
	}}
	p, ui := controlledPlayer(t, src)
	changes := 0
	p.Changed = func() { changes++ }
	p.PlayQueue(controllerQueue("a", "b", "c"), 0)
	before := changes
	s := awaitPlayback(t, p, func(s PlaybackSnapshot) bool { return s.Current != nil && s.Current.ID == "c" && s.Ready && !s.Playing })
	if s.At != 2 || s.SessionGeneration != 3 || s.Error != "" || s.Backend != "mygo-compatible" {
		t.Fatal(s)
	}
	if changes != before {
		t.Fatal("app callback ran without a UI dispatch")
	}
	if len(ui) != 1 {
		t.Fatalf("%d UI updates queued, want one coalesced update", len(ui))
	}
	if asked.Load() != 3 {
		t.Fatalf("source queried %d times for three tracks", asked.Load())
	}
	if s.Stream.URL != "" || len(s.Stream.Headers) != 0 {
		t.Fatal("private transport data escaped in snapshot")
	}
	(<-ui)()
	if changes == before {
		t.Fatal("UI did not receive latest confirmed change")
	}
}

// A blocked source cannot hold up pause/selection; a late A cannot overwrite
// ready B, and neither preparation nor completion invokes the UI dispatcher.
func TestControllerSlowSourcePauseAndReplace(t *testing.T) {
	url := controllerFile(t, 48000*10)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	src := &controllerSource{stream: func(id string) (*music.Stream, error) {
		if id == "a" {
			close(entered)
			<-release
		}
		return &music.Stream{URL: url, Type: "wav", Quality: id}, nil
	}}
	p, _ := controlledPlayer(t, src)
	finished := make(chan struct{}, 2)
	p.controller.call(func(state *playbackState) {
		run := state.runLoad
		state.runLoad = func(fn func()) { run(func() { fn(); finished <- struct{}{} }) }
	})
	p.PlayTrack(controllerQueue("a"), 0)
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("source did not start")
	}
	p.TogglePlay()
	if p.Playing() || !p.Loading() {
		t.Fatal("pause intent lost during loading")
	}
	p.PlayTrack(controllerQueue("b"), 0)
	p.TogglePlay()
	s := awaitPlayback(t, p, func(s PlaybackSnapshot) bool { return s.Current.ID == "b" && s.Ready })
	if s.Playing || s.Position != 0 {
		t.Fatal("replacement did not preserve pause", s)
	}
	once.Do(func() { close(release) })
	for range 2 {
		select {
		case <-finished:
		case <-time.After(3 * time.Second):
			t.Fatal("source job did not return")
		}
	}
	// Drain all events actually handed off by both load jobs before checking.
	barrier := make(chan struct{})
	p.controller.deliver(func() { close(barrier) })
	<-barrier
	if p.Current().ID != "b" || p.Playing() || p.Stream().Quality != "b" {
		t.Fatal(p.Snapshot())
	}
}

// Releasing the selected source after a pending pause prepares it silently;
// accepting a command and applying the source result are separate events.
func TestControllerPrepareKeepsPause(t *testing.T) {
	url := controllerFile(t, 48000*10)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	p, _ := controlledPlayer(t, &controllerSource{stream: func(string) (*music.Stream, error) {
		close(entered)
		<-release
		return &music.Stream{URL: url, Type: "wav"}, nil
	}})
	p.PlayTrack(controllerQueue("a"), 0)
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("source did not start")
	}
	p.TogglePlay()
	once.Do(func() { close(release) })
	s := awaitPlayback(t, p, func(s PlaybackSnapshot) bool { return s.Ready })
	if s.Playing || s.Position != 0 {
		t.Fatal(s)
	}
	p.controller.call(func(state *playbackState) {
		if state.out.Playing() {
			t.Error("output started despite pause")
		}
	})
}

// Queue, nested track artists, lyrics and session copies cannot be used to
// mutate the authoritative state, even while source workers run.
func TestControllerSnapshotOwnership(t *testing.T) {
	p, _ := controlledPlayer(t, nil)
	q := controllerQueue("a", "b")
	q[0].Artists = []music.Artist{{Name: "original"}}
	q[0].Album.Artists = []music.Artist{{Name: "album"}}
	p.Cue(q)
	lines := []lyrics.Line{{Text: "line", Words: []lyrics.Word{{Text: "word"}}}}
	p.SetLyrics(lines)
	lines[0].Words[0].Text = "input changed"
	q[0].Artists[0].Name = "input changed"
	s := p.Snapshot()
	s.Queue[0].ID = "changed"
	s.Current.Artists[0].Name = "changed"
	s.Queue[0].Album.Artists[0].Name = "changed"
	s.Order[0] = 1
	s.Lyrics[0].Words[0].Text = "changed"
	session, _ := p.Session()
	session.Queue[0].Name = "changed"
	fresh := p.Snapshot()
	if fresh.Current.Artists[0].Name != "original" || fresh.Queue[0].Album.Artists[0].Name != "album" || fresh.Queue[0].ID != "a" || fresh.Order[0] != 0 || fresh.Queue[0].Name != "a" || fresh.Lyrics[0].Words[0].Text != "word" {
		t.Fatal(fresh)
	}
}

// Closing during a source call invalidates its result without requiring a UI
// callback or waiting indefinitely for a context-free third-party Source API.
func TestControllerCloseWithLateSource(t *testing.T) {
	entered, release, returned := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	p, _ := controlledPlayer(t, &controllerSource{stream: func(string) (*music.Stream, error) {
		close(entered)
		<-release
		defer close(returned)
		return nil, errors.New("late source error")
	}})
	p.PlayTrack(controllerQueue("a"), 0)
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("source did not start")
	}
	done := make(chan struct{})
	go func() { p.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Close waited for source or UI")
	}
	once.Do(func() { close(release) })
	<-returned
	if s := p.Snapshot(); !s.Closed || s.Playing || s.Ready || s.Loading {
		t.Fatal(s)
	}
	p.Next()
	if !p.Snapshot().Closed {
		t.Fatal("closed controller accepted another session")
	}
}

// A radio response goes directly to the controller while the UI is frozen,
// preserves pause at the boundary, and a replaced radio's late batch is ignored.
func TestControllerRadioResponseAndPauseWithoutUI(t *testing.T) {
	url := controllerFile(t, 48000*10)
	p, _ := controlledPlayer(t, &controllerSource{stream: func(string) (*music.Stream, error) { return &music.Stream{URL: url, Type: "wav"}, nil }})
	answer := make(chan func([]music.Track), 2)
	p.PlayRadio("radio", controllerQueue("a"), func(got func([]music.Track)) { answer <- got })
	var got func([]music.Track)
	select {
	case got = <-answer:
	case <-time.After(3 * time.Second):
		t.Fatal("feed not requested")
	}
	awaitPlayback(t, p, func(s PlaybackSnapshot) bool { return s.Ready })
	// Act at the exhausted queue boundary without waiting for ten seconds of
	// silence; all changes still pass through the real controller.
	p.controller.call(func(s *playbackState) { s.stopForFeed(); s.awaitFeed() })
	p.TogglePlay()
	got(controllerQueue("b"))
	s := awaitPlayback(t, p, func(s PlaybackSnapshot) bool { return s.Current.ID == "b" && s.Ready })
	if s.Playing {
		t.Fatal("radio resumed after pause", s)
	}
	p.EndRadio()
	got(controllerQueue("late"))
	p.controller.call(func(*playbackState) {})
	if p.Radio() != "" || len(p.Queue()) != 2 {
		t.Fatal(p.Snapshot())
	}
}
