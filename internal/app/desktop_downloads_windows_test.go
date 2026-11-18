//go:build windows && amd64

package app

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/egoist/mygo"
	"mygo-music/internal/downloads"
)

// The download sidebar routes stay in the original native window and use
// the same persisted queue while the user switches pages.
func TestDesktopDownloadSidebarNavigation(t *testing.T) {
	if os.Getenv("MYGO_TEST_DOWNLOAD_SIDEBAR") != "1" {
		t.Skip("native download sidebar smoke test")
	}
	runtimeDir, err := os.MkdirTemp("", "mygo-dl-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(runtimeDir) })
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	resources, err := filepath.Abs(filepath.Join("..", "..", "resources"))
	if err != nil {
		t.Fatal(err)
	}
	mygo.App.SetPath(mygo.PathResources, resources)
	t.Cleanup(func() { mygo.App.SetPath(mygo.PathResources, "") })
	dir := userDataDir("downloads")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	task := downloads.Task{ID: strings.Repeat("a", 32), State: downloads.Complete, Cleaned: true,
		Request: downloads.Request{Item: downloads.Item{Provider: downloads.HTTPS, Title: "Fixture"}, Folder: t.TempDir()}}
	data, err := json.Marshal([]downloads.Task{task})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "queue.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	verified := false
	shotHook = func(a *app) {
		windows := len(mygo.Windows())
		a.switchSource(downloadsID)
		if a.route.page != pageDownloads || a.downloads.queue == nil {
			t.Error("the download workspace did not open")
			go mygo.App.Quit()
			return
		}
		a.open(route{page: pageDownloadQueue})
		if a.route.page != pageDownloadQueue {
			t.Errorf("queue route opened %+v", a.route)
		}
		a.open(route{page: pageDownloadCompleted})
		a.win.Update(func() {
			tasks, err := a.downloads.queue.Snapshot()
			if a.route.page != pageDownloadCompleted || len(mygo.Windows()) != windows || a.pl.Current() != nil ||
				err != nil || len(tasks) != 1 || tasks[0].ID != task.ID {
				t.Error("download navigation changed the window count, shared queue or player")
			}
			verified = true
			go mygo.App.Quit()
		})
	}
	t.Cleanup(func() { shotHook = nil })
	onMainThread(func() { err = Run() })
	if err != nil || !verified {
		t.Fatalf("desktop sidebar did not complete verification: %v", err)
	}
}
