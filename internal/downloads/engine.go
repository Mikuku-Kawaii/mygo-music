package downloads

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"mygo-music/internal/music"
)

// NetEaseMedia uses the existing source or a read-only lease on its cache.
// A resolved URL or source header lives only as long as one transfer.
type NetEaseMedia struct {
	Stream *music.Stream
	File   *os.File
}

type Engine struct {
	NetEase    func(context.Context, Request) (NetEaseMedia, error)
	HTTP       *http.Client
	ToolDir    string
	BitTorrent func(context.Context) (*QBClient, error)
}

type stagedFile struct{ path, name string }

func (e *Engine) run(ctx context.Context, t Task, save checkpoint, report progress) error {
	if err := os.MkdirAll(t.Folder, 0755); err != nil {
		return err
	}
	root, err := os.OpenRoot(t.Folder)
	if err != nil {
		return err
	}
	defer root.Close()
	if len(t.Artifacts) > 0 {
		return archive(ctx, root, t, save, report)
	}
	if err := root.MkdirAll(t.stageDir(), 0700); err != nil {
		return err
	}
	var files []stagedFile
	switch t.Provider {
	case HTTPS, NetEase:
		var file stagedFile
		file, err = e.fetchFile(ctx, root, t, save, report)
		files = []stagedFile{file}
	case YouTube, Bilibili:
		files, err = e.videoTransfer(ctx, root, t, report)
	case Nyaa:
		files, err = e.torrentTransfer(ctx, root, t, save, report)
	default:
		return errors.New("Unsupported download source")
	}
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return errors.New("Download returned no media")
	}
	artifacts, err := e.verifyFiles(ctx, root, t, files, report)
	if err != nil {
		var invalid invalidMedia
		if t.Provider != Nyaa && errors.As(err, &invalid) {
			for _, f := range files {
				if removeErr := root.Remove(f.path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
					return removeErr
				}
			}
			if saveErr := save(func(task *Task) { task.Transfer = transfer{} }); saveErr != nil {
				return saveErr
			}
		}
		return err
	}
	var labels Item
	if t.Provider == HTTPS && len(files) == 1 {
		labels = e.fileLabels(ctx, filepath.Join(root.Name(), files[0].path))
	}
	if err := save(func(t *Task) {
		t.Artifacts = artifacts
		t.Item.applyLabels(labels)
	}); err != nil {
		return err
	}
	t.Artifacts = artifacts
	return archive(ctx, root, t, save, report)
}

func (e *Engine) clean(ctx context.Context, t Task) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if t.Provider == Nyaa && t.Torrent != "" {
		if err := e.releaseTorrent(ctx, t); err != nil {
			return err
		}
	}
	root, err := os.OpenRoot(t.Folder)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer root.Close()
	return root.RemoveAll(t.stageDir())
}

func (e *Engine) fetchFile(ctx context.Context, root *os.Root, t Task, save checkpoint, report progress) (stagedFile, error) {
	part := filepath.Join(t.stageDir(), "transfer.part")
	info, err := root.Stat(part)
	if err == nil && info.Mode().IsRegular() && t.Transfer.Size > 0 && info.Size() == t.Transfer.Size && t.Transfer.Ext != "" {
		name := t.Transfer.Name
		if name == "" {
			name = safeName(t.Title) + t.Transfer.Ext
		}
		return stagedFile{part, name}, nil
	}
	var st *music.Stream
	if t.Provider == NetEase {
		if e.NetEase == nil {
			return stagedFile{}, errors.New("Enable the NetEase Music source first")
		}
		media, err := e.NetEase(ctx, t.Request)
		if err != nil {
			return stagedFile{}, err
		}
		if media.File != nil {
			defer media.File.Close()
		}
		st = media.Stream
		if st == nil {
			return stagedFile{}, errors.New("This track has no downloadable audio")
		}
		if t.Format == Lossless && (st.Quality != "lossless" || !strings.EqualFold(st.Type, "flac")) {
			return stagedFile{}, errors.New("This track has no downloadable lossless FLAC")
		}
		if media.File != nil {
			ext := mediaExt("." + st.Type)
			if ext == "" {
				return stagedFile{}, errors.New("Unsupported media format")
			}
			if err := save(func(t *Task) { t.Transfer = transfer{Ext: ext} }); err != nil {
				return stagedFile{}, err
			}
			total := st.Size
			if total <= 0 {
				total = -1
			}
			n, err := writePart(ctx, root, part, media.File, 0, total, report)
			if err != nil {
				return stagedFile{}, err
			}
			if err := save(func(t *Task) { t.Transfer.Size = n }); err != nil {
				return stagedFile{}, err
			}
			return stagedFile{part, safeName(t.Title) + ext}, nil
		}
	} else {
		st = &music.Stream{URL: t.URL}
	}
	return e.httpTransfer(ctx, root, part, t, st, save, report)
}

type cancellableReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r cancellableReader) Read(b []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(b)
}
