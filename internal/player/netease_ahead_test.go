package player

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	engine "mygo-music/internal/audioengine/controller"
	"mygo-music/internal/music"
)

// Native playback prepares the full next FLAC while the current track plays.
// Advancing either before or after completion hands that same fetch to playback.
func TestNativeNetEasePrefetchHandoff(t *testing.T) {
	for _, state := range []string{"cached", "in flight", "replaced"} {
		t.Run(state, func(t *testing.T) {
			b, path := testLosslessFLAC(t)
			cache := NewCache(t.TempDir(), 1<<20)
			if _, kept := cache.keep(neteaseSource, "current", "lossless", &music.Stream{Type: "flac", Quality: "lossless"}, path, int64(len(b))); !kept {
				t.Fatal("could not prepare current track's cache")
			}
			started, finish, canceled := make(chan struct{}, 2), make(chan struct{}), make(chan struct{}, 2)
			var requests atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				w.Header().Set("Content-Length", strconv.Itoa(len(b)))
				w.Write(b[:len(b)/2])
				w.(http.Flusher).Flush()
				started <- struct{}{}
				select {
				case <-finish:
					w.Write(b[len(b)/2:])
				case <-r.Context().Done():
					canceled <- struct{}{}
				}
			}))
			t.Cleanup(srv.Close)
			src := &neteaseTestSource{controllerSource{request: func(id, quality string) (*music.Stream, error) {
				if id != "next" || quality != "lossless" {
					t.Errorf("unexpected stream request for %s at %s", id, quality)
					return nil, music.ErrUnavailable
				}
				return &music.Stream{URL: srv.URL, Type: "flac", Quality: "lossless", Size: int64(len(b))}, nil
			}}}
			p, opened := nativePlayer(t, src)
			p.Cache = cache
			prefs := OutputPreferences{Mode: OutputProcessed, EndpointID: "chosen-dac"}
			prefs.DSP.PreampDB = -4
			if err := p.SetOutput(prefs); err != nil {
				t.Fatal(err)
			}
			queue := []music.Track{{Source: neteaseSource, ID: "current"}, {Source: neteaseSource, ID: "next"}}
			waitPlaying := func(id string) PlaybackSnapshot {
				return awaitPlayback(t, p, func(s PlaybackSnapshot) bool {
					return s.Current.ID == id && s.AudioOutput.Native.EffectivePlaying
				})
			}
			p.PlayQueue(queue, 0)
			waitPlaying("current")
			current := <-opened
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("next fetch did not start during current playback")
			}
			var job *neteaseAhead
			p.controller.call(func(s *playbackState) { job = s.neteaseAhead })
			if job == nil {
				t.Fatal("prefetch did not belong to the playback queue")
			}
			waitPrepared := func() {
				select {
				case <-job.done:
				case <-time.After(5 * time.Second):
					t.Fatal("prefetch did not finish")
				}
			}
			nextPath := filepath.Join(cache.dir, cacheKey(neteaseSource, "next", "lossless")+".flac")
			switch state {
			case "cached":
				close(finish)
				waitPrepared()
				if info, err := os.Stat(nextPath); err != nil || info.Size() != int64(len(b)) {
					t.Fatal("validated next track did not reach the cache")
				}
				current.change(func(s *engine.Snapshot) { s.State, s.EffectivePlaying = engine.Ended, false })
			case "in flight":
				p.Next()
				if s := p.Snapshot(); !s.Loading || s.AudioOutput.Native.EffectivePlaying {
					t.Fatal("next track opened before prefetch finished")
				}
				close(finish)
			case "replaced":
				p.PlayTrack(queue[:1], 0)
				select {
				case <-canceled:
				case <-time.After(5 * time.Second):
					t.Fatal("replaced prefetch kept downloading")
				}
				waitPrepared()
				if _, err := os.Stat(nextPath); !os.IsNotExist(err) {
					t.Fatal("aborted prefetch entered the cache")
				}
				if parts, _ := filepath.Glob(filepath.Join(cache.dir, "*.part")); len(parts) != 0 {
					t.Fatal("aborted prefetch left temporary audio")
				}
				return
			}
			s := waitPlaying("next")
			if requests.Load() != 1 || s.AudioOutput.Requested.Mode != prefs.Mode || s.AudioOutput.Requested.DSP != prefs.DSP {
				t.Fatalf("handoff changed output preferences or made %d downloads", requests.Load())
			}
		})
	}
}
