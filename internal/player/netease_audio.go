package player

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"mygo-music/internal/audioengine/decode"
	"mygo-music/internal/i18n"
	"mygo-music/internal/music"
)

const neteaseSource = "netease"

// Keep the file open until the Windows transport closes. This protects its
// name from cache eviction while the native decoder may reopen it for a seek.
type neteaseFile struct {
	*os.File
	temporary bool
}

func (f *neteaseFile) release() {
	if f == nil {
		return
	}
	f.Close()
	if f.temporary {
		os.Remove(f.Name())
	}
}

func isLosslessFLAC(st *music.Stream) bool {
	return st != nil && st.Quality == "lossless" && strings.EqualFold(st.Type, "flac")
}

func neteaseStreamType(st *music.Stream, quality string) string {
	if st == nil || quality == "lossless" && !isLosslessFLAC(st) {
		return ""
	}
	typ := strings.ToLower(st.Type)
	switch typ {
	case "flac", "mp3", "ogg", "m4a", "mp4", "aac":
		return typ
	}
	return ""
}

func neteaseStreamInfo(st *music.Stream, typ, requested string, size int64) *music.Stream {
	info := *st
	info.URL, info.Headers, info.Type, info.Size = "", nil, typ, size
	if !isLosslessFLAC(st) && info.Quality != "high" && info.Quality != "standard" {
		info.Quality = requested
	}
	return &info
}

func (c *Cache) neteaseCachedAudio(ctx context.Context, id, quality string, stereo bool) (*neteaseFile, *music.Stream) {
	if c == nil {
		return nil, nil
	}
	legacy := &Cache{dir: c.fallbackSongs}
	for _, cache := range []*Cache{c, legacy} {
		if cache.dir == "" || ctx.Err() != nil {
			continue
		}
		cache.mu.Lock()
		f, st, ok := cache.find(neteaseSource, id, quality)
		cache.mu.Unlock()
		if !ok {
			continue
		}
		typ := neteaseStreamType(st, quality)
		if typ == "" || validateNeteaseAudio(ctx, f.Name(), typ, stereo, false) != nil {
			f.Close()
			continue
		}
		if cache == c {
			now := time.Now()
			os.Chtimes(f.Name(), now, now)
		}
		return &neteaseFile{File: f}, neteaseStreamInfo(st, typ, quality, st.Size)
	}
	return nil, nil
}

// OpenNetEaseDownloadCache hands the downloads workspace a read lease on an
// existing FLAC. It never starts or joins a playback load.
func (c *Cache) OpenNetEaseDownloadCache(ctx context.Context, id string) (*os.File, *music.Stream) {
	if f, st := c.neteaseCachedAudio(ctx, id, "lossless", false); f != nil {
		return f.File, st
	}
	return nil, nil
}

// Playback can use any complete version the account can access. Permanent
// downloads choose a quality independently through the downloads workspace.
func (c *Cache) openNetEasePlayback(ctx context.Context, src music.Source, id string, mode OutputMode) (*neteaseFile, *music.Stream, error) {
	if src == nil {
		return nil, nil, errNoSource
	}
	var last error
	for _, quality := range [...]string{"lossless", "high", "standard"} {
		f, st, err := c.openNetEaseQuality(ctx, src, id, quality, mode != OutputCompatible)
		if err == nil || ctx.Err() != nil {
			return f, st, err
		}
		last = err
	}
	return nil, nil, last
}

func (c *Cache) openNetEaseQuality(ctx context.Context, src music.Source, id, quality string, stereo bool) (*neteaseFile, *music.Stream, error) {
	if f, st := c.neteaseCachedAudio(ctx, id, quality, stereo); f != nil {
		return f, st, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	st, err := src.Stream(id, quality)
	if canceled := ctx.Err(); canceled != nil {
		return nil, nil, canceled
	}
	if err != nil {
		return nil, nil, err
	}
	typ := neteaseStreamType(st, quality)
	if typ == "" || st.URL == "" {
		if quality == "lossless" {
			return nil, nil, errors.New(i18n.T("NetEase did not return a lossless FLAC for this track"))
		}
		return nil, nil, music.ErrUnavailable
	}
	dir := ""
	if c != nil {
		dir = c.dir
		if err = os.MkdirAll(dir, 0o700); err != nil {
			return nil, nil, err
		}
	}
	tmp, err := os.CreateTemp(dir, "netease-*.part")
	if err != nil {
		return nil, nil, err
	}
	part := tmp.Name()
	defer func() {
		if part != "" {
			os.Remove(part)
		}
	}()
	n, err := fetchNeteaseAudio(ctx, tmp, st)
	closeErr := tmp.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return nil, nil, err
	}
	if err = validateNeteaseAudio(ctx, part, typ, stereo, true); err != nil {
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		if typ == "flac" {
			return nil, nil, errors.New(i18n.T("The downloaded NetEase file is not a complete playable FLAC"))
		}
		return nil, nil, errors.New(i18n.T("The downloaded NetEase audio could not be decoded"))
	}
	if err = ctx.Err(); err != nil {
		return nil, nil, err
	}
	info := neteaseStreamInfo(st, typ, quality, n)
	path, kept := part, false
	if c != nil {
		c.mu.Lock()
		defer c.mu.Unlock()
		path, kept = c.keepLocked(neteaseSource, id, quality, info, part, n)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	part = "" // the cache or the transport now owns the file
	return &neteaseFile{File: f, temporary: !kept}, info, nil
}

func openNetEaseCompatible(ctx context.Context, cache *Cache, src music.Source, t music.Track, pre *neteaseAhead) (*music.Stream, stream, error) {
	file, info, err := pre.take(ctx, cache, src, t.ID, OutputCompatible)
	if err != nil {
		return nil, nil, err
	}
	dl := opened(file.File, info.Size)
	if file.temporary {
		dl.part = file.Name()
	}
	s, err := openStream(dl, t.Duration)
	if err != nil {
		dl.Close()
		return nil, nil, err
	}
	return info, s, nil
}

func fetchNeteaseAudio(ctx context.Context, dst io.Writer, st *music.Stream) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, st.URL, nil)
	if err != nil || (req.URL.Scheme != "https" && req.URL.Scheme != "http") || req.URL.Hostname() == "" {
		return 0, errors.New(i18n.T("NetEase returned an invalid audio address"))
	}
	req.Header.Set("User-Agent", music.Agent)
	for k, v := range st.Headers {
		req.Header.Set(k, v)
	}
	res, err := music.HeadersStay(http.DefaultClient, req.URL.Hostname(), st.Headers).Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		return 0, errors.New(i18n.T("Could not connect to the NetEase audio server"))
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return 0, fmt.Errorf(i18n.T("NetEase audio request failed (HTTP %d)"), res.StatusCode)
	}
	badLength := errors.New(i18n.T("The NetEase audio download did not match the expected length"))
	size := st.Size
	if size <= 0 {
		size = res.ContentLength
	} else if res.ContentLength >= 0 && res.ContentLength != size {
		return 0, badLength
	}
	limit := size
	if limit <= 0 {
		// Both lengths can be absent for chunked transfer. Bound only this case.
		limit = 512 << 20
	}
	n, err := io.Copy(dst, io.LimitReader(res.Body, limit))
	if ctx.Err() != nil {
		return 0, ctx.Err()
	}
	if err != nil || n == 0 || (size > 0 && n != size) {
		return 0, badLength
	}
	if n == limit {
		var extra [1]byte
		if _, err = io.ReadFull(res.Body, extra[:]); err != io.EOF {
			if ctx.Err() != nil {
				return 0, ctx.Err()
			}
			return 0, badLength
		}
	}
	return n, nil
}

func validateNeteaseAudio(ctx context.Context, path, typ string, stereo, complete bool) error {
	open := decode.OpenProcessed
	if typ == "flac" {
		open = decode.Open
	}
	d, err := open(ctx, path)
	if err != nil {
		return err
	}
	defer d.Close()
	info := d.Info()
	if typ == "flac" && info.Container != "FLAC" || stereo && info.Format.Channels != 2 {
		return decode.ErrUnsupported
	}
	samples := make([]int32, 4096*info.Format.Channels)
	var frames int64
	for {
		n, err := d.Read(ctx, samples)
		frames += int64(n)
		if err == io.EOF && frames > 0 {
			return nil
		}
		if err != nil {
			return err
		}
		if !complete && n > 0 {
			return nil
		}
	}
}
