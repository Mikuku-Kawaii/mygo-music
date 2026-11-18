package downloads

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"mygo-music/internal/audioengine/fixture"
	"mygo-music/internal/audioengine/pcm"
	"mygo-music/internal/music"
)

func flacFixture(t *testing.T, phase int) []byte {
	t.Helper()
	p := filepath.Join(t.TempDir(), "source.flac")
	f := pcm.Format{Kind: pcm.Integer, Rate: 44100, Channels: 2, ChannelMask: 3, ValidBits: 24, ContainerBits: 24}
	if err := fixture.WriteFLAC(p, f, fixture.Samples(24, phase, 8192)); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func pollTask(t *testing.T, q *Queue, id string, want State) Task {
	t.Helper()
	var last Task
	for until := time.Now().Add(8 * time.Second); time.Now().Before(until); {
		ts, err := q.Snapshot()
		if err != nil {
			t.Fatal(err)
		}
		for _, task := range ts {
			if task.ID == id || id == "" {
				last = task
				if task.State == want {
					return task
				}
				if task.State == Failed && want != Failed {
					t.Fatalf("task failed: %s", task.Error)
				}
			}
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatalf("wanted %s, got %s (%s)", want, last.State, last.Error)
	return Task{}
}

func TestHTTPSQueueResumeAfterRestart(t *testing.T) {
	v1, v2 := flacFixture(t, 1), flacFixture(t, 9)
	for _, tc := range []struct {
		name   string
		change bool
	}{{"same entity", false}, {"validator changed", true}} {
		t.Run(tc.name, func(t *testing.T) {
			folder := t.TempDir()
			journal := filepath.Join(t.TempDir(), "queue.json")
			var calls atomic.Int32
			part := len(v1) / 2
			writerStopped := make(chan struct{})
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "audio/flac")
				switch calls.Add(1) {
				case 1:
					w.Header().Set("ETag", `"v1"`)
					w.Header().Set("Content-Length", fmt.Sprint(len(v1)))
					w.Write(v1[:part])
					w.(http.Flusher).Flush()
					<-r.Context().Done()
					close(writerStopped)
				case 2:
					if r.Header.Get("Range") != fmt.Sprintf("bytes=%d-", part) || r.Header.Get("If-Range") != `"v1"` {
						t.Errorf("resume headers = %v", r.Header)
					}
					payload, etag := v1, `"v1"`
					if tc.change {
						payload, etag = v2, `"v2"`
					}
					w.Header().Set("ETag", etag)
					w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", part, len(payload)-1, len(payload)))
					w.WriteHeader(http.StatusPartialContent)
					w.Write(payload[part:])
				default:
					if !tc.change || r.Header.Get("Range") != "" {
						t.Errorf("unexpected retry: %v", r.Header)
					}
					w.Header().Set("ETag", `"v2"`)
					w.Write(v2)
				}
			}))
			defer server.Close()
			e := &Engine{HTTP: server.Client()}
			var playback atomic.Bool
			q, err := Open(journal, e, playback.Load, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer q.Close()
			req := Request{Item: Item{Provider: HTTPS, Title: "Test recording", URL: server.URL + "/file.flac"}, Folder: folder, Format: Original}
			if n, err := q.Enqueue([]Request{req, req}); n != 1 || err != nil {
				t.Fatalf("enqueued %d, %v", n, err)
			}
			active := pollTask(t, q, "", Active)
			p := filepath.Join(folder, active.stageDir(), "transfer.part")
			until := time.Now().Add(5 * time.Second)
			for {
				info, err := os.Stat(p)
				if err == nil && info.Size() == int64(part) {
					break
				}
				if time.Now().After(until) {
					t.Fatalf("partial file not written: %v", err)
				}
				time.Sleep(15 * time.Millisecond)
			}
			if !tc.change {
				playback.Store(true)
				pollTask(t, q, active.ID, Queued)
			}
			q.Close()
			select {
			case <-writerStopped:
			case <-time.After(3 * time.Second):
				t.Fatal("request survived shutdown")
			}
			q2, err := Open(journal, e, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer q2.Close()
			task := pollTask(t, q2, active.ID, Complete)
			got, err := os.ReadFile(filepath.Join(folder, task.Artifacts[0].Name))
			want := v1
			if tc.change {
				want = v2
			}
			if err != nil || !bytes.Equal(got, want) {
				t.Fatalf("saved %d bytes, want %d (%v)", len(got), len(want), err)
			}
			if calls.Load() != 2 && !tc.change || calls.Load() != 3 && tc.change {
				t.Errorf("requests=%d", calls.Load())
			}
		})
	}
}

func TestArchiveAlreadyPublishedAtRestart(t *testing.T) {
	folder := t.TempDir()
	journal := filepath.Join(t.TempDir(), "queue.json")
	task := Task{ID: strings.Repeat("0", 32), State: Active,
		Request: Request{Item: Item{Provider: HTTPS, URL: "https://example.test/sample.flac", Title: "sample"}, Folder: folder}}
	want := flacFixture(t, 4)
	final := "sample.flac"
	if err := os.WriteFile(filepath.Join(folder, final), want, 0600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(folder)
	if err != nil {
		t.Fatal(err)
	}
	digest, size, err := fingerprint(t.Context(), root, final)
	root.Close()
	if err != nil {
		t.Fatal(err)
	}
	task.Artifacts = []Artifact{{From: filepath.Join(task.stageDir(), "transfer.part"), Name: final, Size: size, SHA256: digest}}
	if err := saveTasks(journal, []Task{task}); err != nil {
		t.Fatal(err)
	}
	e := &Engine{HTTP: &http.Client{Transport: rejectMediaRequest{t}}}
	q, err := Open(journal, e, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	got := pollTask(t, q, task.ID, Complete)
	if len(got.Artifacts) != 1 || got.Artifacts[0].Name != final {
		t.Fatalf("existing archive: %+v", got.Artifacts)
	}
}

type rejectMediaRequest struct{ t *testing.T }

func (r rejectMediaRequest) RoundTrip(*http.Request) (*http.Response, error) {
	r.t.Error("an already archived download went back to the source")
	return nil, io.ErrUnexpectedEOF
}

func TestNetEaseBatchPlaybackPriorityAndArchive(t *testing.T) {
	b := flacFixture(t, 1)
	folder := t.TempDir()
	const oldFile = "user's existing file"
	if err := os.WriteFile(filepath.Join(folder, "same name.flac"), []byte(oldFile), 0600); err != nil {
		t.Fatal(err)
	}
	var requested atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Referer") != "https://music.163.com/" {
			t.Error("source header missing")
		}
		if r.URL.Path == "/2.flac" {
			v := bytes.Clone(b)
			v[0] = 'x'
			w.Write(v)
			return
		}
		w.Write(b)
	}))
	defer server.Close()
	e := &Engine{HTTP: server.Client(), NetEase: func(_ context.Context, r Request) (NetEaseMedia, error) {
		requested.Add(1)
		if r.Format != Lossless {
			t.Error("lossless format was lost")
		}
		return NetEaseMedia{Stream: &music.Stream{URL: server.URL + "/" + r.Item.ID + ".flac", Size: int64(len(b)),
			Type: "flac", Quality: "lossless", Headers: map[string]string{"Referer": "https://music.163.com/"}}}, nil
	}}
	var playing atomic.Bool
	playing.Store(true)
	journal := filepath.Join(t.TempDir(), "queue.json")
	q, err := Open(journal, e, playing.Load, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	req := func(id string) Request {
		return Request{Item: Item{Provider: NetEase, ID: id, Title: "same name"}, Folder: folder, Format: Lossless}
	}
	if n, err := q.Enqueue([]Request{req("1"), req("2")}); n != 2 || err != nil {
		t.Fatalf("added %d: %v", n, err)
	}
	tasks, _ := q.Snapshot()
	time.Sleep(240 * time.Millisecond)
	if requested.Load() != 0 {
		t.Fatal("a batch transfer ran while the playback gate was held")
	}
	playing.Store(false)
	first := pollTask(t, q, tasks[0].ID, Complete)
	pollTask(t, q, tasks[1].ID, Failed)
	saved, err := os.ReadFile(filepath.Join(folder, first.Artifacts[0].Name))
	if err != nil || !bytes.Equal(saved, b) || first.Artifacts[0].Name != "same name (2).flac" {
		t.Fatalf("archive: %+v, %v", first.Artifacts, err)
	}
	orig, err := os.ReadFile(filepath.Join(folder, "same name.flac"))
	if err != nil || string(orig) != oldFile {
		t.Fatalf("existing file changed: %q, %v", orig, err)
	}
	data, err := os.ReadFile(journal)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte(server.URL)) || bytes.Contains(data, []byte("Referer")) {
		t.Fatal("resolved NetEase URL or headers were persisted")
	}
}

func TestQBitSelectsLosslessBeforeStarting(t *testing.T) {
	payload := flacFixture(t, 2)
	folder := t.TempDir()
	var job qbTorrent
	const hash = "0123456789012345678901234567890123456789"
	var state sync.Mutex
	files := []qbFile{
		{TorrentFile: TorrentFile{Index: 0, Name: "Album/top.flac", Size: int64(len(payload))}, Priority: 1},
		{TorrentFile: TorrentFile{Index: 1, Name: "Album/other.flac", Size: 150}, Priority: 1},
		{TorrentFile: TorrentFile{Index: 2, Name: "Album/cover.jpg", Size: 100}, Priority: 1},
	}
	var started atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		state.Lock()
		defer state.Unlock()
		switch strings.TrimPrefix(r.URL.Path, "/api/v2/") {
		case "auth/login":
			if r.FormValue("username") != "fixture" || r.FormValue("password") != "fixture" {
				http.Error(w, "bad login", 401)
				return
			}
			http.SetCookie(w, &http.Cookie{Name: "SID", Value: "fixture", Path: "/"})
			w.WriteHeader(http.StatusOK)
		case "app/version":
			if sid, err := r.Cookie("SID"); err != nil || sid.Value != "fixture" {
				http.Error(w, "missing session", 403)
				return
			}
			io.WriteString(w, "v5.3.0")
		case "torrents/info":
			if job.Hash == "" {
				io.WriteString(w, "[]")
				return
			}
			if r.URL.Query().Get("tag") != job.Tags {
				t.Error("ownership query lost tag")
			}
			json.NewEncoder(w).Encode([]qbTorrent{job})
		case "torrents/add":
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Error(err)
				http.Error(w, "bad form", 400)
				return
			}
			if r.FormValue("stopped") != "true" || r.FormValue("stopCondition") != "MetadataReceived" {
				t.Error("torrent payload must remain stopped")
			}
			job = qbTorrent{Hash: hash, Tags: r.FormValue("tags"), SavePath: r.FormValue("savepath"), State: "stoppedDL"}
			fmt.Fprintf(w, `{"success_count":1,"added_torrent_ids":[%q]}`, hash)
		case "torrents/files":
			json.NewEncoder(w).Encode(files)
		case "torrents/filePrio":
			if r.FormValue("hash") != hash || r.FormValue("id") != "1|2" || r.FormValue("priority") != "0" || job.State != "stoppedDL" {
				t.Errorf("bad pre-start file choice: %v", r.Form)
				http.Error(w, "bad priorities", 400)
				return
			}
			files[1].Priority, files[2].Priority = 0, 0
		case "torrents/start":
			started.Add(1)
			if r.FormValue("hashes") != hash || files[0].Priority != 1 || files[1].Priority != 0 || files[2].Priority != 0 {
				t.Error("qBittorrent started before the selected priorities were applied")
				http.Error(w, "bad start", 400)
				return
			}
			path := filepath.Join(job.SavePath, "Album", "top.flac")
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Error(err)
				return
			}
			if err := os.WriteFile(path, payload, 0600); err != nil {
				t.Error(err)
				return
			}
			files[0].Progress = 1
			job.State = "uploading"
		case "torrents/stop":
			if r.FormValue("hashes") != hash {
				t.Error("unexpected removal target")
			}
			job.State = "stoppedDL"
		case "torrents/delete":
			if r.FormValue("hashes") != hash || r.FormValue("deleteFiles") != "false" {
				t.Error("unexpected deletion")
			}
			job = qbTorrent{}
		default:
			t.Errorf("unexpected qBittorrent request %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	e := &Engine{BitTorrent: func(ctx context.Context) (*QBClient, error) {
		return ConnectQB(ctx, QBConfig{URL: server.URL, Username: "fixture", Password: "fixture"})
	}}
	q, err := Open(filepath.Join(t.TempDir(), "queue.json"), e, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	req := Request{Item: Item{Provider: Nyaa, URL: "https://nyaa.si/download/123.torrent", Title: "Test album"}, Folder: folder, Format: Lossless}
	if n, err := q.Enqueue([]Request{req}); n != 1 || err != nil {
		t.Fatalf("enqueued %d, %v", n, err)
	}
	task := pollTask(t, q, "", ChooseFiles)
	if started.Load() != 0 || len(task.Files) != 2 {
		t.Fatalf("file picker: %+v, started %d", task.Files, started.Load())
	}
	if err := q.Select(task.ID, []int{2}); err == nil {
		t.Fatal("image was selectable")
	}
	if err := q.Select(task.ID, []int{0}); err != nil {
		t.Fatal(err)
	}
	task = pollTask(t, q, task.ID, Complete)
	if len(task.Artifacts) != 1 || task.Artifacts[0].Name != "top.flac" {
		t.Fatalf("archive: %+v", task.Artifacts)
	}
	got, err := os.ReadFile(filepath.Join(folder, "top.flac"))
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("wrong downloaded file (%v)", err)
	}
}
