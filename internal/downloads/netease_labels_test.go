package downloads

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mygo-music/internal/music"
)

type labelTransport func(*http.Request) (*http.Response, error)

func (f labelTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// A song link exposes the returned credits, without sending cookies or using
// the music source to resolve an audio stream.
func TestNetEaseLinkUsesPublicCredits(t *testing.T) {
	body := `{"code":200,"songs":[{"id":123,"name":"Track Title","artists":[{"name":"Artist One"},{"name":"Artist Two"}],
		"duration":328020,"album":{"name":"Release","picUrl":"http://p1.music.126.net/cover.jpg"}},
		{"id":999,"name":"Unrequested song"}]}`
	engine := &Engine{HTTP: &http.Client{Transport: labelTransport(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host != "music.163.com" || req.URL.Path != "/api/song/detail/" || req.URL.Query().Get("ids") != "[123]" ||
			req.Header.Get("Cookie") != "" || req.Header.Get("Authorization") != "" {
			t.Fatalf("unexpected public lookup %s", req.URL)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}}
	search := Search{Provider: NetEase, Query: "https://music.163.com/#/song?id=123", Kind: Tracks}
	source := struct{ music.Source }{}
	results, err := engine.Search(t.Context(), search, source)
	if err != nil || len(results.Items) != 1 {
		t.Fatalf("link search: %+v, %v", results, err)
	}
	item := results.Items[0]
	if item.ID != "123" || item.Title != "Track Title" || item.Detail != "Artist One, Artist Two" ||
		item.ImageURL != "https://p1.music.126.net/cover.jpg" || item.Duration != 328020 {
		t.Fatalf("wrong credits: %+v", item)
	}
	engine.HTTP = &http.Client{Transport: labelTransport(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("offline")
	})}
	results, err = engine.Search(t.Context(), search, source)
	if err != nil || len(results.Items) != 1 || results.Items[0].ID != "123" || results.Items[0].Unavailable {
		t.Fatalf("metadata outage blocked the song link: %+v, %v", results, err)
	}
}

// The journal keeps the source song ID separate from its own ID, including
// when an older link-based task only has the song ID in its URL.
func TestNetEaseLabelsAndIdentitySurviveJournalRestart(t *testing.T) {
	folder := t.TempDir()
	path := filepath.Join(t.TempDir(), "queue.json")
	encodedFolder, _ := json.Marshal(folder)
	legacy := `[{"id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","provider":"netease","state":"Completed","cleaned":true,
		"title":"NetEase 123","url":"https://music.163.com/song?id=123","folder":` + string(encodedFolder) +
		`,"format":"lossless","artifacts":[{"name":"NetEase 123.flac","size":420,"sha256":"fixture"}]}]`
	if err := os.WriteFile(path, []byte(legacy), 0600); err != nil {
		t.Fatal(err)
	}
	held := func() bool { return true }
	q, err := Open(path, &Engine{}, held, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	req := Request{Item: Item{Provider: NetEase, ID: "456", Title: "Queued song", Detail: "Artist"}, Folder: folder, Format: Lossless}
	if n, err := q.Enqueue([]Request{req}); n != 1 || err != nil {
		t.Fatalf("enqueue: %d, %v", n, err)
	}
	if err := q.UpdateNetEaseLabels(map[string]Item{"123": {Title: "Track Title", Detail: "Artist One"}}); err != nil {
		t.Fatal(err)
	}
	q.Close()
	reopened, err := Open(path, &Engine{}, held, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	tasks, err := reopened.Snapshot()
	if err != nil || len(tasks) != 2 {
		t.Fatalf("reopened: %+v, %v", tasks, err)
	}
	old, newJob := tasks[0], tasks[1]
	if old.IDForSource() != "123" || old.Title != "Track Title" || old.Detail != "Artist One" || old.State != Complete ||
		len(old.Artifacts) != 1 || old.Artifacts[0].Name != "NetEase 123.flac" || old.Artifacts[0].SHA256 != "fixture" {
		t.Fatalf("legacy archive changed: %+v", old)
	}
	if newJob.IDForSource() != "456" || newJob.ID == "456" || newJob.State != Queued {
		t.Fatalf("song identity was lost: %+v", newJob)
	}
}
