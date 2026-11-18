package controller

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"mygo-music/internal/audioengine/pcm"
)

type fakeTransport struct {
	mu        sync.Mutex
	state     State
	position  uint64
	playing   bool
	closed    int
	starts    int
	failure   error
	closeGate <-chan struct{}
	closing   chan struct{}
}

func (f *fakeTransport) setPlaying(ctx context.Context, playing bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failure != nil {
		return f.failure
	}
	if f.state == Ended {
		return nil
	}
	f.playing = playing
	if playing {
		f.state = Playing
		f.starts++
	} else {
		f.state = Paused
	}
	return nil
}
func (f *fakeTransport) observe() (observation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return observation{state: f.state, playing: f.playing, position: f.position, exact: true}, f.failure
}
func (f *fakeTransport) close() error {
	if f.closing != nil {
		f.closing <- struct{}{}
	}
	if f.closeGate != nil {
		<-f.closeGate
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed++
	f.playing = false
	return nil
}
func (f *fakeTransport) values() (closed, starts int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed, f.starts
}
func media(id string) Media {
	return Media{Item: Item{Source: "local", ID: id, Title: id}, Path: id + ".wav"}
}
func newTestHandle(t *testing.T, open opener) *EngineHandle {
	t.Helper()
	h, err := newHandle(Config{}, open)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { h.Close() })
	return h
}
func await(t *testing.T, r *Receipt, err error) Result {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	result, err := r.Wait(ctx)
	if err != nil {
		t.Fatal(err, result)
	}
	return result
}
func waitSnapshot(t *testing.T, h *EngineHandle, predicate func(Snapshot) bool) Snapshot {
	t.Helper()
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	for {
		s := h.Snapshot()
		if predicate(s) {
			return s
		}
		select {
		case <-h.Changes():
		case <-timer.C:
			t.Fatal("snapshot did not arrive", s)
		}
	}
}
func waitSignal(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("worker did not reach barrier")
	}
}

// Admission remains responsive while preparation is blocked, and neither a
// queue replacement nor a later Pause can publish the old command as applied.
func TestAcceptedIsNotAppliedAndPauseDuringPrepare(t *testing.T) {
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	var mu sync.Mutex
	var sessions []*fakeTransport
	h := newTestHandle(t, func(ctx context.Context, _ Config, _ Media, _ pcm.Generation, _ time.Duration) (transport, error) {
		entered <- struct{}{}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-release:
		}
		f := &fakeTransport{state: Paused}
		mu.Lock()
		sessions = append(sessions, f)
		mu.Unlock()
		return f, nil
	})
	load, err := h.SetQueue(context.Background(), []Media{media("a")}, 0)
	if err != nil {
		t.Fatal(err)
	}
	waitSignal(t, entered)
	if s := h.Snapshot(); s.State != Preparing || s.AppliedCommand != 0 || s.Current.ID != "" {
		t.Fatal(s)
	}
	select {
	case <-load.Done():
		t.Fatal("reported applied before preparation")
	default:
	}
	playCmd, err := h.Play(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	pauseCmd, err := h.Pause(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	waitSnapshot(t, h, func(s Snapshot) bool { return s.RequestedCommand == pauseCmd.ID() && !s.DesiredPlaying })
	// Open the gate once, before awaiting the last command. Every prepared
	// stream must respect the latest intent, including one opened after cancel.
	releaseOnce.Do(func() { close(release) })
	result := await(t, pauseCmd, nil)
	if result.Snapshot.State != Paused || result.Snapshot.EffectivePlaying {
		t.Fatal(result)
	}
	if _, err = load.Wait(context.Background()); !errors.Is(err, ErrSuperseded) {
		t.Fatal(err)
	}
	if _, err = playCmd.Wait(context.Background()); !errors.Is(err, ErrSuperseded) {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, f := range sessions {
		_, starts := f.values()
		if starts != 0 {
			t.Fatal("started despite paused intent")
		}
	}
}

// Even an opener that returns a late success after cancellation cannot replace
// B with A; its resources are closed before another device is opened.
func TestLateSourceIsDiscarded(t *testing.T) {
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	a, b := &fakeTransport{state: Paused}, &fakeTransport{state: Paused}
	h := newTestHandle(t, func(_ context.Context, _ Config, m Media, _ pcm.Generation, _ time.Duration) (transport, error) {
		if m.ID == "a" {
			entered <- struct{}{}
			<-release
			return a, nil
		}
		if closed, _ := a.values(); closed != 1 {
			t.Error("old source not released before new open")
		}
		return b, nil
	})
	first, err := h.SetQueue(context.Background(), []Media{media("a")}, 0)
	if err != nil {
		t.Fatal(err)
	}
	waitSignal(t, entered)
	second, err := h.SetQueue(context.Background(), []Media{media("b")}, 0)
	if err != nil {
		t.Fatal(err)
	}
	waitSnapshot(t, h, func(s Snapshot) bool { return s.Selected.ID == "b" })
	releaseOnce.Do(func() { close(release) })
	r := await(t, second, nil)
	if r.Snapshot.Current.ID != "b" || r.Snapshot.AppliedGeneration.Session != 2 {
		t.Fatal(r)
	}
	if _, err = first.Wait(context.Background()); !errors.Is(err, ErrSuperseded) {
		t.Fatal(err)
	}
	if closed, starts := a.values(); closed != 1 || starts != 0 {
		t.Fatal(closed, starts)
	}
}

// The caller cannot mutate an accepted queue through either its original slice
// or a returned queue view; stop/replay leaves identity and revision intact.
func TestQueueOwnershipAndLifecycle(t *testing.T) {
	var sessions []*fakeTransport
	var mu sync.Mutex
	h := newTestHandle(t, func(context.Context, Config, Media, pcm.Generation, time.Duration) (transport, error) {
		f := &fakeTransport{state: Paused}
		mu.Lock()
		sessions = append(sessions, f)
		mu.Unlock()
		return f, nil
	})
	queue := []Media{media("a"), media("b")}
	cmd, err := h.SetQueue(context.Background(), queue, 0)
	queue[0] = media("mutated")
	loaded := await(t, cmd, err)
	if loaded.Snapshot.Current.ID != "a" {
		t.Fatal(loaded)
	}
	view, rev := h.Queue()
	view[0].ID = "changed"
	again, rev2 := h.Queue()
	if again[0].ID != "a" || rev != rev2 {
		t.Fatal(again, rev, rev2)
	}
	cmd, err = h.Play(context.Background())
	playing := await(t, cmd, err)
	if !playing.Snapshot.EffectivePlaying {
		t.Fatal(playing)
	}
	cmd, err = h.Pause(context.Background())
	paused := await(t, cmd, err)
	if paused.Snapshot.State != Paused || paused.Snapshot.DesiredPlaying {
		t.Fatal(paused)
	}
	cmd, err = h.Select(context.Background(), 1)
	selected := await(t, cmd, err)
	if selected.Snapshot.Current.ID != "b" || selected.Snapshot.EffectivePlaying || selected.Snapshot.RequestedGeneration.Queue != rev {
		t.Fatal(selected)
	}
	cmd, err = h.Stop(context.Background())
	stopped := await(t, cmd, err)
	if stopped.Snapshot.State != Idle || stopped.Snapshot.Current.ID != "" || stopped.Snapshot.Selected.ID != "b" {
		t.Fatal(stopped)
	}
	cmd, err = h.Play(context.Background())
	restarted := await(t, cmd, err)
	if restarted.Snapshot.Current.ID != "b" || !restarted.Snapshot.EffectivePlaying || restarted.Snapshot.AppliedGeneration.Format <= selected.Snapshot.AppliedGeneration.Format {
		t.Fatal(restarted)
	}
	if err = h.Close(); err != nil {
		t.Fatal(err)
	}
	if h.Snapshot().State != Closed {
		t.Fatal(h.Snapshot())
	}
	mu.Lock()
	defer mu.Unlock()
	for _, f := range sessions {
		if n, _ := f.values(); n != 1 {
			t.Fatal("session not closed exactly once", n)
		}
	}
}

// Cancel invalidates only the pending receipt, retains the selected queue,
// joins preparation, and never revokes an unrelated newer command.
func TestCancelPendingCommand(t *testing.T) {
	entered := make(chan struct{}, 1)
	h := newTestHandle(t, func(ctx context.Context, _ Config, _ Media, _ pcm.Generation, _ time.Duration) (transport, error) {
		entered <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	})
	load, err := h.SetQueue(context.Background(), []Media{media("a")}, 0)
	if err != nil {
		t.Fatal(err)
	}
	waitSignal(t, entered)
	cancelCmd, err := h.Cancel(context.Background(), load.ID())
	canceled := await(t, cancelCmd, err)
	if canceled.Snapshot.State != Idle || canceled.Snapshot.DesiredPlaying || canceled.Snapshot.Selected.ID != "a" {
		t.Fatal(canceled)
	}
	if result, err := load.Wait(context.Background()); !errors.Is(err, context.Canceled) || result.Status != Canceled {
		t.Fatal(result, err)
	}
	stale, err := h.Cancel(context.Background(), load.ID())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = stale.Wait(context.Background()); !errors.Is(err, ErrNotPending) {
		t.Fatal(err)
	}
}

// Waiting with a canceled context does not undo an already accepted command;
// completion remains readable by multiple callers and cannot be consumed once.
func TestReceiptWaitIsRepeatable(t *testing.T) {
	f := &fakeTransport{state: Paused}
	h := newTestHandle(t, func(context.Context, Config, Media, pcm.Generation, time.Duration) (transport, error) { return f, nil })
	r, err := h.SetQueue(context.Background(), []Media{media("a")}, 0)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _ = r.Wait(ctx)
	first := await(t, r, nil)
	second := await(t, r, nil)
	if first != second {
		t.Fatal(first, second)
	}
	if s := h.Snapshot(); s.State != Paused || s.Current.ID != "a" {
		t.Fatal(s)
	}
}

// Decoder/read-ahead completion must not advance the queue. Only the output's
// reported presentation moves position/end, and end does not auto-select B.
func TestPresentationAndEndWithoutUI(t *testing.T) {
	f := &fakeTransport{state: Paused}
	h := newTestHandle(t, func(context.Context, Config, Media, pcm.Generation, time.Duration) (transport, error) { return f, nil })
	r, err := h.SetQueue(context.Background(), []Media{media("a"), media("b")}, 0)
	await(t, r, err)
	r, err = h.Play(context.Background())
	await(t, r, err)
	f.mu.Lock()
	f.state = Draining
	f.position = 123
	f.mu.Unlock()
	s := waitSnapshot(t, h, func(s Snapshot) bool { return s.State == Draining && s.PositionFrames == 123 })
	if s.Current.ID != "a" || s.Index != 0 {
		t.Fatal(s)
	}
	f.mu.Lock()
	f.state = Ended
	f.position = 456
	f.playing = false
	f.mu.Unlock()
	s = waitSnapshot(t, h, func(s Snapshot) bool { return s.State == Ended })
	if s.PositionFrames != 456 || s.Current.ID != "a" || s.Index != 0 || s.DesiredPlaying {
		t.Fatal(s)
	}
	r, err = h.Play(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.Wait(context.Background()); !errors.Is(err, ErrEnded) {
		t.Fatal(err)
	}
}

// Failure never publishes Playing and a failed stream is released without
// falling back to another backend. Explicit selection may recover afterward.
func TestFailureAndExplicitRecovery(t *testing.T) {
	f := &fakeTransport{state: Paused, failure: errors.New("exclusive busy")}
	good := &fakeTransport{state: Paused}
	h := newTestHandle(t, func(_ context.Context, _ Config, m Media, _ pcm.Generation, _ time.Duration) (transport, error) {
		if m.ID == "a" {
			return f, nil
		}
		return good, nil
	})
	r, err := h.SetQueue(context.Background(), []Media{media("a"), media("b")}, 0)
	if err != nil {
		t.Fatal(err)
	}
	result, err := r.Wait(context.Background())
	if err == nil || result.Status != CommandFailed || result.Snapshot.State != Failed || result.Snapshot.EffectivePlaying {
		t.Fatal(result, err)
	}
	if result.Snapshot.AppliedCommand != 0 || result.Snapshot.AppliedGeneration != (pcm.Generation{}) {
		t.Fatal("failed preparation was published as applied", result)
	}
	if closed, _ := f.values(); closed != 1 {
		t.Fatal("failed device leaked")
	}
	r, err = h.Select(context.Background(), 1)
	result = await(t, r, err)
	if result.Snapshot.Current.ID != "b" || result.Snapshot.State != Paused {
		t.Fatal(result)
	}
}

// An unread/coalesced update channel never blocks accepted command completion;
// every concurrent admission is either bounded rejection or one final receipt.
func TestConcurrentCommandsAndUnobservedSnapshots(t *testing.T) {
	f := &fakeTransport{state: Paused}
	h := newTestHandle(t, func(context.Context, Config, Media, pcm.Generation, time.Duration) (transport, error) { return f, nil })
	r, err := h.SetQueue(context.Background(), []Media{media("a")}, 0)
	await(t, r, err)
	var wg sync.WaitGroup
	receipts := make(chan *Receipt, 200)
	for i := range 200 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var r *Receipt
			var err error
			if i%2 == 0 {
				r, err = h.Play(context.Background())
			} else {
				r, err = h.Pause(context.Background())
			}
			if err != nil {
				if !errors.Is(err, ErrBusy) {
					t.Error(err)
				}
				return
			}
			receipts <- r
		}()
	}
	wg.Wait()
	close(receipts)
	seen := map[uint64]bool{}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for r := range receipts {
		if seen[r.ID()] {
			t.Fatal("duplicate ID")
		}
		seen[r.ID()] = true
		_, err := r.Wait(ctx)
		if err != nil && !errors.Is(err, ErrSuperseded) {
			t.Fatal(err)
		}
	}
	r, err = h.Pause(context.Background())
	result := await(t, r, err)
	if result.Snapshot.EffectivePlaying || result.Snapshot.State != Paused {
		t.Fatal(result)
	}
}

// Close cannot return before owned device teardown, and concurrent admission
// cannot leave a receipt stranded after the control loop has drained its queue.
func TestCloseJoinsAndSettlesReceipts(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	closing := make(chan struct{}, 1)
	f := &fakeTransport{state: Paused, closeGate: release, closing: closing}
	h := newTestHandle(t, func(context.Context, Config, Media, pcm.Generation, time.Duration) (transport, error) { return f, nil })
	r, err := h.SetQueue(context.Background(), []Media{media("a")}, 0)
	await(t, r, err)
	done := make(chan struct{})
	go func() { h.Close(); close(done) }()
	waitSignal(t, closing)
	select {
	case <-done:
		t.Fatal("Close returned before native release")
	default:
	}
	if _, err = h.Play(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
	releaseOnce.Do(func() { close(release) })
	waitSignal(t, done)
	if f.closed != 1 {
		t.Fatal(f.closed)
	}
}

// Close settles commands admitted during preparation, including those still
// queued when shutdown wins the control select; no receipt is left waiting.
func TestCloseDuringPendingCommands(t *testing.T) {
	entered := make(chan struct{}, 1)
	h := newTestHandle(t, func(ctx context.Context, _ Config, _ Media, _ pcm.Generation, _ time.Duration) (transport, error) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-ctx.Done()
		return nil, ctx.Err()
	})
	load, err := h.SetQueue(context.Background(), []Media{media("a")}, 0)
	if err != nil {
		t.Fatal(err)
	}
	waitSignal(t, entered)
	receipts := []*Receipt{load}
	for range 16 {
		r, e := h.Play(context.Background())
		if e != nil {
			t.Fatal(e)
		}
		receipts = append(receipts, r)
	}
	if err = h.Close(); err != nil {
		t.Fatal(err)
	}
	for _, r := range receipts {
		select {
		case <-r.Done():
		default:
			t.Fatal("unsettled receipt", r.ID())
		}
		result, e := r.Wait(context.Background())
		if !errors.Is(e, ErrClosed) && !errors.Is(e, ErrSuperseded) {
			t.Fatal(result, e)
		}
	}
}

// Invalid commands leave the confirmed queue and session untouched.
func TestInvalidSelection(t *testing.T) {
	h := newTestHandle(t, func(context.Context, Config, Media, pcm.Generation, time.Duration) (transport, error) {
		return nil, fmt.Errorf("unexpected open")
	})
	r, err := h.Play(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.Wait(context.Background()); !errors.Is(err, ErrEmptyQueue) {
		t.Fatal(err)
	}
	if _, err = h.SetQueue(context.Background(), []Media{media("a")}, 7); !errors.Is(err, ErrIndex) {
		t.Fatal(err)
	}
	if h.Snapshot().QueueLength != 0 {
		t.Fatal(h.Snapshot())
	}
}
