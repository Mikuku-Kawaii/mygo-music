package downloads

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

type QBConfig struct {
	URL      string `json:"url"`
	Username string `json:"username"`
	Password string `json:"password"`
}

type QBClient struct {
	base   string
	client *http.Client
}

type qbTorrent struct {
	Hash     string `json:"hash"`
	Tags     string `json:"tags"`
	SavePath string `json:"save_path"`
	State    string `json:"state"`
	Speed    int64  `json:"dlspeed"`
}

type qbFile struct {
	TorrentFile
	Progress float64 `json:"progress"`
	Priority int     `json:"priority"`
}

const torrentPending = "pending"

func ConnectQB(ctx context.Context, cfg QBConfig) (*QBClient, error) {
	u, err := url.Parse(strings.TrimSpace(cfg.URL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") ||
		u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") ||
		(u.Hostname() != "localhost" && (net.ParseIP(u.Hostname()) == nil || !net.ParseIP(u.Hostname()).IsLoopback())) {
		return nil, errors.New("Enter the Web UI address of a local qBittorrent instance")
	}
	origin := u.Scheme + "://" + u.Host
	jar, _ := cookiejar.New(nil)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	client := &http.Client{
		Jar: jar, Transport: transport, Timeout: 4 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	q := &QBClient{origin, client}
	if cfg.Username != "" || cfg.Password != "" {
		_, err := q.form(ctx, "auth/login", url.Values{"username": {cfg.Username}, "password": {cfg.Password}})
		if err != nil {
			return nil, err
		}
	}
	// qBittorrent 5.3 uses an empty success body. The authenticated version
	// endpoint also verifies the login for releases that return a string.
	version, err := q.request(ctx, "app/version", nil, "", nil)
	if err != nil {
		return nil, err
	}
	major, _, _ := strings.Cut(strings.TrimPrefix(strings.TrimSpace(string(version)), "v"), ".")
	v, err := strconv.Atoi(major)
	if err != nil || v < 5 {
		return nil, errors.New("qBittorrent 5 or newer is required for file selection")
	}
	return q, nil
}

func (q *QBClient) request(ctx context.Context, action string, query url.Values, kind string, body io.Reader) ([]byte, error) {
	method := http.MethodGet
	if body != nil {
		method = http.MethodPost
	}
	endpoint := q.base + "/api/v2/" + action
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Origin", q.base)
	req.Header.Set("Referer", q.base+"/")
	if body != nil {
		req.Header.Set("Content-Type", kind)
	}
	resp, err := q.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("Could not reach the local qBittorrent Web UI")
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, errors.New("qBittorrent login failed")
	case http.StatusOK, http.StatusAccepted, http.StatusNoContent:
	default:
		return nil, fmt.Errorf("qBittorrent %s returned HTTP %d", action, resp.StatusCode)
	}
	const limit = 8 << 20
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if len(b) > limit {
		return nil, errors.New("qBittorrent response was too large")
	}
	return b, err
}

func (q *QBClient) form(ctx context.Context, action string, args url.Values) ([]byte, error) {
	return q.request(ctx, action, nil, "application/x-www-form-urlencoded", strings.NewReader(args.Encode()))
}

func (q *QBClient) transferAction(ctx context.Context, action, hash string) error {
	_, err := q.form(ctx, "torrents/"+action, url.Values{"hashes": {hash}})
	return err
}

func torrentTag(t Task) string { return "mygo-download-" + t.ID }
func torrentDir(t Task) string { return filepath.Join(t.stageDir(), "payload") }

func (q *QBClient) owned(ctx context.Context, t Task) (*qbTorrent, error) {
	body, err := q.request(ctx, "torrents/info", url.Values{"tag": {torrentTag(t)}}, "", nil)
	if err != nil {
		return nil, err
	}
	var entries []qbTorrent
	if err = json.Unmarshal(body, &entries); err != nil {
		return nil, errors.New("Invalid qBittorrent task list")
	}
	var found *qbTorrent
	target := filepath.Join(t.Folder, torrentDir(t))
	for _, entry := range entries {
		tags := strings.Split(entry.Tags, ",")
		if !slices.ContainsFunc(tags, func(s string) bool { return strings.TrimSpace(s) == torrentTag(t) }) {
			continue
		}
		if !validHash(entry.Hash) || (t.Torrent != torrentPending && t.Torrent != entry.Hash) ||
			!sameLocalPath(target, entry.SavePath) || found != nil {
			return nil, errors.New("The qBittorrent task no longer matches this download")
		}
		found = &entry
	}
	return found, nil
}

func (q *QBClient) add(ctx context.Context, t Task) error {
	magnet := strings.HasPrefix(t.URL, "magnet:")
	var data bytes.Buffer
	form := multipart.NewWriter(&data)
	for k, v := range map[string]string{
		"tags": torrentTag(t), "savepath": filepath.Join(t.Folder, torrentDir(t)),
		"autoTMM": "false", "useDownloadPath": "false", "contentLayout": "Original",
		"stopped": strconv.FormatBool(!magnet), "stopCondition": "MetadataReceived",
	} {
		if err := form.WriteField(k, v); err != nil {
			return err
		}
	}
	if filepath.IsAbs(t.URL) {
		if !strings.EqualFold(filepath.Ext(t.URL), ".torrent") {
			return errors.New("Choose a .torrent file")
		}
		f, err := os.Open(t.URL)
		if err != nil {
			return err
		}
		defer f.Close()
		info, err := f.Stat()
		if err != nil || !info.Mode().IsRegular() || info.Size() > 8<<20 {
			return errors.New("Invalid .torrent file")
		}
		part, err := form.CreateFormFile("torrents", filepath.Base(t.URL))
		if err != nil {
			return err
		}
		if _, err = io.Copy(part, io.LimitReader(f, 8<<20)); err != nil {
			return err
		}
	} else {
		if _, err := nyaaLink(t.URL); err != nil {
			return err
		}
		if err := form.WriteField("urls", t.URL); err != nil {
			return err
		}
	}
	if err := form.Close(); err != nil {
		return err
	}
	resp, err := q.request(ctx, "torrents/add", nil, form.FormDataContentType(), &data)
	if err != nil {
		return err
	}
	if !strings.HasPrefix(strings.TrimSpace(string(resp)), "Ok") {
		var result struct {
			Added   int      `json:"success_count"`
			Pending int      `json:"pending_count"`
			IDs     []string `json:"added_torrent_ids"`
		}
		if json.Unmarshal(resp, &result) != nil || result.Added+result.Pending != 1 ||
			len(result.IDs) != result.Added || slices.ContainsFunc(result.IDs, func(s string) bool { return !validHash(s) }) {
			return errors.New("qBittorrent could not add this torrent. Check for an existing task")
		}
	}
	return nil
}

func (q *QBClient) files(ctx context.Context, hash string) ([]qbFile, error) {
	b, err := q.request(ctx, "torrents/files", url.Values{"hash": {hash}}, "", nil)
	if err != nil {
		return nil, err
	}
	var files []qbFile
	if err := json.Unmarshal(b, &files); err != nil {
		return nil, errors.New("Invalid qBittorrent file list")
	}
	return files, nil
}

func (q *QBClient) priorities(ctx context.Context, hash string, files []qbFile, selected []int) error {
	groups := map[int][]string{}
	for _, f := range files {
		priority := 0
		if slices.Contains(selected, f.Index) {
			priority = 1
		}
		if f.Priority != priority {
			groups[priority] = append(groups[priority], strconv.Itoa(f.Index))
		}
	}
	for _, priority := range []int{0, 1} {
		group := groups[priority]
		for len(group) > 0 {
			n := min(len(group), 200)
			_, err := q.form(ctx, "torrents/filePrio", url.Values{
				"hash": {hash}, "id": {strings.Join(group[:n], "|")}, "priority": {strconv.Itoa(priority)},
			})
			if err != nil {
				return err
			}
			group = group[n:]
		}
	}
	return nil
}

func validHash(hash string) bool {
	if len(hash) != 40 && len(hash) != 64 {
		return false
	}
	return strings.IndexFunc(strings.ToLower(hash), func(r rune) bool { return !strings.ContainsRune("0123456789abcdef", r) }) < 0
}

func stopped(state string) bool {
	return state == "stoppedDL" || state == "stoppedUP" || state == "pausedDL" || state == "pausedUP"
}

func wait(ctx context.Context) error {
	timer := time.NewTimer(500 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (e *Engine) torrentTransfer(ctx context.Context, root *os.Root, t Task, save checkpoint, report progress) (files []stagedFile, result error) {
	if err := root.MkdirAll(torrentDir(t), 0700); err != nil {
		return nil, err
	}
	q, err := e.qb(ctx)
	if err != nil {
		return nil, err
	}
	newTask := t.Torrent == ""
	if newTask {
		if err := save(func(task *Task) { task.Torrent = torrentPending }); err != nil {
			return nil, err
		}
		t.Torrent = torrentPending
	}
	defer func() {
		stop, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := q.stopOwned(stop, t); err != nil && (result == nil || errors.Is(result, errChooseFiles)) {
			files, result = nil, err
		}
	}()
	entry, err := q.owned(ctx, t)
	if err != nil {
		return nil, err
	}
	if entry == nil {
		if t.Torrent != torrentPending {
			return nil, errors.New("The qBittorrent task was removed")
		}
		if err := q.add(ctx, t); err != nil {
			return nil, err
		}
	}
	report("Waiting for torrent metadata", 0, 0, 0)
	metadata, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	var available []qbFile
	restartedMetadata := false
	for {
		entry, err = q.owned(metadata, t)
		if err != nil {
			return nil, err
		}
		if entry != nil && entry.Hash != t.Torrent {
			if err = save(func(task *Task) { task.Torrent = entry.Hash }); err != nil {
				return nil, err
			}
			t.Torrent = entry.Hash
		}
		if entry != nil {
			if entry.State == "error" || entry.State == "missingFiles" {
				return nil, errors.New("qBittorrent reported a file error")
			}
			if len(t.Selected) > 0 && !stopped(entry.State) {
				if err := q.transferAction(ctx, "stop", entry.Hash); err != nil {
					return nil, err
				}
			}
			if stopped(entry.State) {
				available, err = q.files(metadata, entry.Hash)
				if err != nil {
					return nil, err
				}
				if len(available) > 0 {
					break
				}
				if !restartedMetadata && len(t.Selected) == 0 && strings.HasPrefix(t.URL, "magnet:") {
					// Stop clears MetadataReceived in qBittorrent. Add the
					// metadata-only job afresh rather than starting payload.
					if err := save(func(task *Task) { task.Torrent = torrentPending }); err != nil {
						return nil, err
					}
					t.Torrent = torrentPending
					if err := q.remove(metadata, entry.Hash); err != nil {
						return nil, err
					}
					if err := q.add(metadata, t); err != nil {
						return nil, err
					}
					restartedMetadata = true
				}
			}
		}
		if err := wait(metadata); err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, errors.New("Torrent metadata was unavailable. Check seeders and retry")
		}
	}
	hash := entry.Hash
	if len(t.Selected) == 0 {
		lossless := make([]TorrentFile, 0)
		for _, f := range available {
			rel := filepath.FromSlash(f.Name)
			if filepath.IsLocal(rel) && isLosslessName(f.Name) && f.Size > 0 {
				lossless = append(lossless, f.TorrentFile)
			}
		}
		if len(lossless) == 0 {
			return nil, errors.New("This torrent has no selectable lossless audio files")
		}
		if err := save(func(task *Task) { task.Files = lossless }); err != nil {
			return nil, err
		}
		return nil, errChooseFiles
	}
	for _, id := range t.Selected {
		if !slices.ContainsFunc(available, func(f qbFile) bool {
			return f.Index == id && filepath.IsLocal(filepath.FromSlash(f.Name)) && isLosslessName(f.Name) && f.Size > 0
		}) {
			return nil, errors.New("The selected torrent files changed")
		}
	}
	if err := q.priorities(ctx, hash, available, t.Selected); err != nil {
		return nil, err
	}
	if err := q.transferAction(ctx, "start", hash); err != nil {
		return nil, err
	}
	for {
		entry, err = q.owned(ctx, t)
		if err != nil {
			return nil, err
		}
		if entry == nil || entry.State == "error" || entry.State == "missingFiles" {
			return nil, errors.New("qBittorrent reported a file error")
		}
		available, err = q.files(ctx, hash)
		if err != nil {
			return nil, err
		}
		files = files[:0]
		var bytes, total int64
		for _, f := range available {
			if !slices.Contains(t.Selected, f.Index) {
				continue
			}
			total += f.Size
			bytes += int64(float64(f.Size) * f.Progress)
			rel := filepath.FromSlash(f.Name)
			if f.Progress < 1 || !filepath.IsLocal(rel) {
				continue
			}
			path := filepath.Join(torrentDir(t), rel)
			info, err := root.Stat(path)
			if err == nil && info.Mode().IsRegular() && info.Size() == f.Size {
				files = append(files, stagedFile{path, filepath.Base(rel)})
			}
		}
		report("Downloading", bytes, total, entry.Speed)
		if len(files) == len(t.Selected) {
			return files, nil
		}
		if stopped(entry.State) && bytes == total {
			return nil, errors.New("qBittorrent could not finalize the selected files")
		}
		if err := wait(ctx); err != nil {
			return nil, err
		}
	}
}

func (e *Engine) qb(ctx context.Context) (*QBClient, error) {
	if e.BitTorrent == nil {
		return nil, errors.New("Connect a local qBittorrent Web UI first")
	}
	return e.BitTorrent(ctx)
}

func (q *QBClient) stopOwned(ctx context.Context, t Task) error {
	entry, err := q.owned(ctx, t)
	if err != nil || entry == nil || stopped(entry.State) {
		return err
	}
	return q.transferAction(ctx, "stop", entry.Hash)
}

func (e *Engine) stopTorrent(ctx context.Context, t Task) error {
	q, err := e.qb(ctx)
	if err != nil {
		return err
	}
	return q.stopOwned(ctx, t)
}

func (e *Engine) releaseTorrent(ctx context.Context, t Task) error {
	q, err := e.qb(ctx)
	if err != nil {
		return err
	}
	entry, err := q.owned(ctx, t)
	if err != nil || entry == nil {
		return err
	}
	if err := q.transferAction(ctx, "stop", entry.Hash); err != nil {
		return err
	}
	return q.remove(ctx, entry.Hash)
}

func (q *QBClient) remove(ctx context.Context, hash string) error {
	_, err := q.form(ctx, "torrents/delete", url.Values{"hashes": {hash}, "deleteFiles": {"false"}})
	return err
}
