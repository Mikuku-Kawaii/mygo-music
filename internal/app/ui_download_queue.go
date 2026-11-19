package app

import (
	"fmt"
	"slices"
	"strings"

	"github.com/egoist/mygo/ui"

	"mygo-music/internal/downloads"
)

func (a *app) downloadQueuePage(c *ui.Context, completed bool) {
	d, p := a.downloads, a.pal
	title := T("Download Queue")
	if completed {
		title = T("Completed")
	}
	a.downloadPageFrame(c, title, func() {
		if d.queue == nil {
			if d.err != "" {
				ui.Text(c, d.err).FontSize(13).TextColor(p.accent).MaxLines(3)
			}
			return
		}
		tasks, err := d.queue.Snapshot()
		if err != nil {
			ui.Text(c, T(err.Error())).FontSize(13).TextColor(p.accent).MaxLines(3)
		}
		if !completed && d.selecting != "" {
			i := slices.IndexFunc(tasks, func(t downloads.Task) bool { return t.ID == d.selecting && t.State == downloads.ChooseFiles })
			if i >= 0 {
				a.downloadTorrentPicker(c.Key("torrent-files"), p, tasks[i])
				return
			}
			d.selecting, d.fileSelection = "", nil
		}
		tasks = slices.DeleteFunc(tasks, func(task downloads.Task) bool { return (task.State == downloads.Complete) != completed })
		empty := T("The download queue is empty")
		if completed {
			slices.Reverse(tasks)
			empty = T("No completed downloads")
		}
		if len(tasks) == 0 {
			ui.Column(c).Grow(1).Center().Children(func() {
				ui.Text(c, empty).FontSize(14).TextColor(p.muted)
			})
			return
		}
		if !completed && a.pl != nil && a.pl.DownloadPriorityBusy() &&
			slices.ContainsFunc(tasks, func(t downloads.Task) bool { return t.State == downloads.Active || t.State == downloads.Queued }) {
			ui.Text(c, T("Downloads resume after audio loading or prefetching finishes.")).FontSize(12.5).TextColor(p.muted).MaxLines(2)
		}
		ui.List(c.Key(a.route), &a.trackList, len(tasks)).Grow(1).MinHeight(0).
			ItemKey(func(i int) any { return tasks[i].ID }).Rows(func(row ui.ListRow) {
			a.downloadTaskRow(row.Context, p, tasks[row.Index])
		})
	})
}

func (a *app) downloadTaskRow(c *ui.Context, p palette, t downloads.Task) {
	status := T(string(t.State))
	if t.State == downloads.Active && t.Stage != "" {
		status = T(t.Stage)
	}
	if t.Bytes > 0 && (t.Stage == "Downloading" || t.State == downloads.Queued || t.State == downloads.Paused) {
		status += " · " + downloadBytes(t.Bytes)
		if t.Total > 0 {
			status += " / " + downloadBytes(t.Total)
		}
		if t.Rate > 0 {
			status += " · " + downloadBytes(t.Rate) + "/s"
		}
	}
	details := []string{t.Detail, downloadProviderName(t.Provider), status}
	if t.State == downloads.Complete {
		details = details[:2]
		var total int64
		for _, f := range t.Artifacts {
			total += f.Size
		}
		if total > 0 {
			details = append(details, downloadBytes(total))
		}
	}
	details = slices.DeleteFunc(details, func(s string) bool { return s == "" })
	ui.Column(c).Gap(6).Padding(7, 9).Children(func() {
		ui.Row(c).Height(44).Gap(11).AlignItems(ui.Center).Children(func() {
			a.cover(c, t.ImageURL, 96, 38, 6)
			downloadItemTitle(c, p, t.Title, strings.Join(details, " · "))
			a.downloadTaskControls(c.Key("actions"), p, t)
		})
		if t.State == downloads.Active && t.Total > 0 {
			ui.Progress(c, float64(t.Bytes)/float64(t.Total)).Label(status)
		}
		if t.Error != "" {
			ui.Text(c, T(t.Error)).FontSize(12.5).TextColor(p.accent).MaxLines(3)
		}
		ui.Box(c).Height(1).Background(p.hairline)
	})
}

func (a *app) downloadTaskControls(c *ui.Context, p palette, t downloads.Task) {
	d := a.downloads
	action := func(key, label string, fn func(string) error) {
		ui.Button(c.Key(key), T(label)).Height(28).Shrink(0).OnClick(func() { a.downloadTaskAction(fn(t.ID)) })
	}
	switch t.State {
	case downloads.Active, downloads.Queued:
		action("pause", "Pause", d.queue.Pause)
	case downloads.Paused:
		action("resume", "Resume Download", d.queue.Resume)
	case downloads.Failed:
		action("retry", "Retry", d.queue.Resume)
	case downloads.ChooseFiles:
		ui.PrimaryButton(c.Key("pick"), T("Select Lossless Files")).Height(28).Shrink(0).OnClick(func() {
			d.selecting = t.ID
			d.torrentList = ui.ListState{}
			d.fileSelection = make(map[int]*bool, len(t.Files))
			for _, f := range t.Files {
				d.fileSelection[f.Index] = new(bool)
			}
		})
	case downloads.Complete:
		ui.Button(c.Key("show"), T("Show Folder")).Height(28).Shrink(0).
			Tooltip(t.Folder).OnClick(func() { revealFolder(t.Folder) })
	}
	if !t.Cleaned && (t.State == downloads.Complete || t.State == downloads.Canceled) && t.Error != "" {
		action("cleanup", "Retry Cleanup", d.queue.RetryCleanup)
	}
	button := iconButton(c.Key("more"), iconMore, 16, p.muted, Tf("More for %s", t.Title)).Shrink(0)
	button.Menu(func(m *ui.Menu) {
		if t.State == downloads.Complete && a.local != nil && m.Item(T("Add Folder to Library")).Chosen() {
			a.local.AddFolders(t.Folder)
			c.Toast(T("Download folder added to the local library"))
		}
		if t.State == downloads.Complete || t.State == downloads.Canceled {
			if m.Item(T("Remove from Queue")).Disabled(!t.Cleaned).Chosen() {
				a.downloadTaskAction(d.queue.Forget(t.ID))
			}
			return
		}
		if t.State == downloads.Failed && t.Provider == downloads.Nyaa && m.Item(T("qBittorrent Settings")).Chosen() {
			a.showDownloadQBitSettings()
		}
		if m.Item(T("Cancel")).Chosen() {
			a.downloadTaskAction(d.queue.Cancel(t.ID))
		}
	})
}

func (a *app) showDownloadQBitSettings() {
	if a.libOf(downloadsID) == nil {
		return
	}
	a.open(route{page: pageDownloads, src: downloadsID})
	d := a.downloads
	if d.provider != downloads.Nyaa {
		d.setProvider(downloads.Nyaa)
	}
	d.qbOpen = true
	a.enterQBSettings()
}

func (a *app) downloadTaskAction(err error) {
	if err != nil {
		a.toast = T(err.Error())
	}
}

func (a *app) downloadTorrentPicker(c *ui.Context, p palette, t downloads.Task) {
	d := a.downloads
	ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
		ui.Button(c.Key("back"), T("Back to Queue")).Height(28).OnClick(func() { d.selecting = "" })
		ui.Text(c, t.Title).FontSize(16).FontWeight(600).SingleLine().MinWidth(0).Grow(1)
	})
	ui.List(c.Key("files-list"), &d.torrentList, len(t.Files), func(i int) {
		f := t.Files[i]
		check := d.fileSelection[f.Index]
		row := ui.CheckboxBase(c.Key(f.Index), check).Height(42).PaddingX(9).Gap(10).Radius(8).
			AlignItems(ui.Center).Label(f.Name)
		if *check {
			row.Background(p.accent.Alpha(0.1))
		} else if row.Hovered() {
			row.Background(p.hover)
		}
		row.Children(func() {
			downloadCheckMark(c, p, *check)
			ui.Text(c, f.Name).FontSize(13).SingleLine().Grow(1).MinWidth(0).Tooltip(f.Name)
			ui.Text(c, downloadBytes(f.Size)).FontSize(12.5).FontFeatures("tnum").TextColor(p.muted)
		})
	}).Grow(1).MinHeight(0)
	ui.Row(c).Gap(8).AlignItems(ui.Center).Children(func() {
		ui.Button(c.Key("all"), T("Select All")).OnClick(func() { setChecks(d.fileSelection, true) })
		ui.Button(c.Key("none"), T("Clear Selection")).OnClick(func() { setChecks(d.fileSelection, false) })
		ui.Box(c).Grow(1)
		selected := countChecks(d.fileSelection)
		ui.PrimaryButton(c.Key("download"), Tf("Download Selected (%d)", selected)).Disabled(selected == 0).OnClick(func() {
			var ids []int
			for _, f := range t.Files {
				if *d.fileSelection[f.Index] {
					ids = append(ids, f.Index)
				}
			}
			if err := d.queue.Select(t.ID, ids); err != nil {
				a.downloadTaskAction(err)
				return
			}
			d.selecting, d.fileSelection = "", nil
		})
	})
}

func downloadBytes(n int64) string {
	size := float64(n)
	for _, unit := range []string{"B", "KiB", "MiB", "GiB", "TiB"} {
		if size < 1024 || unit == "TiB" {
			if unit == "B" {
				return fmt.Sprintf("%d B", n)
			}
			return fmt.Sprintf("%.1f %s", size, unit)
		}
		size /= 1024
	}
	return ""
}
