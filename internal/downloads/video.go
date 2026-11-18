package downloads

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

func (e *Engine) tool(name string) string {
	file := name
	if runtime.GOOS == "windows" {
		file += ".exe"
	}
	if e.ToolDir != "" {
		p := filepath.Join(e.ToolDir, file)
		if info, err := os.Stat(p); err == nil && info.Mode().IsRegular() {
			return p
		}
	}
	p, _ := exec.LookPath(name)
	return p
}

func (e *Engine) videoArgs(p Provider) (path string, args []string, err error) {
	path = e.tool("yt-dlp")
	if path == "" {
		return "", nil, errors.New("Install yt-dlp to use YouTube or Bilibili downloads")
	}
	args = []string{"--ignore-config", "--no-plugin-dirs", "--no-cache-dir", "--no-remote-components",
		"--socket-timeout", "15", "--retries", "2", "--fragment-retries", "2", "--no-color"}
	if p == YouTube {
		runtimePath := e.tool("deno")
		runtimeName := "deno"
		if runtimePath == "" {
			runtimePath, runtimeName = e.tool("node"), "node"
		}
		if runtimePath == "" {
			return "", nil, errors.New("YouTube requires Deno or a supported Node.js runtime")
		}
		args = append(args, "--no-js-runtimes", "--js-runtimes", runtimeName+":"+runtimePath)
	}
	if ffmpeg := e.tool("ffmpeg"); ffmpeg != "" {
		args = append(args, "--ffmpeg-location", ffmpeg)
	}
	return path, args, nil
}

func videoURL(provider Provider, raw string) (string, bool) {
	u, err := httpsURL(strings.TrimSpace(raw))
	if err != nil || u.Port() != "" {
		return "", false
	}
	host := strings.ToLower(u.Hostname())
	allowed := false
	switch provider {
	case YouTube:
		allowed = host == "youtu.be" || host == "youtube.com" || strings.HasSuffix(host, ".youtube.com")
	case Bilibili:
		allowed = host == "b23.tv" || host == "bilibili.com" || strings.HasSuffix(host, ".bilibili.com")
	}
	return u.String(), allowed
}

type videoEntry struct {
	ID       string        `json:"id"`
	Title    string        `json:"title"`
	URL      string        `json:"url"`
	Page     string        `json:"webpage_url"`
	Uploader string        `json:"uploader"`
	Channel  string        `json:"channel"`
	Entries  []*videoEntry `json:"entries"`
}

func (e *Engine) searchVideo(ctx context.Context, provider Provider, query string) (Results, error) {
	path, args, err := e.videoArgs(provider)
	if err != nil {
		return Results{}, err
	}
	input := []string{query}
	if strings.Contains(query, "://") {
		input = strings.Fields(query)
		if len(input) > 25 {
			return Results{}, errors.New("Search up to 25 video links at a time")
		}
		for i, u := range input {
			link, ok := videoURL(provider, u)
			if !ok {
				return Results{}, errors.New("Enter a link for the selected video platform")
			}
			input[i] = link
		}
	} else {
		prefix := "ytsearch20:"
		if provider == Bilibili {
			prefix = "bilisearch20:"
		}
		input[0] = prefix + query
	}
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	result := Results{}
	for _, target := range input {
		options := append(append([]string(nil), args...), "-J", "--flat-playlist", "--playlist-end", "100", "--", target)
		cmd := toolCommand(ctx, path, options...)
		var out, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &stderr
		stop, err := startTool(cmd)
		if err != nil {
			return Results{}, videoProblem(ctx, stderr.String(), err)
		}
		err = cmd.Wait()
		stop()
		if err != nil {
			return Results{}, videoProblem(ctx, stderr.String(), err)
		}
		var top videoEntry
		if json.Unmarshal(out.Bytes(), &top) != nil {
			return Results{}, errors.New("Could not read the video search results")
		}
		entries := top.Entries
		if entries == nil {
			entries = []*videoEntry{&top}
		}
		for _, entry := range entries {
			if entry == nil || entry.Title == "" {
				continue
			}
			page, ok := videoURL(provider, entry.Page)
			if !ok {
				page, ok = videoURL(provider, entry.URL)
			}
			if !ok && provider == YouTube && entry.ID != "" && !strings.ContainsAny(entry.ID, "/?&#:") {
				page, ok = videoURL(provider, "https://www.youtube.com/watch?v="+entry.ID)
			}
			if !ok && entries[0] == &top {
				page, ok = videoURL(provider, target)
			}
			if !ok {
				continue
			}
			detail := entry.Uploader
			if detail == "" {
				detail = entry.Channel
			}
			result.Items = append(result.Items, Item{Provider: provider, Title: html.UnescapeString(entry.Title), Detail: detail, URL: page})
		}
	}
	return result, nil
}

func (e *Engine) videoTransfer(ctx context.Context, root *os.Root, t Task, report progress) ([]stagedFile, error) {
	u, ok := videoURL(t.Provider, t.URL)
	if !ok {
		return nil, errors.New("Enter a link for the selected video platform")
	}
	path, args, err := e.videoArgs(t.Provider)
	if err != nil {
		return nil, err
	}
	format := "bestaudio"
	if t.Format == Video {
		format = "best"
		if e.tool("ffmpeg") != "" {
			format = "bestvideo+bestaudio/best"
		}
	}
	abs := filepath.Join(root.Name(), t.stageDir())
	args = append(args, "--continue", "--no-playlist", "--no-overwrites", "--no-simulate", "--abort-on-unavailable-fragments",
		"--progress", "--newline", "--progress-delta", "0.3", "--progress-template",
		"download:MYGO_PROGRESS %(progress.downloaded_bytes)s %(progress.total_bytes,progress.total_bytes_estimate)s %(progress.speed)s",
		"--print", "after_move:MYGO_FILE %(filepath)j",
		"-f", format, "-P", abs, "-o", "media.%(ext)s", "--", u)
	cmd := toolCommand(ctx, path, args...)
	raw, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stop, err := startTool(cmd)
	if err != nil {
		return nil, videoProblem(ctx, stderr.String(), err)
	}
	defer stop()
	scan := bufio.NewScanner(raw)
	scan.Buffer(make([]byte, 16<<10), 256<<10)
	final := ""
	for scan.Scan() {
		line := scan.Text()
		if values, ok := strings.CutPrefix(line, "MYGO_PROGRESS "); ok {
			v := strings.Fields(values)
			if len(v) == 3 {
				report("Downloading", positive(v[0]), positive(v[1]), positive(v[2]))
			}
		}
		if values, ok := strings.CutPrefix(line, "MYGO_FILE "); ok {
			_ = json.Unmarshal([]byte(values), &final)
		}
	}
	if scan.Err() != nil {
		stop()
		raw.Close()
	}
	if err := cmd.Wait(); err != nil {
		return nil, videoProblem(ctx, stderr.String(), err)
	}
	rel, err := filepath.Rel(abs, final)
	ext := mediaExt(filepath.Ext(rel))
	if err != nil || !filepath.IsLocal(rel) || rel != filepath.Base(rel) || !strings.HasPrefix(rel, "media.") || ext == "" {
		return nil, errors.New("The video downloader returned no usable file")
	}
	return []stagedFile{{filepath.Join(t.stageDir(), rel), safeName(t.Title) + ext}}, nil
}

func positive(s string) int64 {
	n, err := strconv.ParseFloat(s, 64)
	if err != nil || !(n > 0 && n < 1<<62) {
		return 0
	}
	return int64(n)
}

func videoProblem(ctx context.Context, stderr string, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	stderr = strings.ToLower(stderr)
	switch {
	case strings.Contains(stderr, "sign in"), strings.Contains(stderr, "login required"), strings.Contains(stderr, "members-only"):
		return errors.New("The video platform requires sign-in or access to this item")
	case strings.Contains(stderr, "http error 403"), strings.Contains(stderr, "http error 412"), strings.Contains(stderr, "http error 429"):
		return errors.New("The video platform rejected or limited this request")
	case strings.Contains(stderr, "ffmpeg is not installed"):
		return errors.New("FFmpeg is required to merge this video")
	case strings.Contains(stderr, "javascript runtime"), strings.Contains(stderr, "challenge solving"):
		return errors.New("YouTube could not resolve streams with the available JavaScript runtime")
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return fmt.Errorf("Video extraction failed (yt-dlp exit %d)", exit.ExitCode())
	}
	return errors.New("Could not start yt-dlp")
}
