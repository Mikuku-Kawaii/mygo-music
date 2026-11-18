package downloads

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"mygo-music/internal/music"
)

func (i Item) NeedsNetEaseLabels() bool {
	return i.Provider == NetEase && i.Group == "" && numericID(i.ID) && (i.Title == "NetEase "+i.ID || i.Detail == "")
}

func (i *Item) applyLabels(labels Item) {
	if labels.Title != "" {
		i.Title = labels.Title
	}
	if labels.Detail != "" {
		i.Detail = labels.Detail
	}
	if labels.ImageURL != "" {
		i.ImageURL = labels.ImageURL
	}
	if labels.Album != "" {
		i.Album = labels.Album
	}
	if labels.Duration > 0 {
		i.Duration = labels.Duration
	}
}

// NetEaseLabels gets public song credits independently of the playback
// plugin and its session. Missing metadata leaves an otherwise valid song
// link usable, even when the lookup times out.
func (e *Engine) NetEaseLabels(ctx context.Context, ids []string) map[string]Item {
	unique := make([]string, 0, len(ids))
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		if numericID(id) && !seen[id] {
			unique = append(unique, id)
			seen[id] = true
		}
	}
	if len(unique) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 7*time.Second)
	defer cancel()
	labels := make(map[string]Item, len(unique))
	for batch := range slices.Chunk(unique, 50) {
		if !e.netEaseLabelBatch(ctx, batch, labels) {
			break
		}
	}
	return labels
}

func (e *Engine) netEaseLabelBatch(ctx context.Context, ids []string, out map[string]Item) bool {
	query := url.Values{"ids": {"[" + strings.Join(ids, ",") + "]"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://music.163.com/api/song/detail/?"+query.Encode(), nil)
	if err != nil {
		return false
	}
	req.Header.Set("User-Agent", music.Agent)
	client := e.client()
	client.Jar = nil
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	res, err := client.Do(req)
	if err != nil {
		return false
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return false
	}
	var body struct {
		Code  int `json:"code"`
		Songs []struct {
			ID       uint64         `json:"id"`
			Name     string         `json:"name"`
			Artists  []music.Artist `json:"artists"`
			Duration int64          `json:"duration"`
			Album    struct {
				Name    string `json:"name"`
				Picture string `json:"picUrl"`
			} `json:"album"`
		} `json:"songs"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 2<<20)).Decode(&body); err != nil || body.Code != 200 {
		return false
	}
	for _, song := range body.Songs {
		id := strconv.FormatUint(song.ID, 10)
		name := strings.TrimSpace(song.Name)
		if !slices.Contains(ids, id) || name == "" {
			continue
		}
		t := music.Track{Artists: song.Artists}
		out[id] = Item{Title: name, Detail: t.ArtistNames(), Album: song.Album.Name,
			ImageURL: netEaseArtwork(song.Album.Picture), Duration: song.Duration}
	}
	return true
}

func netEaseArtwork(raw string) string {
	if rest, ok := strings.CutPrefix(raw, "http://"); ok {
		raw = "https://" + rest
	}
	u, err := httpsURL(raw)
	if err != nil || u.Port() != "" || !strings.HasSuffix(u.Hostname(), ".music.126.net") {
		return ""
	}
	return u.String()
}
