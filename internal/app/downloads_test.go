package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/egoist/mygo/ui"

	"mygo-music/internal/downloads"
	"mygo-music/internal/i18n"
	"mygo-music/internal/plugin"
)

// Batch selection adds jobs while the workspace stays on its results page.
func TestDownloadWorkspaceInNativeSourceSwitcher(t *testing.T) {
	a := newTestApp()
	source := a.library
	p, err := plugin.Load(filepath.Join("..", "..", "resources", downloadsID), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	lib := addFake(a, p)
	d := a.downloads
	d.engine = &downloads.Engine{}
	d.folder = t.TempDir()
	d.setProvider(downloads.HTTPS)
	d.query = "https://example.test/first.flac\nhttps://example.test/second.wav"
	d.queue, err = downloads.Open(filepath.Join(t.TempDir(), "queue.json"), d.engine, func() bool { return true }, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer d.queue.Close()
	ch := posts(a)
	tt := ui.NewTester(a.view, 880, 580)
	tt.Frame()
	chooseSource(t, tt, lib.name())
	if a.library != lib || a.route.page != pageDownloads || a.settings.Source != downloadsID {
		t.Fatalf("wrong landing: %+v", a.route)
	}
	if err := tt.Click(T("Read Links")); err != nil {
		t.Fatal(err)
	}
	until(t, ch, func() bool { return len(d.items) == 2 })
	tt.Frame()
	if err := tt.Click(T("Select All")); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if err := tt.Click(Tf("Queue Selected (%d)", 2)); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	tasks, err := d.queue.Snapshot()
	if err != nil || len(tasks) != 2 || a.route.page != pageDownloads || countChecks(d.chosen) != 0 {
		t.Fatalf("batch did not retain the search page: %d, %+v, %v", len(tasks), a.route, err)
	}
	chooseSource(t, tt, source.name())
	if a.library != source || a.route.page != pageHome || a.pl.Current() != nil {
		t.Fatalf("switch back: %+v", a.route)
	}
}

// The sidebar routes to the shared queue and completed history, including
// task actions and Back, while the player keeps its transport state.
func TestDownloadSidebarQueueActions(t *testing.T) {
	a := newTestApp()
	source := newFakeSource(downloadsID)
	source.info.Workspace = "downloads"
	source.caps = nil
	addFake(a, source)
	a.switchSource(downloadsID)
	d := a.downloads
	folder := t.TempDir()
	completed := downloads.Task{ID: strings.Repeat("a", 32), State: downloads.Complete, Cleaned: true,
		Request: downloads.Request{Item: downloads.Item{Provider: downloads.NetEase, ID: "123", Title: "A completed song"}, Folder: folder}}
	data, err := json.Marshal([]downloads.Task{completed})
	if err != nil {
		t.Fatal(err)
	}
	journal := filepath.Join(t.TempDir(), "queue.json")
	if err := os.WriteFile(journal, data, 0600); err != nil {
		t.Fatal(err)
	}
	d.queue, err = downloads.Open(journal, &downloads.Engine{}, func() bool { return true }, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer d.queue.Close()
	req := downloads.Request{Item: downloads.Item{Provider: downloads.HTTPS, Title: "Queued audio", URL: "https://example.test/a.flac"}, Folder: folder}
	if _, err := d.queue.Enqueue([]downloads.Request{req}); err != nil {
		t.Fatal(err)
	}

	tt := ui.NewTester(a.view, 880, 580)
	tt.Frame()
	if err := tt.Click(T("Download Queue")); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if a.route.page != pageDownloadQueue {
		t.Fatalf("queue sidebar opened %+v", a.route)
	}
	if err := tt.Click(T("Pause")); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	tasks, _ := d.queue.Snapshot()
	if tasks[1].State != downloads.Paused {
		t.Fatalf("queue action left the job in %s", tasks[1].State)
	}
	var shown string
	reveal := revealFolder
	t.Cleanup(func() { revealFolder = reveal })
	revealFolder = func(folder string) { shown = folder }
	if err := tt.Click(T("Completed")); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if err := tt.Click(T("Show Folder")); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if shown != folder || a.route.page != pageDownloadCompleted || a.pl.Current() != nil {
		t.Fatalf("completed action: shown %q, route %+v", shown, a.route)
	}
	a.back()
	tt.Frame()
	tasks, _ = d.queue.Snapshot()
	if a.route.page != pageDownloadQueue || tasks[1].State != downloads.Paused {
		t.Fatalf("back navigation changed the queue: route %+v, state %s", a.route, tasks[1].State)
	}
	tt.Key(ui.Cmd, ui.KeyF)
	tt.Frame()
	if a.route.page != pageDownloads {
		t.Fatalf("search shortcut opened %+v", a.route)
	}
}

// Nyaa connection settings are reachable without a separate navigation tab,
// and the popover closes when the user changes download source.
func TestDownloadQBitSettingsStayWithNyaa(t *testing.T) {
	a := newTestApp()
	a.route.page = pageDownloads
	d := a.downloads
	d.setProvider(downloads.Nyaa)
	d.qbChecked = true // The test never consults the platform's credentials.
	tt := ui.NewTester(a.view, 880, 580)
	tt.Frame()
	if err := tt.Click(T("qBittorrent Settings")); err != nil {
		t.Fatal(err)
	}
	tt.Frame()
	if !d.qbOpen {
		t.Fatal("Nyaa connection settings did not open")
	}
	d.setProvider(downloads.NetEase)
	tt.Frame()
	if d.qbOpen {
		t.Fatal("connection popover stayed open on another provider")
	}
}

// With MM_FRAMES set, saves the three views of the native downloads workspace.
func TestDownloadViewsFrames(t *testing.T) {
	dir := framesDir(t)
	language := i18n.Language()
	i18n.SetLanguage(i18n.Chinese)
	t.Cleanup(func() { i18n.SetLanguage(language) })
	a := newTestApp()
	src, err := plugin.Load(filepath.Join("..", "..", "resources", downloadsID), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	addFake(a, src)
	a.switchSource(downloadsID)
	d := a.downloads
	d.setProvider(downloads.NetEase)
	d.folder, d.query = t.TempDir(), "极光"
	d.engine = &downloads.Engine{}
	var items []downloads.Item
	names := []string{"极光", "蓝色海面", "一路向北", "纸船", "驶向港湾的最后一班渡轮", "冬天的来信", "暮色里", "雪原"}
	for i, track := range framesTracks(a) {
		items = append(items, downloads.Item{Provider: downloads.NetEase, ID: track.ID, Title: names[i],
			Detail: "极光乐队", Album: "漫长极夜", Duration: track.Duration, ImageURL: track.Album.ImageURL})
	}
	d.setResults(downloads.Results{Items: items}, false)
	*d.chosen[0], *d.chosen[2] = true, true
	complete := downloads.Task{ID: strings.Repeat("a", 32), Request: downloads.Request{Item: items[0], Folder: d.folder},
		State: downloads.Complete, Artifacts: []downloads.Artifact{{Name: "极光.flac", Size: 32260622}}, Cleaned: true}
	pending := downloads.Task{ID: strings.Repeat("b", 32), Request: downloads.Request{Item: items[2], Folder: d.folder},
		State: downloads.Paused}
	journal := filepath.Join(t.TempDir(), "queue.json")
	data, err := json.Marshal([]downloads.Task{complete, pending})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(journal, data, 0600); err != nil {
		t.Fatal(err)
	}
	d.queue, err = downloads.Open(journal, d.engine, func() bool { return true }, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer d.queue.Close()
	main := ui.NewTester(a.view, 1168, 816)
	main.SetDark(true)
	settled(main)
	saveFrame(t, dir, "downloads-search.png", main.Image())
	narrow := ui.NewTester(a.view, 880, 580)
	settled(narrow)
	saveFrame(t, dir, "downloads-narrow.png", narrow.Image())
	a.open(route{page: pageDownloadQueue})
	settled(main)
	saveFrame(t, dir, "downloads-queue.png", main.Image())
	a.open(route{page: pageDownloadCompleted})
	settled(main)
	saveFrame(t, dir, "downloads-completed.png", main.Image())
}
