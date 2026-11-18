package app

import (
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/egoist/mygo/ui"

	"mygo-music/internal/downloads"
	"mygo-music/internal/music"
)

func downloadProviderName(p downloads.Provider) string {
	switch p {
	case downloads.NetEase:
		return T("NetEase Music")
	case downloads.HTTPS:
		return T("HTTPS Direct Links")
	case downloads.YouTube:
		return "YouTube"
	case downloads.Bilibili:
		return T("Bilibili")
	case downloads.Nyaa:
		return "Nyaa"
	}
	return string(p)
}

func (a *app) downloadPage(c *ui.Context) {
	d, p := a.downloads, a.pal
	a.downloadPageFrame(c, T("Search and Downloads"), func() {
		a.downloadSearchBar(c.Key("download-search-bar"))
		a.downloadFilters(c.Key("download-filters"))
		for _, msg := range []string{d.err, d.searchErr} {
			if msg != "" {
				ui.Text(c, msg).TextColor(p.accent).FontSize(13).MaxLines(3)
			}
		}
		a.downloadResults(c.Key("results"))
		if selected := countChecks(d.chosen); selected > 0 {
			a.downloadBatchBar(c.Key("batch"), selected)
		}
	})
}

func (a *app) downloadPageFrame(c *ui.Context, title string, content func()) {
	top, bottom := a.insets(c)
	ui.Column(c).Fill().Padding(top+pageGap, pagePad, bottom+12, pagePad).Gap(12).Children(func() {
		ui.Text(c, title).FontSize(30).FontWeight(700).LetterSpacing(-0.6).SingleLine().Margin(8, 0, 4, 0)
		content()
	})
}

func (a *app) downloadSearchBar(c *ui.Context) {
	d := a.downloads
	ui.Row(c).Gap(8).AlignItems(ui.Start).Children(func() {
		providers := []downloads.Provider{downloads.NetEase, downloads.HTTPS, downloads.YouTube, downloads.Bilibili, downloads.Nyaa}
		names := make([]string, len(providers))
		for i, p := range providers {
			names[i] = downloadProviderName(p)
		}
		label := downloadProviderName(d.provider)
		if ui.Select(c.Key("provider"), &label, names).Height(36).Width(138).Label(T("Download Source")).Changed() {
			if at := slices.Index(names, label); at >= 0 {
				d.setProvider(providers[at])
			}
		}
		if d.provider == downloads.HTTPS {
			ui.TextArea(c.Key("links"), &d.query).Bind(&d.search).Label(T("HTTPS URLs")).
				Placeholder(T("Paste one HTTPS link per line")).Height(62).Grow(1).MinWidth(0)
		} else {
			placeholder := "Enter keywords or a link"
			switch d.provider {
			case downloads.NetEase:
				placeholder = "Search songs, or paste a song, album or playlist link"
			case downloads.Nyaa:
				placeholder = "Search lossless releases, or paste a Nyaa or magnet link"
			}
			ui.TextInput(c.Key("query"), &d.query).Bind(&d.search).Label(T("Search or paste a link")).
				Placeholder(T(placeholder)).Height(36).Grow(1).MinWidth(0).OnSubmit(func() { a.findDownloads(false) })
		}
		if d.searching {
			ui.Button(c.Key("stop-search"), T("Cancel")).Height(36).Shrink(0).OnClick(d.stopSearch)
		} else {
			label := T("Search")
			if d.provider == downloads.HTTPS {
				label = T("Read Links")
			}
			ui.PrimaryButton(c.Key("submit-search"), label).Height(36).Shrink(0).
				Disabled(d.engine == nil || strings.TrimSpace(d.query) == "").OnClick(func() { a.findDownloads(false) })
		}
	})
}

func (a *app) downloadFilters(c *ui.Context) {
	d := a.downloads
	if d.provider != downloads.NetEase && d.provider != downloads.Nyaa && !d.searching {
		return
	}
	ui.Row(c).AlignItems(ui.Center).Gap(8).Children(func() {
		switch d.provider {
		case downloads.NetEase:
			kinds := []music.SearchKind{downloads.Tracks, downloads.Albums, downloads.Playlists}
			selected := max(slices.Index(kinds, d.kind), 0)
			if segmented(c.Key("search-kind"), &selected, T("Songs"), T("Albums"), T("Playlists")) {
				d.stopSearch()
				d.kind = kinds[selected]
				d.items, d.chosen, d.more, d.submitted, d.parent, d.group = nil, nil, false, false, nil, ""
				d.results, d.searchErr = ui.ListState{}, ""
			}
		case downloads.Nyaa:
			ui.Button(c.Key("open-torrent"), T("Open .torrent File")).Height(26).OnClick(a.chooseTorrentFile)
			a.downloadQBitEntry(c.Key("qbit"))
		}
		if d.searching {
			ui.Text(c, T("Searching…")).FontSize(12).TextColor(a.pal.muted).SingleLine()
		}
	})
}

func (a *app) downloadQBitEntry(c *ui.Context) {
	d := a.downloads
	b := ui.Button(c.Key("settings"), T("qBittorrent Settings")).Height(26).OnClick(func() {
		d.qbOpen = !d.qbOpen
		if d.qbOpen {
			a.enterQBSettings()
		}
	})
	ui.Popover(c.Key("panel"), b, &d.qbOpen, func() {
		a.downloadQBitPane(c.Key("connection"))
	})
}

func (a *app) downloadBatchBar(c *ui.Context, selected int) {
	d, p := a.downloads, a.pal
	ui.Row(c).AlignItems(ui.Center).Gap(8).Padding(8, 10).Radius(10).Background(p.hover).Children(func() {
		ui.Text(c, Tf("%d selected", selected)).FontSize(13).FontWeight(600).SingleLine().Shrink(0)
		ui.Box(c).MinWidth(0).Grow(1)
		a.downloadFormats(c)
		folder := iconButton(c.Key("folder"), iconFolder, 16, p.muted, T("Choose Folder…")).Tooltip(d.folder)
		folder.OnClick(func() {
			pickFolders(a, func(folders []string) {
				d.folder = filepath.Clean(folders[0])
				a.settings.DownloadFolder = d.folder
				if err := a.settings.save(); err != nil {
					a.toast = Tf("Could not save the download folder (%s)", err)
				}
			})
		})
		ui.PrimaryButton(c.Key("enqueue"), T("Download Selection")).
			Label(Tf("Queue Selected (%d)", selected)).Height(32).Shrink(0).
			Disabled(!filepath.IsAbs(d.folder) || d.queue == nil || d.searching).OnClick(a.enqueueDownloads)
	})
}

func (a *app) downloadFormats(c *ui.Context) {
	d := a.downloads
	switch d.provider {
	case downloads.NetEase:
		labels := []string{T("Lossless FLAC"), T("High Quality")}
		label := labels[0]
		if d.format == downloads.Original {
			label = labels[1]
		}
		if ui.Select(c.Key("quality"), &label, labels).Label(T("Download Format")).Height(32).Width(124).Changed() {
			d.format = downloads.Lossless
			if label == labels[1] {
				d.format = downloads.Original
			}
		}
	case downloads.YouTube, downloads.Bilibili:
		labels := []string{T("Original Audio"), T("Video")}
		label := labels[0]
		if d.format == downloads.Video {
			label = labels[1]
		}
		if ui.Select(c.Key("format"), &label, labels).Label(T("Download Format")).Height(32).Width(124).Changed() {
			d.format = downloads.Audio
			if label == labels[1] {
				d.format = downloads.Video
			}
		}
	}
}

func countChecks(choices map[int]*bool) int {
	n := 0
	for _, selected := range choices {
		if *selected {
			n++
		}
	}
	return n
}

func setChecks(choices map[int]*bool, selected bool) {
	for _, check := range choices {
		*check = selected
	}
}

func (a *app) downloadResults(c *ui.Context) {
	d, p := a.downloads, a.pal
	if d.parent != nil {
		ui.Row(c.Key("collection")).Gap(8).AlignItems(ui.Center).Children(func() {
			ui.Button(c.Key("back"), T("Back to Results")).OnClick(func() {
				d.stopSearch()
				d.setResults(*d.parent, false)
				d.parent, d.group, d.searchErr = nil, "", ""
			})
			ui.Text(c, d.group).FontSize(17).FontWeight(600).SingleLine().Shrink(1).MinWidth(0)
		})
	}
	if len(d.items) == 0 {
		ui.Column(c).Grow(1).Center().Children(func() {
			if d.submitted && !d.searching && d.searchErr == "" {
				ui.Text(c, T("No results")).FontSize(14).TextColor(p.muted)
			}
		})
		return
	}
	ui.Row(c.Key("results-heading")).PaddingX(9).AlignItems(ui.Center).Gap(8).Children(func() {
		ui.Text(c, Tf("%d results", len(d.items))).FontSize(12).TextColor(p.muted).FontFeatures("tnum").Grow(1)
		if len(d.chosen) > 0 {
			if countChecks(d.chosen) == len(d.chosen) {
				ui.Button(c.Key("clear-all"), T("Clear Selection")).Height(26).OnClick(func() { setChecks(d.chosen, false) })
			} else {
				ui.Button(c.Key("select-all"), T("Select All")).Height(26).OnClick(func() { setChecks(d.chosen, true) })
			}
		}
	})
	rows := len(d.items)
	if d.more {
		rows++
	}
	ui.List(c.Key("download-results"), &d.results, rows, func(i int) {
		if i == len(d.items) {
			ui.Button(c.Key("search-more"), T("Load More")).AlignSelf(ui.Start).
				Disabled(d.searching).OnClick(func() { a.findDownloads(true) })
			return
		}
		a.downloadResultRow(c.Key(i), d.items[i], d.chosen[i])
	}).Grow(1).MinHeight(0).Padding(0, 0, 4, 0)
}

func (a *app) downloadResultRow(c *ui.Context, item downloads.Item, choice *bool) {
	p := a.pal
	var row ui.Element
	switch {
	case choice != nil:
		row = ui.CheckboxBase(c, choice).Label(item.Title)
	case item.Group != "":
		row = ui.ButtonBase(c).Label(item.Title).Disabled(a.downloads.searching).OnClick(func() { a.expandDownloadGroup(item) })
	default:
		row = ui.Row(c).Opacity(0.52)
	}
	row.Height(54).PaddingX(10).Gap(12).Radius(9).AlignItems(ui.Center)
	if choice != nil && *choice {
		row.Background(p.accent.Alpha(0.10))
	} else if row.Hovered() {
		row.Background(p.hover)
	}
	row.Children(func() {
		ui.Box(c).Width(22).Shrink(0).Center().Children(func() {
			if choice != nil {
				downloadCheckMark(c, p, *choice)
			}
		})
		a.cover(c, item.ImageURL, 96, 38, 6)
		downloadItemTitle(c, p, item.Title, item.Detail)
		if w, _ := c.Size(); w-sidebarWidth > 820 && item.Album != "" {
			ui.Text(c, item.Album).FontSize(12.5).TextColor(p.muted).SingleLine().Grow(1).Basis(0).MinWidth(0)
		}
		switch {
		case item.Unavailable:
			ui.Text(c, T("Unavailable")).FontSize(12).TextColor(p.muted).SingleLine()
		case item.Group != "":
			ui.Text(c, T("Show Tracks")).FontSize(12).TextColor(p.muted).SingleLine()
		case item.Duration > 0:
			ui.Text(c, clock(time.Duration(item.Duration)*time.Millisecond)).FontSize(12.5).FontFeatures("tnum").
				TextColor(p.muted).Width(44).Shrink(0).TextAlign(ui.End)
		}
	})
}

func downloadCheckMark(c *ui.Context, p palette, chosen bool) {
	box := ui.Box(c).Size(16, 16).Radius(4).Center().Border(1, p.faint)
	if chosen {
		box.Background(p.accent).Border(1, p.accent).Children(func() {
			ui.Icon(c, iconCheck).Size(12, 12).TextColor(p.onAccent)
		})
	}
}

func downloadItemTitle(c *ui.Context, p palette, title, detail string) {
	ui.Column(c).Grow(2).Basis(0).MinWidth(0).Gap(3).Children(func() {
		ui.Text(c, title).FontSize(14).FontWeight(500).TextColor(p.text).SingleLine().Tooltip(title)
		if detail != "" {
			ui.Text(c, detail).FontSize(12.5).TextColor(p.muted).SingleLine()
		}
	})
}

func (a *app) downloadQBitPane(c *ui.Context) {
	d, p := a.downloads, a.pal
	ui.Column(c).Width(330).Gap(8).Padding(4).Children(func() {
		ui.Text(c, T("qBittorrent Settings")).FontSize(16).FontWeight(650)
		ui.Text(c, T("Enable the Web UI in qBittorrent and enter its local address.")).FontSize(12.5).TextColor(p.muted).MaxLines(3)
		ui.TextInput(c.Key("qb-url"), &d.qbURL).Bind(&d.qbURLInput).Label(T("Web UI Address")).Placeholder(qbLocalURL).Disabled(d.qbChecking)
		ui.TextInput(c.Key("qb-user"), &d.qbUser).Bind(&d.qbUserInput).Label(T("Username")).
			Placeholder(T("Username")).Disabled(d.qbChecking)
		ui.TextInput(c.Key("qb-password"), &d.qbPassword).Bind(&d.qbPassInput).Label(T("Password")).
			Placeholder(T("Password")).Password().Disabled(d.qbChecking)
		label := T("Test and Save Connection")
		if d.qbChecking {
			label = T("Connecting…")
		}
		ui.PrimaryButton(c.Key("qb-connect"), label).AlignSelf(ui.Start).Disabled(d.qbChecking).OnClick(a.connectQB)
		if d.qbStatus != "" {
			ui.Text(c, d.qbStatus).FontSize(13).MaxLines(3)
		}
	})
}
