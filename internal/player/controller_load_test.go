package player

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"mygo-music/internal/music"
)

// Two stalled context-free source APIs cannot create unlimited opens: rapid
// selections still commit, obsolete queued work is canceled, and only the last
// selection prepares when the two workers become available.
func TestControllerBoundsSlowSourceWork(t *testing.T) {
	url := controllerFile(t, 48000*10)
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	var active, peak, asked atomic.Int32
	p, _ := controlledPlayer(t, &controllerSource{stream: func(id string) (*music.Stream, error) {
		n := active.Add(1)
		defer active.Add(-1)
		for old := peak.Load(); n > old && !peak.CompareAndSwap(old, n); old = peak.Load() {
		}
		asked.Add(1)
		if id == "a" || id == "b" {
			entered <- struct{}{}
			<-release
		}
		return &music.Stream{URL: url, Type: "wav", Quality: id}, nil
	}})
	for _, id := range []string{"a", "b"} {
		p.PlayTrack(controllerQueue(id), 0)
		select {
		case <-entered:
		case <-time.After(3 * time.Second):
			t.Fatal("source worker not started")
		}
	}
	for i := range 200 {
		p.PlayTrack(controllerQueue(fmt.Sprint(i)), 0)
	}
	p.TogglePlay()
	if p.Current().ID != "199" || p.Playing() || !p.Loading() || peak.Load() != 2 {
		t.Fatal(p.Snapshot(), peak.Load())
	}
	once.Do(func() { close(release) })
	s := awaitPlayback(t, p, func(s PlaybackSnapshot) bool { return s.Ready })
	if s.Current.ID != "199" || s.Stream.Quality != "199" || s.Playing || asked.Load() != 3 || peak.Load() > 2 {
		t.Fatal(s, asked.Load(), peak.Load())
	}
}

// Closing the controller drops queued remote side effects while a plugin is
// busy; finishing that in-flight call cannot start another command afterward.
func TestControllerCloseDropsQueuedRemoteCommands(t *testing.T) {
	p, _ := controlledPlayer(t, nil)
	entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	var sent atomic.Int32
	p.controller.call(func(state *playbackState) {
		state.cmds.add(func() { close(entered); <-release; close(finished) })
	})
	<-entered
	p.controller.call(func(state *playbackState) { state.cmds.add(func() { sent.Add(1) }) })
	p.Close()
	once.Do(func() { close(release) })
	<-finished
	// Inspect queue termination under its own mutex; the active command may
	// have returned but not yet completed the command worker's last iteration.
	eventually(t, "remote command worker did not stop", func() bool {
		cmds := &p.controller.state.cmds
		cmds.mu.Lock()
		defer cmds.mu.Unlock()
		return !cmds.busy
	})
	if sent.Load() != 0 {
		t.Fatal("remote command sent after Close")
	}
}
