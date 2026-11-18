package app

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"

	"mygo-music/internal/downloads"
	"mygo-music/internal/music"
	"mygo-music/internal/plugin"
)

const downloadsID = "media-downloads"
const qbSecretKey = "media-downloads/qbittorrent"
const qbLocalURL = "http://127.0.0.1:8080"

type downloadSource struct{ source music.Source }

// Transfers own their data on the queue worker. The controls below belong to
// the main thread. The atomics describe which surfaces need redraws.
type downloadWorkspace struct {
	queue   *downloads.Queue
	engine  *downloads.Engine
	source  atomic.Pointer[downloadSource]
	enabled atomic.Bool
	visible atomic.Bool
	posted  atomic.Bool
	err     string

	provider downloads.Provider
	kind     music.SearchKind
	format   downloads.Format
	folder   string
	query    string
	search   ui.Handle

	searching bool
	submitted bool
	searchErr string
	cancel    context.CancelFunc
	revision  uint64
	last      downloads.Search
	items     []downloads.Item
	more      bool
	results   ui.ListState
	chosen    map[int]*bool
	group     string
	parent    *downloads.Results

	torrentList   ui.ListState
	selecting     string
	fileSelection map[int]*bool
	labelsCancel  context.CancelFunc

	qbURL, qbUser, qbPassword            string
	qbURLInput, qbUserInput, qbPassInput ui.Handle
	qbOpen                               bool
	qbChecked, qbChecking                bool
	qbStatus                             string
}

func (a *app) addBundledDownloads() {
	for _, l := range a.allLibs {
		if l.id == downloadsID {
			return
		}
	}
	dir, err := mygo.App.Path(mygo.PathResources)
	if err != nil {
		return
	}
	p, err := plugin.Load(filepath.Join(dir, downloadsID), userDataDir("plugin-data"))
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			log.Printf("loading downloads workspace: %v", err)
		}
		return
	}
	lib := newLibrary(p)
	lib.bundled = true
	a.plugins = append(a.plugins, p)
	a.allLibs = append(a.allLibs, lib)
}

func (a *app) refreshDownloadSources() {
	d := a.downloads
	lib := a.libOf(downloadsID)
	d.enabled.Store(lib != nil && lib.downloads)
	src := a.sourceByID("netease")
	if src == nil {
		d.source.Store(nil)
	} else {
		d.source.Store(&downloadSource{src})
	}
}

func (d *downloadWorkspace) netEase() music.Source {
	if src := d.source.Load(); src != nil {
		return src.source
	}
	return nil
}

func (a *app) initDownloads() {
	d := a.downloads
	resources, _ := mygo.App.Path(mygo.PathResources)
	dir := a.settings.DownloadFolder
	if !filepath.IsAbs(dir) {
		folder, _ := mygo.App.Path(mygo.PathDownloads)
		if filepath.IsAbs(folder) {
			dir = filepath.Join(folder, "MyGo Music")
		} else {
			dir = ""
		}
	}
	d.folder, d.qbURL = dir, qbLocalURL
	d.setProvider(downloads.NetEase)
	cache := a.pl.Cache
	d.engine = &downloads.Engine{
		ToolDir: filepath.Join(resources, "download-tools"),
		NetEase: func(ctx context.Context, r downloads.Request) (downloads.NetEaseMedia, error) {
			if r.Format == downloads.Lossless {
				if f, st := cache.OpenNetEaseDownloadCache(ctx, r.ID); f != nil {
					return downloads.NetEaseMedia{Stream: st, File: f}, nil
				}
			}
			st, err := downloads.ResolveNetEase(ctx, d.netEase(), r.ID, r.Format)
			return downloads.NetEaseMedia{Stream: st}, err
		},
		BitTorrent: func(ctx context.Context) (*downloads.QBClient, error) {
			conf, err := loadQBConfig()
			if err != nil {
				return nil, err
			}
			return downloads.ConnectQB(ctx, conf)
		},
	}
	win := a.win
	queuePath := userDataDir("downloads")
	q, err := downloads.Open(filepath.Join(queuePath, "queue.json"), d.engine, func() bool {
		return !d.enabled.Load() || a.pl.DownloadPriorityBusy()
	}, func() {
		if d.visible.Load() && d.posted.CompareAndSwap(false, true) {
			win.Update(func() { d.posted.Store(false) })
		}
	})
	if err != nil {
		d.err = Tf("Could not open the download queue (%s)", err)
		return
	}
	d.queue = q
}

func (a *app) stopDownloads() {
	d := a.downloads
	d.visible.Store(false)
	d.stopSearch()
	if d.labelsCancel != nil {
		d.labelsCancel()
	}
	if d.queue != nil {
		d.queue.Close()
	}
}

func (d *downloadWorkspace) stopSearch() {
	if d.cancel != nil {
		d.cancel()
		d.cancel = nil
	}
	d.revision++
	d.searching = false
}

func (d *downloadWorkspace) setProvider(provider downloads.Provider) {
	d.stopSearch()
	d.qbOpen = false
	d.provider, d.kind, d.format = provider, downloads.Tracks, downloads.Original
	switch provider {
	case downloads.NetEase, downloads.Nyaa:
		d.format = downloads.Lossless
	case downloads.YouTube, downloads.Bilibili:
		d.format = downloads.Audio
	}
	d.items, d.chosen, d.parent = nil, nil, nil
	d.results = ui.ListState{}
	d.group, d.searchErr, d.submitted, d.more = "", "", false, false
}

func (a *app) findDownloads(more bool) {
	d := a.downloads
	if d.engine == nil || d.searching {
		return
	}
	req := downloads.Search{Provider: d.provider, Query: strings.TrimSpace(d.query), Kind: d.kind}
	if more {
		req = d.last
		req.Offset = len(d.items)
	} else {
		d.items, d.chosen, d.parent = nil, nil, nil
		d.group, d.more = "", false
		d.results = ui.ListState{}
	}
	d.last = req
	d.submitted = true
	source := d.netEase()
	a.downloadSearch(func(ctx context.Context) (downloads.Results, error) {
		return d.engine.Search(ctx, req, source)
	}, func(r downloads.Results) {
		d.setResults(r, more)
	})
}

func (a *app) expandDownloadGroup(item downloads.Item) {
	d := a.downloads
	source := d.netEase()
	a.downloadSearch(func(ctx context.Context) (downloads.Results, error) {
		t, err := downloads.ExpandNetEase(ctx, source, item)
		return downloads.Results{Items: t}, err
	}, func(r downloads.Results) {
		d.parent = &downloads.Results{Items: d.items, More: d.more}
		d.group = item.Title
		d.setResults(r, false)
	})
}

func (a *app) downloadSearch(run func(context.Context) (downloads.Results, error), apply func(downloads.Results)) {
	d := a.downloads
	d.stopSearch()
	revision := d.revision
	ctx, cancel := context.WithCancel(context.Background())
	d.cancel, d.searching, d.searchErr = cancel, true, ""
	go func() {
		defer cancel()
		r, err := run(ctx)
		a.post(func() {
			if d.revision != revision {
				return
			}
			d.searching, d.cancel = false, nil
			if err != nil {
				d.searchErr = T(err.Error())
				return
			}
			apply(r)
		})
	}()
}

func (d *downloadWorkspace) setResults(r downloads.Results, appendPage bool) {
	if !appendPage {
		d.items = nil
		d.chosen = map[int]*bool{}
		d.results = ui.ListState{}
	}
	for _, item := range r.Items {
		if !item.Unavailable && item.Group == "" {
			d.chosen[len(d.items)] = new(bool)
		}
		d.items = append(d.items, item)
	}
	d.more = r.More && len(r.Items) > 0
}

func (a *app) enqueueDownloads() {
	d := a.downloads
	if d.queue == nil || !filepath.IsAbs(d.folder) {
		return
	}
	var requests []downloads.Request
	for i, item := range d.items {
		if check := d.chosen[i]; check != nil && *check {
			requests = append(requests, downloads.Request{Item: item, Folder: d.folder, Format: d.format})
		}
	}
	n, err := d.queue.Enqueue(requests)
	if err != nil {
		a.toast = T(err.Error())
		return
	}
	if n == 0 {
		a.toast = T("These items are already in the queue")
		return
	}
	a.toast = Tf("Added %d downloads", n)
	setChecks(d.chosen, false)
}

func (a *app) refreshDownloadLabels() {
	d := a.downloads
	if d.queue == nil || d.engine == nil || d.labelsCancel != nil {
		return
	}
	q, engine := d.queue, d.engine
	tasks, _ := q.Snapshot()
	var ids []string
	for _, t := range tasks {
		if t.Item.NeedsNetEaseLabels() {
			ids = append(ids, t.IDForSource())
		}
	}
	if len(ids) == 0 {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	d.labelsCancel = cancel
	go func() {
		defer cancel()
		items := engine.NetEaseLabels(ctx, ids)
		if ctx.Err() == nil {
			_ = q.UpdateNetEaseLabels(items)
		}
		a.post(func() { d.labelsCancel = nil })
	}()
}

func (a *app) chooseTorrentFile() {
	go func() {
		files, err := mygo.Dialog.Open(mygo.OpenDialogOptions{Parent: a.win, Title: T("Open .torrent File"),
			Filters: []mygo.FileFilter{{Name: "BitTorrent", Extensions: []string{"torrent"}}}})
		if err == nil && len(files) > 0 {
			a.post(func() {
				if a.downloads.provider != downloads.Nyaa {
					return
				}
				a.downloads.query = files[0]
				a.findDownloads(false)
			})
		}
	}()
}

func loadQBConfig() (downloads.QBConfig, error) {
	b, err := mygo.Secrets.Get(qbSecretKey)
	var c downloads.QBConfig
	if err != nil || len(b) == 0 || json.Unmarshal(b, &c) != nil {
		return c, errors.New("Connect a local qBittorrent Web UI first")
	}
	return c, nil
}

func (a *app) enterQBSettings() {
	d := a.downloads
	if d.qbChecked {
		return
	}
	d.qbChecked = true
	go func() {
		conf, err := loadQBConfig()
		if err == nil {
			a.post(func() {
				if d.qbURL == qbLocalURL && d.qbUser == "" && d.qbPassword == "" && !d.qbChecking {
					d.qbURL, d.qbUser = conf.URL, conf.Username
				}
			})
		}
	}()
}

func (a *app) connectQB() {
	d := a.downloads
	c := downloads.QBConfig{URL: strings.TrimSpace(d.qbURL), Username: strings.TrimSpace(d.qbUser), Password: d.qbPassword}
	d.qbChecking, d.qbStatus = true, ""
	go func() {
		if c.Password == "" {
			if last, err := loadQBConfig(); err == nil && last.URL == c.URL && last.Username == c.Username {
				c.Password = last.Password
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 9*time.Second)
		defer cancel()
		_, err := downloads.ConnectQB(ctx, c)
		message := "qBittorrent connection saved"
		if err != nil {
			message = err.Error()
		} else {
			b, _ := json.Marshal(c)
			if err := mygo.Secrets.Set(qbSecretKey, b); err != nil {
				message = "Could not save the qBittorrent connection"
			}
		}
		a.post(func() {
			d.qbChecking, d.qbStatus = false, T(message)
			d.qbPassword = ""
		})
	}()
}
