package downloads

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"mygo-music/internal/music"
)

func httpsURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Opaque != "" {
		return nil, errors.New("Enter a valid HTTPS link")
	}
	u.Fragment = ""
	return u, nil
}

func (e *Engine) client() http.Client {
	if e.HTTP != nil {
		return *e.HTTP
	}
	return http.Client{Transport: mediaTransport}
}

var mediaTransport = func() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.ResponseHeaderTimeout = 20 * time.Second
	return t
}()

func (e *Engine) httpTransfer(ctx context.Context, root *os.Root, part string, t Task, st *music.Stream, save checkpoint, report progress) (stagedFile, error) {
	u, err := httpsURL(st.URL)
	if err != nil {
		return stagedFile{}, err
	}
	var offset int64
	if t.Transfer.ETag != "" && t.Transfer.Resource != "" {
		if f, err := root.Stat(part); err == nil && f.Mode().IsRegular() {
			offset = f.Size()
		}
	}
	if t.Transfer.Size > 0 && offset >= t.Transfer.Size {
		offset = 0
	}
	res, err := e.mediaGET(ctx, u, st.Headers, offset, t.Transfer.ETag)
	if err != nil {
		return stagedFile{}, err
	}
	// A range is appendable only for the same strong validator and an exact
	// range starting at the local size. Otherwise request a fresh body once.
	total := res.ContentLength
	if offset > 0 && res.StatusCode != http.StatusOK {
		if size, ok := verifiedRange(res, offset, t.Transfer, t.Provider == NetEase); ok {
			total = size
		} else if res.StatusCode == http.StatusPartialContent || res.StatusCode == http.StatusRequestedRangeNotSatisfiable {
			res.Body.Close()
			offset = 0
			res, err = e.mediaGET(ctx, u, st.Headers, 0, "")
			if err != nil {
				return stagedFile{}, err
			}
			total = res.ContentLength
		}
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusOK {
		offset = 0
	} else if res.StatusCode != http.StatusPartialContent || offset == 0 {
		return stagedFile{}, fmt.Errorf("Media download failed (HTTP %d)", res.StatusCode)
	}
	if st.Size > 0 {
		if total >= 0 && total != st.Size {
			return stagedFile{}, errors.New("Media length differs from the source")
		}
		total = st.Size
	}
	name, ext := mediaName(t, st, res)
	if ext == "" {
		return stagedFile{}, errors.New("Unsupported media format")
	}
	etag := strongETag(res.Header.Get("ETag"))
	if offset == 0 {
		f, err := root.OpenFile(part, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
		if err != nil {
			return stagedFile{}, err
		}
		if err = f.Close(); err != nil {
			return stagedFile{}, err
		}
	}
	resource := mediaResource(res.Request.URL, t.Provider == NetEase)
	if err := save(func(t *Task) {
		t.Transfer = transfer{ETag: etag, Resource: resource, Size: max(total, 0), Ext: ext, Name: name}
	}); err != nil {
		return stagedFile{}, err
	}
	n, err := writePart(ctx, root, part, res.Body, offset, total, report)
	if err != nil {
		return stagedFile{}, err
	}
	if err := save(func(t *Task) { t.Transfer.Size = n }); err != nil {
		return stagedFile{}, err
	}
	return stagedFile{part, name}, nil
}

func (e *Engine) mediaGET(ctx context.Context, u *url.URL, headers map[string]string, offset int64, etag string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, errors.New("Invalid media address")
	}
	req.Header.Set("User-Agent", music.Agent)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("Accept-Encoding", "identity")
	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
		req.Header.Set("If-Range", etag)
	}
	client := e.client()
	client.CheckRedirect = func(next *http.Request, via []*http.Request) error {
		if len(via) >= 10 || next.URL.Scheme != "https" || next.URL.User != nil {
			return errors.New("Invalid media redirect")
		}
		if !strings.EqualFold(next.URL.Host, u.Host) {
			for k := range headers {
				next.Header.Del(k)
			}
		}
		return nil
	}
	res, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("Could not connect to the media server")
	}
	if encoding := res.Header.Get("Content-Encoding"); encoding != "" && !strings.EqualFold(encoding, "identity") {
		res.Body.Close()
		return nil, errors.New("The server changed the media transfer encoding")
	}
	return res, nil
}

func strongETag(s string) string {
	if len(s) >= 2 && len(s) <= 256 && s[0] == '"' && s[len(s)-1] == '"' {
		return s
	}
	return ""
}

func mediaResource(u *url.URL, signed bool) string {
	endpoint := *u
	if signed {
		endpoint.RawQuery = ""
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte(endpoint.String())))
}

func verifiedRange(res *http.Response, offset int64, original transfer, signed bool) (int64, bool) {
	if res.StatusCode != http.StatusPartialContent || strongETag(res.Header.Get("ETag")) != original.ETag ||
		mediaResource(res.Request.URL, signed) != original.Resource {
		return 0, false
	}
	value, ok := strings.CutPrefix(res.Header.Get("Content-Range"), "bytes ")
	if !ok {
		return 0, false
	}
	span, length, ok := strings.Cut(value, "/")
	if !ok {
		return 0, false
	}
	a, b, ok := strings.Cut(span, "-")
	if !ok {
		return 0, false
	}
	start, e1 := strconv.ParseInt(a, 10, 64)
	end, e2 := strconv.ParseInt(b, 10, 64)
	total, e3 := strconv.ParseInt(length, 10, 64)
	valid := e1 == nil && e2 == nil && e3 == nil && start == offset && total > start && end == total-1 &&
		(original.Size <= 0 || original.Size == total) && (res.ContentLength < 0 || res.ContentLength == total-offset)
	return total, valid
}

func mediaName(t Task, st *music.Stream, res *http.Response) (string, string) {
	ext := mediaExt("." + st.Type)
	name := t.Title
	if ext == "" {
		_, params, _ := mime.ParseMediaType(res.Header.Get("Content-Disposition"))
		filename := params["filename"]
		if ext = mediaExt(filepath.Ext(filename)); ext != "" {
			name = strings.TrimSuffix(filename, filepath.Ext(filename))
		}
	}
	if ext == "" {
		ext = mediaExt(filepath.Ext(res.Request.URL.Path))
	}
	if ext == "" {
		kind, _, _ := mime.ParseMediaType(res.Header.Get("Content-Type"))
		ext = map[string]string{"audio/flac": ".flac", "audio/mpeg": ".mp3", "audio/wav": ".wav", "audio/mp4": ".m4a", "audio/aac": ".aac", "audio/ogg": ".ogg", "video/mp4": ".mp4", "video/webm": ".webm"}[kind]
	}
	return safeName(name) + ext, ext
}

func writePart(ctx context.Context, root *os.Root, path string, source io.Reader, offset, total int64, report progress) (int64, error) {
	f, err := root.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	if err = f.Truncate(offset); err != nil {
		return 0, err
	}
	if _, err = f.Seek(offset, io.SeekStart); err != nil {
		return 0, err
	}
	reader := cancellableReader{ctx, source}
	buf := make([]byte, 64<<10)
	count, last := offset, offset
	lastAt := time.Now()
	report("Downloading", count, max(total, 0), 0)
	for {
		n, readErr := reader.Read(buf)
		if n > 0 {
			if total >= 0 && int64(n) > total-count {
				return 0, errors.New("Media length differs from the source")
			}
			if _, err = f.Write(buf[:n]); err != nil {
				return 0, err
			}
			count += int64(n)
			if elapsed := time.Since(lastAt); elapsed >= 200*time.Millisecond {
				report("Downloading", count, max(total, 0), int64(float64(count-last)/elapsed.Seconds()))
				last, lastAt = count, time.Now()
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			if ctx.Err() != nil {
				return 0, ctx.Err()
			}
			return 0, errors.New("Media download interrupted")
		}
	}
	if count == 0 || total >= 0 && count != total {
		return 0, errors.New("Media download was incomplete")
	}
	if err = f.Sync(); err != nil {
		return 0, err
	}
	report("Downloading", count, count, 0)
	return count, nil
}
