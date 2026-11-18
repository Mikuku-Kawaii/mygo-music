package downloads

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"mygo-music/internal/audioengine/decode"
)

// A generic HTTPS link can only supply the filename during search. Prefer
// embedded credits once its media has been downloaded and verified.
func (e *Engine) fileLabels(ctx context.Context, path string) Item {
	probe := e.tool("ffprobe")
	if probe == "" {
		return Item{}
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	cmd := toolCommand(ctx, probe, "-v", "error", "-show_entries", "format=duration:format_tags=title,artist,album",
		"-of", "json", "-i", path)
	data, err := cmd.Output()
	if err != nil {
		return Item{}
	}
	var result struct {
		Format struct {
			Duration string            `json:"duration"`
			Tags     map[string]string `json:"tags"`
		} `json:"format"`
	}
	if json.Unmarshal(data, &result) != nil {
		return Item{}
	}
	var item Item
	for k, v := range result.Format.Tags {
		v = strings.TrimSpace(v)
		switch strings.ToLower(k) {
		case "title":
			item.Title = v
		case "artist":
			item.Detail = v
		case "album":
			item.Album = v
		}
	}
	if seconds, err := strconv.ParseFloat(result.Format.Duration, 64); err == nil && seconds > 0 && seconds < 8640000 {
		item.Duration = int64(seconds * 1000)
	}
	return item
}

type invalidMedia string

func (m invalidMedia) Error() string { return string(m) }

func mediaExt(ext string) string {
	switch ext = strings.ToLower(ext); ext {
	case ".flac", ".wav", ".wave", ".ape", ".wv", ".alac", ".aiff", ".aif",
		".mp3", ".aac", ".m4a", ".ogg", ".oga", ".opus", ".mp4", ".mkv", ".webm", ".mov":
		return ext
	}
	return ""
}

func isLosslessName(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".flac", ".wav", ".wave", ".ape", ".wv", ".alac", ".aiff", ".aif":
		return true
	}
	return false
}

func losslessCodec(codec string) bool {
	return strings.HasPrefix(codec, "pcm_") || codec == "flac" || codec == "alac" ||
		codec == "ape" || codec == "wavpack"
}

func safeName(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || strings.ContainsRune(`<>:"/\|?*`, r) {
			return '_'
		}
		return r
	}, strings.TrimSpace(s))
	for len(s) > 190 {
		_, n := utf8.DecodeLastRuneInString(s)
		s = s[:len(s)-n]
	}
	s = strings.Trim(s, " .")
	if s == "" {
		return "download"
	}
	base, _, _ := strings.Cut(strings.ToUpper(s), ".")
	switch strings.TrimSpace(base) {
	case "CON", "PRN", "AUX", "NUL", "CONIN$", "CONOUT$",
		"COM1", "COM2", "COM3", "COM4", "COM5", "COM6", "COM7", "COM8", "COM9", "COM¹", "COM²", "COM³",
		"LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9", "LPT¹", "LPT²", "LPT³":
		return "_" + s
	}
	return s
}

func (e *Engine) verifyFiles(ctx context.Context, root *os.Root, t Task, files []stagedFile, report progress) ([]Artifact, error) {
	artifacts := make([]Artifact, 0, len(files))
	for i, file := range files {
		report("Verifying", int64(i), int64(len(files)), 0)
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		ext := mediaExt(filepath.Ext(file.name))
		if ext == "" {
			return nil, invalidMedia("Unsupported media format")
		}
		name := safeName(strings.TrimSuffix(file.name, filepath.Ext(file.name))) + ext
		fullPath := filepath.Join(root.Name(), file.path)
		switch ext {
		case ".flac", ".wav", ".wave":
			if err := verifyPCM(ctx, fullPath, ext); err != nil {
				return nil, err
			}
		default:
			if err := e.probe(ctx, fullPath, t.Format == Lossless || t.Provider == Nyaa, t.Format == Video); err != nil {
				return nil, err
			}
		}
		digest, size, err := fingerprint(ctx, root, file.path)
		if err != nil {
			return nil, err
		}
		artifacts = append(artifacts, Artifact{From: file.path, Name: name, Size: size, SHA256: digest})
	}
	return artifacts, nil
}

func fingerprint(ctx context.Context, root *os.Root, name string) (string, int64, error) {
	f, err := root.Open(name)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		return "", 0, err
	}
	if !stat.Mode().IsRegular() || stat.Size() == 0 {
		return "", 0, errors.New("Media file is empty or invalid")
	}
	h := sha256.New()
	n, err := io.CopyBuffer(h, cancellableReader{ctx, f}, make([]byte, 64<<10))
	return fmt.Sprintf("%x", h.Sum(nil)), n, err
}

func verifyPCM(ctx context.Context, path, ext string) error {
	d, err := decode.Open(ctx, path)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return invalidMedia("Media file is not a complete playable FLAC or WAV")
	}
	defer d.Close()
	info := d.Info()
	if (ext == ".flac") != strings.EqualFold(info.Container, "flac") {
		return invalidMedia("Media container differs from its file extension")
	}
	buffer := make([]int32, 4096*info.Format.Channels)
	var frames int64
	for {
		n, err := d.Read(ctx, buffer)
		frames += int64(n)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil || n == 0 {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return invalidMedia("Media file is not a complete playable FLAC or WAV")
		}
	}
	if frames == 0 || info.Frames > 0 && info.Frames != frames {
		return invalidMedia("Media file is not a complete playable FLAC or WAV")
	}
	return nil
}

func (e *Engine) probe(ctx context.Context, path string, wantLossless, wantVideo bool) error {
	probe := e.tool("ffprobe")
	if probe == "" {
		return errors.New("FFprobe is required to verify this format")
	}
	cmd := toolCommand(ctx, probe, "-v", "error", "-show_entries", "stream=codec_type,codec_name", "-of", "json", "-i", path)
	b, err := cmd.Output()
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var info struct {
		Streams []struct {
			Kind  string `json:"codec_type"`
			Codec string `json:"codec_name"`
		} `json:"streams"`
	}
	if err != nil || json.Unmarshal(b, &info) != nil {
		return invalidMedia("The saved file could not be opened as media")
	}
	for _, s := range info.Streams {
		if wantVideo && s.Kind == "video" {
			return nil
		}
		if !wantVideo && s.Kind == "audio" && (!wantLossless || losslessCodec(s.Codec)) {
			return nil
		}
		if !wantVideo && !wantLossless && s.Kind == "video" {
			return nil
		}
	}
	if wantLossless {
		return invalidMedia("The selected file does not contain lossless audio")
	}
	return invalidMedia("The expected media stream is missing")
}

func archive(ctx context.Context, root *os.Root, t Task, save checkpoint, report progress) error {
	report("Saving", 0, int64(len(t.Artifacts)), 0)
	for i := range t.Artifacts {
		a := &t.Artifacts[i]
		rel, err := filepath.Rel(t.stageDir(), a.From)
		if err != nil || !filepath.IsLocal(rel) || rel == "." ||
			!filepath.IsLocal(a.Name) || a.Name != filepath.Base(a.Name) || mediaExt(filepath.Ext(a.Name)) == "" {
			return errors.New("Invalid saved download path")
		}
		base, ext := strings.TrimSuffix(a.Name, filepath.Ext(a.Name)), filepath.Ext(a.Name)
		for suffix := 1; ; suffix++ {
			if err := ctx.Err(); err != nil {
				return err
			}
			if info, err := root.Lstat(a.Name); err == nil {
				// The journal may have been saved immediately before a crash
				// while publishing this same, already verified artifact.
				if info.Mode().IsRegular() && info.Size() == a.Size {
					part, partErr := root.Stat(a.From)
					if partErr == nil && os.SameFile(info, part) {
						break
					}
					if digest, n, err := fingerprint(ctx, root, a.Name); err == nil && n == a.Size && digest == a.SHA256 {
						break
					}
				}
				a.Name = fmt.Sprintf("%s (%d)%s", base, suffix+1, ext)
				if err := save(func(task *Task) { task.Artifacts[i].Name = a.Name }); err != nil {
					return err
				}
				continue
			} else if !errors.Is(err, os.ErrNotExist) {
				return err
			}
			if err := publishFile(root, a.From, a.Name); errors.Is(err, os.ErrExist) {
				continue
			} else if err != nil {
				return err
			}
			break
		}
		report("Saving", int64(i+1), int64(len(t.Artifacts)), 0)
	}
	return nil
}
