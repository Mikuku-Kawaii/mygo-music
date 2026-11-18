package local

import (
	"crypto/sha256"
	"encoding/json"
	pcmfixture "mygo-music/internal/audioengine/fixture"
	"mygo-music/internal/audioengine/pcm"
	"mygo-music/internal/music"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Native header observations survive scanning/index reload while ordinary
// search, likes and playlists retain identity and never change source bytes.
func TestNativeCatalogProperties(t *testing.T) {
	dir := t.TempDir()
	data := t.TempDir()
	path := filepath.Join(dir, "测试 - Native.wav")
	f := pcm.Format{Kind: pcm.Integer, Rate: 44100, Channels: 2, ChannelMask: 3, ValidBits: 24, ContainerBits: 24}
	if err := pcmfixture.WriteWAV(path, f, make([]int32, 88200)); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	s := New(data)
	s.AddFolders(dir)
	wait(t, s)
	q := songs(t, s)
	if len(q) != 1 {
		t.Fatal(q)
	}
	track := q[0]
	a := track.Audio
	if !a.Known || a.Rate != 44100 || a.ValidBits != 24 || a.Frames != 44100 || !a.NativePrecision || !a.ExactSeek || !a.Trimmed {
		t.Fatal(a)
	}
	search, err := s.Search("Native", music.SearchTracks, 0)
	if err != nil || len(search.Tracks) != 1 || search.Tracks[0].ID != track.ID {
		t.Fatal(search, err)
	}
	if err = s.Like(track.ID, true); err != nil {
		t.Fatal(err)
	}
	list, err := s.CreatePlaylist("M1")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.AddToPlaylist(list.ID, []string{track.ID}); err != nil {
		t.Fatal(err)
	}
	s.Played(track)
	s.Completed(track, time.Second)
	again := New(data)
	got, err := again.Playlist(list.ID)
	if err != nil || len(got.Tracks) != 1 || got.Tracks[0].Audio != a {
		t.Fatal(got, err)
	}
	likes, err := again.IsLiked([]string{track.ID})
	if err != nil || !likes[track.ID] {
		t.Fatal(likes, err)
	}
	if c := again.Completions(); len(c) != 1 || c[0].Position != 1000 || c[0].ID != track.ID {
		t.Fatal(c)
	}
	after, _ := os.ReadFile(path)
	if sha256.Sum256(before) != sha256.Sum256(after) {
		t.Fatal("source changed")
	}
	b, _ := json.Marshal(track)
	var wire map[string]any
	json.Unmarshal(b, &wire)
	if _, ok := wire["Audio"]; ok {
		t.Fatal("local metadata leaked into plugin wire schema")
	}
}

// A version-4 catalog retains its visible entries and refreshes existing Ogg headers on the next scan.
func TestLegacyCatalogRefreshesOgg(t *testing.T) {
	dir := t.TempDir()
	s := New(t.TempDir())
	ogg := entry{Path: filepath.Join(dir, "old.OGG"), Size: 123, Mod: 100, Title: "Visible Ogg"}
	wav := entry{Path: filepath.Join(dir, "old.wav"), Size: 234, Mod: 200, Title: "Visible WAV"}
	b, err := json.Marshal(indexFile{Version: 4, Tracks: []entry{ogg, wav}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.indexPath(), b, 0600); err != nil {
		t.Fatal(err)
	}
	items := s.loadIndex([]string{dir})
	ogg.Mod = 0
	if len(items) != 2 || items[0] != ogg || items[1] != wav {
		t.Fatal(items)
	}
}
