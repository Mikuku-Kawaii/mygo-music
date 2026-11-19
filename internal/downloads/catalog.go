package downloads

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"mygo-music/internal/music"
)

type Search struct {
	Provider Provider
	Query    string
	Kind     music.SearchKind
	Offset   int
}

type Results struct {
	Items []Item
	More  bool
}

const (
	Tracks    = music.SearchTracks
	Albums    = music.SearchAlbums
	Playlists = music.SearchPlaylists
)

func (e *Engine) Search(ctx context.Context, req Search, source music.Source) (Results, error) {
	query := strings.TrimSpace(req.Query)
	if query == "" {
		return Results{}, errors.New("Enter keywords or a link")
	}
	if len(query) > 48<<10 {
		return Results{}, errors.New("The search input is too long")
	}
	if req.Provider == HTTPS {
		var result Results
		for _, raw := range strings.Fields(query) {
			u, err := httpsURL(raw)
			if err != nil {
				return Results{}, err
			}
			title := filepath.Base(u.Path)
			title = strings.TrimSuffix(title, mediaExt(filepath.Ext(title)))
			if title == "/" || title == "." || title == "" {
				title = u.Hostname()
			}
			result.Items = append(result.Items, Item{Provider: HTTPS, URL: u.String(), Title: title, Detail: u.Hostname()})
		}
		return result, nil
	}
	switch req.Provider {
	case NetEase:
		return e.searchNetEase(ctx, query, req.Kind, req.Offset, source)
	case YouTube, Bilibili:
		return e.searchVideo(ctx, req.Provider, query)
	case Nyaa:
		return e.searchNyaa(ctx, query)
	default:
		return Results{}, errors.New("Unsupported download source")
	}
}

func numericID(s string) bool {
	id, err := strconv.ParseUint(s, 10, 64)
	return id > 0 && err == nil
}

func neteaseLink(raw string) (kind, id string, err error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") ||
		(u.Hostname() != "music.163.com" && u.Hostname() != "y.music.163.com") || u.User != nil {
		return "", "", errors.New("Enter a NetEase song, album or playlist link")
	}
	if strings.HasPrefix(u.Fragment, "/") {
		u, err = url.Parse(u.Fragment)
		if err != nil {
			return "", "", errors.New("Enter a NetEase song, album or playlist link")
		}
	}
	kind = filepath.Base(u.Path)
	id = u.Query().Get("id")
	if !numericID(id) || (kind != "song" && kind != "album" && kind != "playlist") {
		return "", "", errors.New("Enter a NetEase song, album or playlist link")
	}
	return kind, id, nil
}

func (e *Engine) searchNetEase(ctx context.Context, query string, kind music.SearchKind, offset int, source music.Source) (Results, error) {
	if source == nil {
		return Results{}, errors.New("Enable the NetEase Music source first")
	}
	if strings.Contains(query, "://") {
		var result Results
		var ids []string
		for _, link := range strings.Fields(query) {
			k, id, err := neteaseLink(link)
			if err != nil {
				return Results{}, err
			}
			if k != "song" {
				items, err := ExpandNetEase(ctx, source, Item{Provider: NetEase, ID: id, Group: k})
				if err != nil {
					return Results{}, err
				}
				result.Items = append(result.Items, items...)
				continue
			}
			ids = append(ids, id)
			result.Items = append(result.Items, Item{Provider: NetEase, ID: id, Title: "NetEase " + id, URL: "https://music.163.com/song?id=" + id})
		}
		labels := e.NetEaseLabels(ctx, ids)
		for i := range result.Items {
			if result.Items[i].NeedsNetEaseLabels() {
				result.Items[i].applyLabels(labels[result.Items[i].ID])
			}
		}
		if err := ctx.Err(); err != nil {
			return Results{}, err
		}
		return result, nil
	}
	response, err := source.Search(query, kind, offset)
	if err != nil {
		return Results{}, sourceError(err, "NetEase search failed")
	}
	if ctx.Err() != nil {
		return Results{}, ctx.Err()
	}
	if response == nil {
		return Results{}, errors.New("NetEase returned no search response")
	}
	result := Results{More: response.More}
	switch kind {
	case Albums:
		for _, a := range response.Albums {
			credits := music.Track{Artists: a.Artists}
			result.Items = append(result.Items, Item{Provider: NetEase, ID: a.ID, Title: a.Name, Detail: credits.ArtistNames(), ImageURL: a.ImageURL, Group: "album"})
		}
	case Playlists:
		for _, p := range response.Playlists {
			result.Items = append(result.Items, Item{Provider: NetEase, ID: p.ID, Title: p.Name, Detail: p.Owner, ImageURL: p.ImageURL, Group: "playlist"})
		}
	default:
		result.Items = trackItems(response.Tracks)
	}
	return result, nil
}

func ExpandNetEase(ctx context.Context, source music.Source, item Item) ([]Item, error) {
	if source == nil {
		return nil, errors.New("Enable the NetEase Music source first")
	}
	var tracks []music.Track
	var err error
	switch item.Group {
	case "album":
		var album *music.AlbumPage
		album, err = source.Album(item.ID)
		if album != nil {
			tracks = album.Tracks
		}
	case "playlist":
		var playlist *music.Playlist
		playlist, err = source.Playlist(item.ID)
		if playlist != nil {
			tracks = playlist.Tracks
		}
	default:
		return nil, errors.New("Invalid NetEase collection")
	}
	if err != nil {
		return nil, sourceError(err, "Could not open the NetEase collection")
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return trackItems(tracks), nil
}

func trackItems(tracks []music.Track) []Item {
	items := make([]Item, 0, len(tracks))
	for _, t := range tracks {
		items = append(items, Item{Provider: NetEase, ID: t.ID, Title: t.Name, Detail: t.ArtistNames(),
			ImageURL: t.Album.ImageURL, Album: t.Album.Name, Duration: t.Duration, Unavailable: t.Unavailable})
	}
	return items
}

// sourceError displays the service's status and the source's user-facing
// reason. Raw plugin errors can include request URLs and must stay out of the journal.
func sourceError(err error, fallback string) error {
	message := err.Error()
	switch {
	case strings.Contains(message, "-460"):
		return errors.New("NetEase rejected this request (-460)")
	case strings.HasPrefix(message, "[460] "):
		return errors.New("NetEase rejected this request (460)")
	case errors.Is(err, music.ErrSignIn):
		return errors.New("Sign in to NetEase Music first")
	case errors.Is(err, music.ErrSetup):
		return errors.New("Set up the NetEase Music source first")
	case errors.Is(err, music.ErrUnavailable):
		if reason := music.Reason(err); reason != "" {
			return errors.New(reason)
		}
		return errors.New("This track is unavailable to the current NetEase account")
	default:
		return errors.New(fallback)
	}
}

func ResolveNetEase(ctx context.Context, src music.Source, id string, format Format) (*music.Stream, error) {
	if src == nil {
		return nil, errors.New("Enable the NetEase Music source first")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if id == "" {
		return nil, errors.New("Search for this NetEase track again to restore its song ID")
	}
	quality := "lossless"
	if format == Original {
		quality = "high"
	}
	stream, err := src.Stream(id, quality)
	if err != nil {
		return nil, sourceError(err, "Could not resolve NetEase audio")
	}
	return stream, ctx.Err()
}

func nyaaLink(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err == nil && u.Scheme == "magnet" && u.Opaque == "" {
		for _, xt := range u.Query()["xt"] {
			hash := strings.TrimPrefix(strings.ToLower(xt), "urn:btih:")
			if hash != xt && (validHash(hash) || len(hash) == 32) {
				return u.String(), nil
			}
			algo, value, ok := strings.Cut(strings.ToLower(xt), "1220")
			if ok && algo == "urn:btmh:" && len(value) == 64 && validHash(value) {
				return u.String(), nil
			}
		}
	}
	if err == nil && u.Scheme == "https" && u.Host == "nyaa.si" && u.User == nil {
		kind, id, ok := strings.Cut(strings.Trim(u.Path, "/"), "/")
		if ok && (kind == "view" || kind == "download") && numericID(strings.TrimSuffix(id, ".torrent")) {
			return "https://nyaa.si/download/" + strings.TrimSuffix(id, ".torrent") + ".torrent", nil
		}
	}
	return "", errors.New("Enter a Nyaa torrent, magnet link or local .torrent file")
}

func (e *Engine) searchNyaa(ctx context.Context, query string) (Results, error) {
	if filepath.IsAbs(query) && strings.EqualFold(filepath.Ext(query), ".torrent") {
		return Results{Items: []Item{{Provider: Nyaa, Title: filepath.Base(query), URL: query}}}, nil
	}
	var queries []string
	if strings.HasPrefix(query, "https://") || strings.HasPrefix(query, "magnet:") {
		for _, raw := range strings.Fields(query) {
			if u, err := url.Parse(raw); err == nil && u.Scheme == "https" && u.Host == "nyaa.si" && u.Path == "/" {
				query = u.Query().Get("q")
				queries = nil
				break
			}
			link, err := nyaaLink(raw)
			if err != nil {
				return Results{}, err
			}
			queries = append(queries, link)
		}
	}
	if len(queries) > 0 {
		result := Results{}
		for _, link := range queries {
			title := filepath.Base(link)
			if u, err := url.Parse(link); err == nil && u.Scheme == "magnet" {
				title = u.Query().Get("dn")
				if title == "" {
					title = "Magnet"
				}
			}
			result.Items = append(result.Items, Item{Provider: Nyaa, Title: title, URL: link})
		}
		return result, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	q := url.Values{"page": {"rss"}, "c": {"2_1"}, "q": {query}}
	addr := "https://nyaa.si/?" + q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, addr, nil)
	if err != nil {
		return Results{}, err
	}
	req.Header.Set("User-Agent", music.Agent)
	client := e.client()
	client.CheckRedirect = func(next *http.Request, via []*http.Request) error {
		if len(via) >= 5 || next.URL.Scheme != "https" || next.URL.Host != "nyaa.si" {
			return http.ErrUseLastResponse
		}
		return nil
	}
	res, err := client.Do(req)
	if err != nil {
		return Results{}, errors.New("Could not reach the Nyaa index")
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return Results{}, fmt.Errorf("Nyaa search returned HTTP %d", res.StatusCode)
	}
	var feed struct {
		Items []struct {
			Title   string `xml:"title"`
			Link    string `xml:"link"`
			Hash    string `xml:"infoHash"`
			Size    string `xml:"size"`
			Seeders int    `xml:"seeders"`
		} `xml:"channel>item"`
	}
	if err := xml.NewDecoder(io.LimitReader(res.Body, 4<<20)).Decode(&feed); err != nil {
		return Results{}, errors.New("Could not read the Nyaa search results")
	}
	result := Results{}
	for _, x := range feed.Items {
		link, err := nyaaLink(x.Link)
		if err != nil {
			continue
		}
		result.Items = append(result.Items, Item{Provider: Nyaa, ID: x.Hash, Title: x.Title, URL: link, Detail: fmt.Sprintf("%s · ↑ %d", x.Size, x.Seeders)})
	}
	return result, nil
}
