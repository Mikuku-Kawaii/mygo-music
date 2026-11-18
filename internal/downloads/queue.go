// Package downloads owns permanent media downloads independently of playback.
package downloads

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"
)

type Provider string

const (
	NetEase  Provider = "netease"
	HTTPS    Provider = "https"
	YouTube  Provider = "youtube"
	Bilibili Provider = "bilibili"
	Nyaa     Provider = "nyaa"
)

type Format string

const (
	Original Format = "original"
	Lossless Format = "lossless"
	Audio    Format = "audio"
	Video    Format = "video"
)

type Item struct {
	Provider    Provider `json:"provider"`
	ID          string   `json:"sourceId,omitempty"`
	URL         string   `json:"url,omitempty"`
	Title       string   `json:"title"`
	Detail      string   `json:"detail,omitempty"`
	ImageURL    string   `json:"imageUrl,omitempty"`
	Album       string   `json:"album,omitempty"`
	Duration    int64    `json:"duration,omitempty"` // milliseconds
	Group       string   `json:"group,omitempty"`
	Unavailable bool     `json:"unavailable,omitempty"`
}

type Request struct {
	Item
	Folder string `json:"folder"`
	Format Format `json:"format"`
}

type State string

const (
	Queued      State = "Queued"
	Active      State = "Active"
	Paused      State = "Paused"
	ChooseFiles State = "Choose torrent files"
	Failed      State = "Failed"
	Complete    State = "Completed"
	Canceled    State = "Canceled"
)

type Task struct {
	ID string `json:"id"`
	Request
	State     State         `json:"state"`
	Error     string        `json:"error,omitempty"`
	Stage     string        `json:"-"`
	Bytes     int64         `json:"-"`
	Total     int64         `json:"-"`
	Rate      int64         `json:"-"`
	Transfer  transfer      `json:"transfer,omitzero"`
	Torrent   string        `json:"torrent,omitempty"`
	Files     []TorrentFile `json:"files,omitempty"`
	Selected  []int         `json:"selected,omitempty"`
	Artifacts []Artifact    `json:"artifacts,omitempty"`
	Cleaned   bool          `json:"cleaned,omitempty"`
}

type transfer struct {
	ETag     string `json:"etag,omitempty"`
	Resource string `json:"resource,omitempty"`
	Size     int64  `json:"size,omitempty"`
	Ext      string `json:"ext,omitempty"`
	Name     string `json:"name,omitempty"`
}

type TorrentFile struct {
	Index int    `json:"index"`
	Name  string `json:"name"`
	Size  int64  `json:"size"`
}

type Artifact struct {
	From   string `json:"from"`
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

func (t Task) stageDir() string { return filepath.Join(".mygo-downloads", t.ID) }

func cloneTask(t Task) Task {
	t.Files, t.Selected, t.Artifacts = slices.Clone(t.Files), slices.Clone(t.Selected), slices.Clone(t.Artifacts)
	return t
}

var errChooseFiles = errors.New("torrent files are ready to select")

type checkpoint func(func(*Task)) error
type progress func(stage string, bytes, total, rate int64)

type executor interface {
	run(context.Context, Task, checkpoint, progress) error
	clean(context.Context, Task) error
}

// Queue admits one transfer at a time. Playback only supplies a read-only gate.
// Mutations and checkpoints are durable before the worker advances.
type Queue struct {
	mu               sync.Mutex
	path             string
	tasks            []Task
	err              error
	engine           executor
	busy             func() bool
	notify           func()
	wake, stop, done chan struct{}
	active           string
	cancel           context.CancelFunc
	closed           bool
	recovering       []Task
}

func Open(path string, engine *Engine, busy func() bool, notify func()) (*Queue, error) {
	return openQueue(path, engine, busy, notify)
}

func openQueue(path string, engine executor, busy func() bool, notify func()) (*Queue, error) {
	q := &Queue{path: path, engine: engine, busy: busy, notify: notify,
		wake: make(chan struct{}, 1), stop: make(chan struct{}), done: make(chan struct{})}
	b, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err == nil {
		if err := json.Unmarshal(b, &q.tasks); err != nil {
			return nil, err
		}
		for i := range q.tasks {
			t := &q.tasks[i]
			if !safeID(t.ID) || !filepath.IsAbs(t.Folder) {
				return nil, errors.New("Invalid download journal")
			}
			// Old journals wrote the queue ID over the embedded song ID.
			// The original link still identifies jobs added from a song URL.
			if t.Provider == NetEase && t.Item.ID == "" {
				if kind, id, err := neteaseLink(t.URL); err == nil && kind == "song" {
					t.Item.ID = id
				}
			}
			if t.State == Active {
				if t.Provider == Nyaa && t.Torrent != "" {
					q.recovering = append(q.recovering, cloneTask(*t))
				}
				t.State = Queued
			}
			if t.State != Complete && t.State != Canceled {
				if info, err := os.Stat(filepath.Join(t.Folder, t.stageDir(), "transfer.part")); err == nil {
					t.Bytes, t.Total = info.Size(), t.Transfer.Size
				}
			}
		}
	}
	go q.loop()
	return q, nil
}

func safeID(id string) bool {
	_, err := hex.DecodeString(id)
	return len(id) == 32 && err == nil
}

func (q *Queue) Snapshot() ([]Task, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	tasks := make([]Task, len(q.tasks))
	for i, t := range q.tasks {
		tasks[i] = cloneTask(t)
	}
	return tasks, q.err
}

func (q *Queue) PendingCount() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	n := 0
	for _, t := range q.tasks {
		if t.State != Complete && t.State != Canceled {
			n++
		}
	}
	return n
}

func (q *Queue) Enqueue(requests []Request) (int, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return 0, os.ErrClosed
	}
	previous := len(q.tasks)
	for _, r := range requests {
		if r.Unavailable || r.Group != "" || !filepath.IsAbs(r.Folder) {
			continue
		}
		if slices.ContainsFunc(q.tasks, func(t Task) bool {
			return t.State != Canceled && t.Provider == r.Provider && t.Folder == r.Folder && t.Format == r.Format &&
				((r.ID != "" && t.IDForSource() == r.ID) || (r.ID == "" && t.URL == r.URL))
		}) {
			continue
		}
		if len(q.tasks) >= 10000 {
			break
		}
		id := make([]byte, 16)
		_, _ = rand.Read(id)
		q.tasks = append(q.tasks, Task{ID: hex.EncodeToString(id), Request: r, State: Queued})
	}
	added := len(q.tasks) - previous
	if added == 0 {
		return 0, nil
	}
	if err := q.save(); err != nil {
		q.tasks = q.tasks[:previous]
		return 0, err
	}
	q.signal()
	return added, nil
}

func (t Task) IDForSource() string { return t.Request.Item.ID }

// UpdateNetEaseLabels fills the display fields of existing jobs. It leaves the
// verified artifacts, destination and transfer checkpoint as they were.
func (q *Queue) UpdateNetEaseLabels(items map[string]Item) error {
	if len(items) == 0 {
		return nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return os.ErrClosed
	}
	var revised []Task
	for i, t := range q.tasks {
		if t.Provider != NetEase || !t.Item.NeedsNetEaseLabels() {
			continue
		}
		item, ok := items[t.Item.ID]
		if !ok {
			continue
		}
		if revised == nil {
			revised = slices.Clone(q.tasks)
		}
		revised[i].Item.applyLabels(item)
	}
	if revised == nil {
		return nil
	}
	// Updating a label is optional. Its failure must not suspend transfers.
	if err := saveTasks(q.path, revised); err != nil {
		return err
	}
	q.tasks = revised
	q.signal()
	return nil
}

func (q *Queue) Pause(id string) error               { return q.change(id, Paused, nil) }
func (q *Queue) Resume(id string) error              { return q.change(id, Queued, nil) }
func (q *Queue) Cancel(id string) error              { return q.change(id, Canceled, nil) }
func (q *Queue) Select(id string, files []int) error { return q.change(id, Queued, files) }

func (q *Queue) change(id string, state State, files []int) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return os.ErrClosed
	}
	i := q.index(id)
	if i < 0 {
		return errors.New("Download task no longer exists")
	}
	t := &q.tasks[i]
	if t.State == Complete || t.State == Canceled {
		return nil
	}
	switch state {
	case Paused:
		if t.State != Active && t.State != Queued {
			return nil
		}
	case Queued:
		if t.State == ChooseFiles {
			if len(files) == 0 || slices.ContainsFunc(files, func(index int) bool {
				return !slices.ContainsFunc(t.Files, func(f TorrentFile) bool { return f.Index == index && isLosslessName(f.Name) })
			}) {
				return errors.New("Choose at least one lossless audio file")
			}
		} else if t.State != Paused && t.State != Failed {
			return nil
		}
	}
	old := *t
	t.State, t.Error = state, ""
	if files != nil {
		t.Selected = slices.Clone(files)
	}
	if err := q.save(); err != nil {
		*t = old
		return err
	}
	if q.active == id && q.cancel != nil && state != Queued {
		q.cancel()
	}
	q.signal()
	return nil
}

// Forget removes a terminal entry from history and leaves archived files alone.
func (q *Queue) Forget(id string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	i := q.index(id)
	if i < 0 || q.closed {
		return nil
	}
	t := q.tasks[i]
	if (t.State != Complete && t.State != Canceled) || !t.Cleaned {
		return nil
	}
	old := slices.Clone(q.tasks)
	q.tasks = slices.Delete(q.tasks, i, i+1)
	if err := q.save(); err != nil {
		q.tasks = old
		return err
	}
	q.signal()
	return nil
}

func (q *Queue) RetryCleanup(id string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	i := q.index(id)
	if i < 0 || q.closed {
		return nil
	}
	t := &q.tasks[i]
	if (t.State != Complete && t.State != Canceled) || t.Cleaned {
		return nil
	}
	previous := t.Error
	t.Error = ""
	if err := q.save(); err != nil {
		t.Error = previous
		return err
	}
	q.signal()
	return nil
}

func (q *Queue) index(id string) int {
	return slices.IndexFunc(q.tasks, func(t Task) bool { return t.ID == id })
}

func (q *Queue) signal() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
	if q.notify != nil {
		q.notify()
	}
}

// Close saves the pending intent and lets the active worker release its file or
// external process before the next app instance can resume the same task.
func (q *Queue) Close() {
	q.mu.Lock()
	if !q.closed {
		q.closed = true
		if q.cancel != nil {
			q.cancel()
		}
		close(q.stop)
	}
	q.mu.Unlock()
	<-q.done
}

func (q *Queue) loop() {
	defer close(q.done)
	q.recoverTorrents()
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	completed := make(chan struct{})
	for {
		select {
		case <-q.stop:
			q.mu.Lock()
			active := q.active != ""
			q.mu.Unlock()
			if active {
				<-completed
			}
			return
		case <-completed:
			q.mu.Lock()
			q.cancel()
			q.active, q.cancel = "", nil
			q.mu.Unlock()
			q.signal()
		case <-tick.C:
		case <-q.wake:
		}
		q.schedule(completed)
	}
}

func (q *Queue) recoverTorrents() {
	engine, ok := q.engine.(*Engine)
	if !ok {
		return
	}
	for _, t := range q.recovering {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		err := engine.stopTorrent(ctx, t)
		cancel()
		if err != nil {
			q.mu.Lock()
			current := &q.tasks[q.index(t.ID)]
			if current.State == Queued {
				current.State, current.Error = Failed, err.Error()
				q.save()
			}
			q.mu.Unlock()
		}
		q.signal()
	}
	q.recovering = nil
}

func (q *Queue) schedule(completed chan<- struct{}) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return
	}
	if q.busy != nil && q.busy() {
		if q.cancel != nil {
			q.cancel()
		}
		return
	}
	if q.active != "" || q.err != nil {
		return
	}
	i := slices.IndexFunc(q.tasks, func(t Task) bool { return t.State == Queued })
	if i < 0 {
		i = slices.IndexFunc(q.tasks, func(t Task) bool {
			return (t.State == Complete || t.State == Canceled) && !t.Cleaned && t.Error == ""
		})
		if i < 0 {
			return
		}
		t := cloneTask(q.tasks[i])
		ctx, cancel := context.WithCancel(context.Background())
		q.active, q.cancel = t.ID, cancel
		go func() { q.clean(ctx, t); completed <- struct{}{} }()
		return
	}
	t := &q.tasks[i]
	t.State, t.Stage = Active, "Resolving"
	if err := q.save(); err != nil {
		t.State = Queued
		q.signal()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	q.active, q.cancel = t.ID, cancel
	go q.run(ctx, cloneTask(*t), completed)
	q.signal()
}

func (q *Queue) run(ctx context.Context, t Task, completed chan<- struct{}) {
	save := func(edit func(*Task)) error {
		q.mu.Lock()
		defer q.mu.Unlock()
		if err := ctx.Err(); err != nil {
			return err
		}
		current := &q.tasks[q.index(t.ID)]
		before := cloneTask(*current)
		edit(current)
		if err := q.save(); err != nil {
			*current = before
			return err
		}
		q.signal()
		return nil
	}
	update := func(stage string, bytes, total, rate int64) {
		q.mu.Lock()
		defer q.mu.Unlock()
		current := &q.tasks[q.index(t.ID)]
		current.Stage, current.Bytes, current.Total, current.Rate = stage, bytes, total, rate
		q.signal()
	}
	err := q.engine.run(ctx, t, save, update)
	q.mu.Lock()
	current := &q.tasks[q.index(t.ID)]
	current.Stage, current.Rate = "", 0
	switch {
	case err == nil:
		current.State, current.Error = Complete, ""
	case current.State == Paused || current.State == Canceled:
	case errors.Is(err, errChooseFiles):
		current.State = ChooseFiles
	case ctx.Err() != nil:
		current.State = Queued
	default:
		current.State, current.Error = Failed, err.Error()
	}
	persisted := q.save() == nil
	clean := persisted && (current.State == Complete || current.State == Canceled)
	copied := cloneTask(*current)
	q.mu.Unlock()
	if clean {
		q.clean(ctx, copied)
	}
	completed <- struct{}{}
}

func (q *Queue) clean(parent context.Context, t Task) {
	ctx, cancel := context.WithTimeout(parent, 8*time.Second)
	defer cancel()
	err := q.engine.clean(ctx, t)
	q.mu.Lock()
	defer q.mu.Unlock()
	current := &q.tasks[q.index(t.ID)]
	current.Cleaned = err == nil
	if err != nil && parent.Err() == nil {
		current.Error = err.Error()
	}
	q.save()
}

// save is called under mu. Only the small journal, never the media bytes, is
// replaced here. A failed write blocks admission rather than losing intent.
func (q *Queue) save() error {
	err := saveTasks(q.path, q.tasks)
	if err != nil {
		q.err = fmt.Errorf("Could not save the download queue: %w", err)
	} else {
		q.err = nil
	}
	return q.err
}

func saveTasks(path string, tasks []Task) error {
	b, err := json.Marshal(tasks)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), "downloads-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
