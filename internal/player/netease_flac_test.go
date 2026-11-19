package player

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	engine "mygo-music/internal/audioengine/controller"
	"mygo-music/internal/audioengine/decode"
	"mygo-music/internal/audioengine/fixture"
	"mygo-music/internal/audioengine/pcm"
	"mygo-music/internal/music"
)

type neteaseTestSource struct{ controllerSource }

func (neteaseTestSource) Info() music.Info {
	return music.Info{ID: neteaseSource, Playback: music.PlaybackStream}
}

func testLosslessFLAC(t *testing.T) ([]byte, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sample.flac")
	f := pcm.Format{Kind: pcm.Integer, Rate: 44100, Channels: 2, ChannelMask: 3, ValidBits: 24, ContainerBits: 32}
	if err := fixture.WriteFLAC(path, f, make([]int32, 8192)); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b, path
}

// Clicking a NetEase song uses the read-only MyGo cache or waits for a whole
// FLAC, then uses the selected native device and DSP. Replay hits the new cache.
func TestNativeNetEaseFLACCacheAndDownload(t *testing.T) {
	for _, mode := range []OutputMode{OutputShared, OutputExclusive, OutputProcessed} {
		t.Run(string(mode), func(t *testing.T) { checkNetEaseFLACCacheAndDownload(t, mode) })
	}
}

func checkNetEaseFLACCacheAndDownload(t *testing.T, mode OutputMode) {
	t.Helper()
	b, fixturePath := testLosslessFLAC(t)
	legacy := NewCache(t.TempDir(), 1<<20)
	legacyPath, kept := legacy.keep(neteaseSource, "cached", "lossless", &music.Stream{Type: "flac", Quality: "lossless", Gain: -3}, fixturePath, int64(len(b)))
	if !kept {
		t.Fatal("legacy cache fixture was not kept")
	}
	mtime := time.Unix(1700000000, 0)
	if err := os.Chtimes(legacyPath, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	started, finish := make(chan struct{}), make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Audio-Request") != "test" {
			t.Error("missing plugin stream header")
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(b)))
		w.Write(b[:len(b)/2])
		w.(http.Flusher).Flush()
		close(started)
		select {
		case <-finish:
			w.Write(b[len(b)/2:])
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)
	var requests atomic.Int32
	src := &neteaseTestSource{controllerSource{request: func(id, quality string) (*music.Stream, error) {
		requests.Add(1)
		if id != "fresh" || quality != "lossless" {
			t.Errorf("unexpected stream request for %s at %s", id, quality)
			return nil, music.ErrUnavailable
		}
		return &music.Stream{URL: srv.URL, Type: "flac", Quality: "lossless", Size: int64(len(b)), Headers: map[string]string{"X-Audio-Request": "test"}}, nil
	}}}
	cache := NewCacheWithFallback(t.TempDir(), 1<<20, legacy.dir)
	p, _ := nativePlayer(t, src)
	p.Cache = cache
	p.Quality = func() string { return "high" }
	opened := make(chan *testNative, 4)
	configs := make(chan engine.Config, 4)
	p.controller.call(func(s *playbackState) {
		s.openNative = func(c engine.Config) (nativeEngine, error) {
			h := newTestNative()
			opened <- h
			configs <- c
			return h, nil
		}
	})
	prefs := OutputPreferences{Mode: mode, EndpointID: "chosen-dac"}
	prefs.DSP.PreampDB = -5
	if err := p.SetOutput(prefs); err != nil {
		t.Fatal(err)
	}
	play := func(id string) { p.PlayTrack([]music.Track{{Source: neteaseSource, ID: id}}, 0) }
	check := func(id, wantPath string) {
		t.Helper()
		s := awaitPlayback(t, p, func(s PlaybackSnapshot) bool {
			return s.Current.ID == id && s.AudioOutput.Native.EffectivePlaying
		})
		h, cfg := <-opened, <-configs
		h.mu.Lock()
		media := h.media
		h.mu.Unlock()
		if media.Path != wantPath || s.Backend != string(mode) || s.Stream == nil || s.Stream.Quality != "lossless" || s.Stream.URL != "" || s.Stream.Headers != nil {
			t.Fatalf("unexpected transport path or stream: %s %+v", media.Path, s.Stream)
		}
		if cfg.Processing != (mode == OutputProcessed) || cfg.Shared != (mode == OutputShared) || cfg.EndpointID != prefs.EndpointID || cfg.DSP != prefs.DSP {
			t.Fatalf("native configuration lost the requested device or DSP: %+v", cfg)
		}
	}
	play("cached")
	check("cached", legacyPath)
	if stat, err := os.Stat(legacyPath); err != nil || !stat.ModTime().Equal(mtime) || requests.Load() != 0 {
		t.Fatal("legacy cache hit changed the file or resolved a stream")
	}
	play("fresh")
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("download did not start")
	}
	if len(opened) != 0 || !p.Snapshot().Loading {
		t.Fatal("native engine opened before the complete download")
	}
	close(finish)
	path := filepath.Join(p.Cache.dir, cacheKey(neteaseSource, "fresh", "lossless")+".flac")
	check("fresh", path)
	if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, b) {
		t.Fatal("cache did not retain the original FLAC bytes")
	}
	play("fresh")
	check("fresh", path)
	if requests.Load() != 1 {
		t.Fatal("replay resolved the cached song again")
	}
}

// A lower-quality response, short HTTP body or corrupt FLAC cannot become a
// playable file or a reusable cache entry, even if it has a FLAC extension.
func TestNetEaseFLACRejectsIncompleteOrLossy(t *testing.T) {
	b, _ := testLosslessFLAC(t)
	for _, tc := range []struct {
		name, quality string
		body          []byte
		size          int
	}{
		{"lower quality", "high", b, len(b)},
		{"short transfer", "lossless", b[:len(b)/2], len(b)},
		{"truncated FLAC", "lossless", b[:len(b)/2], len(b) / 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write(tc.body) }))
			defer srv.Close()
			src := &neteaseTestSource{controllerSource{request: func(string, string) (*music.Stream, error) {
				return &music.Stream{URL: srv.URL, Type: "flac", Quality: tc.quality, Size: int64(tc.size)}, nil
			}}}
			cache := NewCache(t.TempDir(), 1<<20)
			f, _, err := cache.openNetEaseQuality(t.Context(), src, "a", "lossless", true)
			f.release()
			entries, _ := os.ReadDir(cache.dir)
			if err == nil || f != nil || len(entries) != 0 {
				t.Fatalf("invalid download was accepted or kept: %v, %v", err, entries)
			}
		})
	}
}

// An unavailable lossless source reports the service's reason in the playback snapshot.
func TestNativeNetEaseFLACKeepsUnavailableReason(t *testing.T) {
	const reason = "MP3 320 kbps"
	src := &neteaseTestSource{controllerSource{request: func(string, string) (*music.Stream, error) {
		return nil, music.Fail(music.ErrUnavailable, reason)
	}}}
	p, _ := nativePlayer(t, src)
	p.PlayTrack([]music.Track{{Source: neteaseSource, ID: "a"}}, 0)
	s := awaitPlayback(t, p, func(s PlaybackSnapshot) bool { return s.Error != "" })
	if s.Error != reason {
		t.Fatalf("unavailable reason lost: %q", s.Error)
	}
}

// A full MP3 can play on the selected output when NetEase has no lossless stream.
func TestNativeNetEaseFallsBackWithoutChangingOutput(t *testing.T) {
	b, err := os.ReadFile("testdata/seek.mp3")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write(b)
	}))
	defer srv.Close()
	for _, tc := range []struct {
		mode    OutputMode
		quality string
	}{
		{OutputShared, "high"},
		{OutputExclusive, "standard"},
		{OutputProcessed, "high"},
	} {
		t.Run(string(tc.mode), func(t *testing.T) {
			requests := make(chan string, 8)
			src := &neteaseTestSource{controllerSource{request: func(_ string, quality string) (*music.Stream, error) {
				requests <- quality
				if quality != tc.quality {
					return nil, music.ErrUnavailable
				}
				return &music.Stream{URL: srv.URL, Type: "mp3", Quality: quality, Size: int64(len(b)), BitRate: 128000}, nil
			}}}
			cache := NewCache(t.TempDir(), 4<<20)
			p, _ := nativePlayer(t, src)
			p.Cache = cache
			opened := make(chan *testNative, 1)
			configs := make(chan engine.Config, 1)
			p.controller.call(func(s *playbackState) {
				s.openNative = func(c engine.Config) (nativeEngine, error) {
					h := newTestNative()
					opened <- h
					configs <- c
					return h, nil
				}
			})
			prefs := OutputPreferences{Mode: tc.mode, EndpointID: "chosen-dac"}
			prefs.DSP.PreampDB = -5
			if err := p.SetOutput(prefs); err != nil {
				t.Fatal(err)
			}
			p.PlayTrack([]music.Track{{Source: neteaseSource, ID: "mp3"}}, 0)
			s := awaitPlayback(t, p, func(s PlaybackSnapshot) bool {
				return s.Error != "" || s.AudioOutput.Native.EffectivePlaying
			})
			if s.Error != "" || s.Stream == nil || s.Stream.Type != "mp3" || s.Stream.Quality != tc.quality || s.Stream.BitRate != 128000 {
				t.Fatalf("fallback stream: %+v, error: %q", s.Stream, s.Error)
			}
			h, cfg := <-opened, <-configs
			h.mu.Lock()
			media := h.media
			h.mu.Unlock()
			if !media.DecodeCompressed || cfg.Processing != (tc.mode == OutputProcessed) || cfg.Shared != (tc.mode == OutputShared) ||
				cfg.EndpointID != prefs.EndpointID || cfg.DSP != prefs.DSP {
				t.Fatalf("fallback changed the selected output: %+v", cfg)
			}
			d, err := decode.OpenProcessed(t.Context(), media.Path)
			if err != nil {
				t.Fatal(err)
			}
			d.Close()
			if f, _ := cache.OpenNetEaseDownloadCache(t.Context(), "mp3"); f != nil {
				f.Close()
				t.Fatal("a playback fallback entered the lossless download cache")
			}
			want := []string{"lossless", "high"}
			if tc.quality == "standard" {
				want = append(want, "standard")
			}
			got := make([]string, 0, len(requests))
			for len(requests) > 0 {
				got = append(got, <-requests)
			}
			if !slices.Equal(got, want) {
				t.Fatalf("requested qualities %v, want %v", got, want)
			}
		})
	}
}

// Both an explicit selection and a prefetched next song can skip an unavailable track.
func TestNativeNetEaseSkipsUnavailableInPlayOrder(t *testing.T) {
	for _, afterEnd := range []bool{false, true} {
		t.Run(strconv.FormatBool(afterEnd), func(t *testing.T) {
			buf, _ := testLosslessFLAC(t)
			cache := NewCache(t.TempDir(), 1<<20)
			for _, id := range []string{"first", "last"} {
				path := filepath.Join(cache.dir, id+".part")
				if err := os.WriteFile(path, buf, 0600); err != nil {
					t.Fatal(err)
				}
				if _, ok := cache.keep(neteaseSource, id, "lossless", &music.Stream{Type: "flac", Quality: "lossless"}, path, int64(len(buf))); !ok {
					t.Fatal("could not cache test track")
				}
			}
			var requests atomic.Int32
			src := &neteaseTestSource{controllerSource{request: func(id, _ string) (*music.Stream, error) {
				if id != "missing" {
					t.Errorf("unexpected request for %s", id)
				}
				requests.Add(1)
				return nil, music.ErrUnavailable
			}}}
			p, opened := nativePlayer(t, src)
			p.Cache = cache
			queue := []music.Track{{Source: neteaseSource, ID: "first"}, {Source: neteaseSource, ID: "missing"}, {Source: neteaseSource, ID: "last"}}
			waitFor := func(id string) PlaybackSnapshot {
				return awaitPlayback(t, p, func(s PlaybackSnapshot) bool {
					return s.Error != "" || s.Current.ID == id && s.AudioOutput.Native.EffectivePlaying
				})
			}
			if afterEnd {
				p.PlayQueue(queue, 0)
				if s := waitFor("first"); s.Error != "" {
					t.Fatal(s.Error)
				}
				nativeEnd(<-opened)
			} else {
				p.PlayTrack(queue, 1)
			}
			if s := waitFor("last"); s.Error != "" || s.Current.ID != "last" || requests.Load() != 3 {
				t.Fatalf("did not advance past missing audio once: %s, %q, %d requests", s.Current.ID, s.Error, requests.Load())
			}
		})
	}
}

// A queue without playable audio stops after one pass even with repeat-all enabled.
func TestNativeNetEaseDoesNotLoopOnUnavailableQueue(t *testing.T) {
	var requests atomic.Int32
	src := &neteaseTestSource{controllerSource{request: func(_, _ string) (*music.Stream, error) {
		requests.Add(1)
		return nil, music.ErrUnavailable
	}}}
	p, _ := nativePlayer(t, src)
	p.controller.call(func(s *playbackState) { s.repeat = RepeatAll })
	queue := []music.Track{{Source: neteaseSource, ID: "a"}, {Source: neteaseSource, ID: "b"}}
	p.PlayTrack(queue, 1)
	s := awaitPlayback(t, p, func(s PlaybackSnapshot) bool { return s.Error != "" && !s.Playing })
	if s.Current.ID != "a" || requests.Load() != 6 {
		t.Fatalf("expected one pass through both tracks: %s, %d requests", s.Current.ID, requests.Load())
	}
}

// The compatible decoder opens a lower-quality NetEase file at its reported quality.
func TestNetEaseCompatiblePlaysLowerQuality(t *testing.T) {
	data, err := os.ReadFile("testdata/seek.mp3")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write(data) }))
	defer srv.Close()
	src := &neteaseTestSource{controllerSource{request: func(_ string, quality string) (*music.Stream, error) {
		if quality != "high" {
			return nil, music.ErrUnavailable
		}
		return &music.Stream{URL: srv.URL, Type: "mp3", Size: int64(len(data)), Quality: "high"}, nil
	}}}
	info, s, err := openNetEaseCompatible(t.Context(), NewCache(t.TempDir(), 0), src, music.Track{ID: "a", Duration: 5000}, nil)
	if err != nil {
		t.Fatal(err)
	}
	dl, _ := s.source()
	path := dl.part
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if info.Quality != "high" || info.Type != "mp3" || path == "" {
		t.Fatalf("wrong fallback or file lease: %+v, %q", info, path)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("temporary fallback was not released: %v", err)
	}
}

// Cancelling a load stops the HTTP body and removes its partial file.
func TestNetEaseFLACCancelsDownload(t *testing.T) {
	started := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("fLaC"))
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
	}))
	defer srv.Close()
	src := &neteaseTestSource{controllerSource{request: func(string, string) (*music.Stream, error) {
		return &music.Stream{URL: srv.URL, Type: "flac", Quality: "lossless"}, nil
	}}}
	cache := NewCache(t.TempDir(), 1<<20)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		f, _, err := cache.openNetEaseQuality(ctx, src, "a", "lossless", true)
		f.release()
		result <- err
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("download did not start")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancel did not stop the download")
	}
	if entries, err := os.ReadDir(cache.dir); err != nil || len(entries) != 0 {
		t.Fatalf("partial download left behind: %v %v", entries, err)
	}
}
