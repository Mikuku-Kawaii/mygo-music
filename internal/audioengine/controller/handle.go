package controller

import (
	"context"
	"errors"
	"mygo-music/internal/audioengine/output"
	"mygo-music/internal/audioengine/render"
	"slices"
	"sync"
	"sync/atomic"
	"time"
)

type action uint8

const (
	replaceQueue action = iota
	selectItem
	play
	pause
	stop
	cancelCommand
	seek
	volume
	preamp
)

type command struct {
	gain     float64
	action   action
	queue    []Media
	index    int
	target   uint64
	position time.Duration
	receipt  *Receipt
	accepted time.Time
}
type published struct {
	snapshot Snapshot
	queue    []Item
}

// EngineHandle is safe for concurrent callers. A single control goroutine owns
// intent and immutable publication; a single transport worker owns all device
// handles. Waiting consumers never block either one. Close joins both workers.
type EngineHandle struct {
	ctx              context.Context
	shutdown         context.CancelFunc
	commands         chan command
	jobs             chan job
	reports          chan workerReport
	workerDone, done chan struct{}
	changes          chan struct{}
	latest           atomic.Pointer[published]
	submitMu         sync.Mutex // admission/Close only, never acquired by workers
	nextID           uint64
	closeErr         error // published by done
	config           Config
}

// New creates an idle engine and does not open a device. Config is fixed for
// this handle; output-mode/configuration commands belong to later M1 tasks.
func New(config Config) (*EngineHandle, error) { return newHandle(config, openNative) }
func newHandle(config Config, open opener) (*EngineHandle, error) {
	if config.Processing && config.Shared {
		return nil, errors.New("engine: DSP uses processed exclusive output")
	}
	if config.DoP && (config.Shared || config.Processing || config.EndpointID == "") {
		return nil, output.ErrDoPFormat
	}
	if config.ASIO != "" && (config.DoP || config.Shared || config.Processing) {
		return nil, output.ErrASIOFormat
	}
	if config.Processing {
		if err := config.DSP.Validate(0); err != nil {
			return nil, err
		}
	}
	if config.Period < 0 || config.Period > time.Second {
		return nil, errors.New("engine: invalid device period")
	}
	if config.ContainerBits != 0 && config.ContainerBits != 16 && config.ContainerBits != 24 && config.ContainerBits != 32 {
		return nil, errors.New("engine: invalid integer container")
	}
	ctx, shutdown := context.WithCancel(context.Background())
	h := &EngineHandle{ctx: ctx, shutdown: shutdown, commands: make(chan command, 32), jobs: make(chan job, 1), reports: make(chan workerReport, 8), workerDone: make(chan struct{}), done: make(chan struct{}), changes: make(chan struct{}, 1), config: config}
	initial := Snapshot{State: Idle, Index: -1, Shared: config.Shared, Processing: config.Processing, Volume: 1}
	if config.Processing {
		initial.Volume = config.InitialVolume
		initial.PreampDB = config.DSP.PreampDB
	} else if config.Shared {
		initial.Volume = 0
	}
	initial.RequestedGeneration.Config = 1
	h.latest.Store(&published{snapshot: initial})
	go h.runWorker(config, open)
	go h.runControl(initial)
	return h, nil
}

// SetQueue owns a copy, selects index and prepares it PAUSED. Empty queues use
// index -1 and stop the stream. Explicit Play is required for unity-gain output.
func (h *EngineHandle) SetQueue(ctx context.Context, queue []Media, index int) (*Receipt, error) {
	if (len(queue) == 0 && index != -1) || (len(queue) > 0 && (index < 0 || index >= len(queue))) {
		return nil, ErrIndex
	}
	for _, m := range queue {
		if m.Source == "" || m.ID == "" || m.Path == "" {
			return nil, errors.New("engine: media requires source, ID and local path")
		}
	}
	return h.submit(ctx, command{action: replaceQueue, queue: slices.Clone(queue), index: index})
}

// Select changes the selected entry, keeping the latest playing intent. It
// explicitly starts a new session even when reselecting the current entry.
func (h *EngineHandle) Select(ctx context.Context, index int) (*Receipt, error) {
	return h.submit(ctx, command{action: selectItem, index: index})
}
func (h *EngineHandle) Play(ctx context.Context) (*Receipt, error) {
	return h.submit(ctx, command{action: play})
}
func (h *EngineHandle) Pause(ctx context.Context) (*Receipt, error) {
	return h.submit(ctx, command{action: pause})
}

func (h *EngineHandle) SetVolume(ctx context.Context, gain float64) (*Receipt, error) {
	if (!h.config.Shared && !h.config.Processing) || !(gain >= 0 && gain <= 1) {
		return nil, errors.New("engine: invalid output volume")
	}
	return h.submit(ctx, command{action: volume, gain: gain})
}

func (h *EngineHandle) SetPreamp(ctx context.Context, db float64) (*Receipt, error) {
	if !h.config.Processing || !(db >= -30 && db <= 18) {
		return nil, errors.New("engine: invalid preamp")
	}
	return h.submit(ctx, command{action: preamp, gain: db})
}

// Seek starts a new output epoch without changing the selected item or playing
// intent. Known durations clamp at EOF; the receipt confirms the actual frame.
func (h *EngineHandle) Seek(ctx context.Context, position time.Duration) (*Receipt, error) {
	if position < 0 {
		return nil, ErrSeekRange
	}
	return h.submit(ctx, command{action: seek, position: position})
}

// Stop releases the session and retains the selected queue entry for Play.
func (h *EngineHandle) Stop(ctx context.Context) (*Receipt, error) {
	return h.submit(ctx, command{action: stop})
}

// Cancel stops a still-pending command, releases its session and retains the
// queue. It cannot cancel a newer command or undo an already-applied command.
func (h *EngineHandle) Cancel(ctx context.Context, id uint64) (*Receipt, error) {
	return h.submit(ctx, command{action: cancelCommand, target: id})
}
func (h *EngineHandle) Snapshot() Snapshot { return h.latest.Load().snapshot }

// Queue returns a caller-owned copy plus the matching queue revision.
func (h *EngineHandle) Queue() ([]Item, uint64) {
	p := h.latest.Load()
	return slices.Clone(p.queue), p.snapshot.RequestedGeneration.Queue
}

// Changes is a coalesced wake-up for one consumer, not an event log. Read
// Snapshot after waking. No reads are required for playback or command progress.
func (h *EngineHandle) Changes() <-chan struct{} { return h.changes }
func (h *EngineHandle) Done() <-chan struct{}    { return h.done }
func (h *EngineHandle) Close() error {
	h.submitMu.Lock()
	h.shutdown()
	h.submitMu.Unlock()
	<-h.done
	return h.closeErr
}
func (h *EngineHandle) submit(ctx context.Context, c command) (*Receipt, error) {
	h.submitMu.Lock()
	defer h.submitMu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if h.ctx.Err() != nil {
		return nil, ErrClosed
	}
	h.nextID++
	r := &Receipt{id: h.nextID, done: make(chan struct{})}
	c.receipt = r
	if c.action == volume || c.action == preamp {
		c.accepted = time.Now()
	}
	select {
	case h.commands <- c:
		return r, nil
	default:
		return nil, ErrBusy
	}
}

type control struct {
	h              *EngineHandle
	snapshot       Snapshot
	queue          []Media
	view           []Item // immutable after each replacement
	active         bool
	pending        *Receipt
	pendingConfig  bool
	cancelJob      context.CancelFunc
	revision       uint64
	next           *render.NextSlot
	lastPublish    time.Time
	configAccepted time.Time
}

func (h *EngineHandle) runControl(initial Snapshot) {
	defer close(h.done)
	defer close(h.changes)
	c := control{h: h, snapshot: initial, next: h.config.Next}
	for {
		select {
		case <-h.ctx.Done():
			if c.cancelJob != nil {
				c.cancelJob()
			}
			<-h.workerDone
			c.snapshot.State = Closed
			c.snapshot.DesiredPlaying = false
			c.snapshot.EffectivePlaying = false
			c.snapshot.Current = Item{}
			c.snapshot.Error = ""
			if h.closeErr != nil {
				c.snapshot.Error = h.closeErr.Error()
			}
			c.publish()
			if c.pending != nil {
				c.pending.finish(Canceled, c.snapshot, ErrClosed)
			}
			for {
				select {
				case cmd := <-h.commands:
					cmd.receipt.finish(Canceled, c.snapshot, ErrClosed)
				default:
					return
				}
			}
		case cmd := <-h.commands:
			c.accept(cmd)
		case report := <-h.reports:
			c.observe(report)
		}
	}
}
func (c *control) publish() {
	c.lastPublish = time.Now()
	c.snapshot.EventSeq++
	c.h.latest.Store(&published{c.snapshot, c.view})
	select {
	case c.h.changes <- struct{}{}:
	default:
	}
}
func (c *control) accept(cmd command) {
	reject := func(err error) { cmd.receipt.finish(Rejected, c.snapshot, err) }
	if (cmd.action == volume || cmd.action == preamp) && c.snapshot.State == Failed {
		reject(errors.New("engine: failed stream requires reopen"))
		return
	}
	if cmd.action == selectItem && (cmd.index < 0 || cmd.index >= len(c.queue)) {
		reject(ErrIndex)
		return
	}
	if (cmd.action == play || cmd.action == seek) && c.snapshot.Index < 0 {
		reject(ErrEmptyQueue)
		return
	}
	if cmd.action == play && c.snapshot.State == Ended {
		reject(ErrEnded)
		return
	}
	if cmd.action == cancelCommand && (c.pending == nil || c.pending.ID() != cmd.target) {
		reject(ErrNotPending)
		return
	}
	if c.pending != nil {
		status, err := Superseded, ErrSuperseded
		if cmd.action == cancelCommand {
			status, err = Canceled, context.Canceled
		}
		c.pending.finish(status, c.snapshot, err)
		c.pending = nil
	}
	if c.cancelJob != nil {
		c.cancelJob()
	}
	newSession := false
	resetTransition := false
	switch cmd.action {
	case replaceQueue:
		c.queue = cmd.queue
		c.view = make([]Item, len(c.queue))
		for i, m := range c.queue {
			c.view[i] = m.Item
		}
		c.snapshot.Index = cmd.index
		c.snapshot.QueueLength = len(c.queue)
		c.snapshot.RequestedGeneration.Queue++
		c.snapshot.DesiredPlaying = false
		c.active = len(c.queue) > 0
		newSession = true
	case selectItem:
		c.snapshot.Index = cmd.index
		c.active = true
		newSession = true
	case play:
		c.snapshot.DesiredPlaying = true
		if !c.active {
			c.active = true
			newSession = true
		}
	case pause:
		if c.h.config.ASIO != "" && c.active && c.snapshot.DesiredPlaying && c.snapshot.Source.Format.Rate > 0 {
			// ASIO Stop can discard driver-owned buffers. Locate a new
			// paused stream at the last confirmed presentation anchor.
			frame, rate := c.snapshot.PositionFrames, uint64(c.snapshot.Source.Format.Rate)
			c.snapshot.SeekTarget = time.Duration(frame/rate)*time.Second +
				time.Duration((frame%rate*uint64(time.Second)+rate-1)/rate)
			c.snapshot.RequestedGeneration.Seek++
			c.snapshot.RequestedGeneration.Format++
			resetTransition = true
		}
		c.snapshot.DesiredPlaying = false
	case volume:
		c.snapshot.Volume = cmd.gain
		c.snapshot.RequestedGeneration.Config++
		c.configAccepted = cmd.accepted
	case preamp:
		c.snapshot.PreampDB = cmd.gain
		c.snapshot.RequestedGeneration.Config++
		c.configAccepted = cmd.accepted
	case seek:
		c.active = true
		c.snapshot.SeekTarget = cmd.position
		c.snapshot.RequestedGeneration.Seek++
		c.snapshot.RequestedGeneration.Format++
		resetTransition = true
	case stop, cancelCommand:
		c.snapshot.DesiredPlaying = false
		c.active = false
		newSession = true
	}
	if newSession {
		c.snapshot.SeekTarget = 0
		c.snapshot.RequestedGeneration.Session++
		c.snapshot.RequestedGeneration.Format++
		resetTransition = true
	}
	if resetTransition {
		c.snapshot.ContinuationToken = 0
		c.snapshot.Measurement.Valid = false
		c.snapshot.Reconfigure = "open"
		if cmd.action == seek || cmd.action == pause {
			c.snapshot.Reconfigure = "seek"
		}
	}
	c.snapshot.Selected = Item{}
	if c.snapshot.Index >= 0 {
		c.snapshot.Selected = c.queue[c.snapshot.Index].Item
	}
	c.snapshot.RequestedCommand = cmd.receipt.ID()
	c.snapshot.Error = ""
	c.snapshot.ErrorKind = ""
	c.snapshot.ErrorCode = 0
	if c.active && (resetTransition || c.snapshot.State == Failed) {
		c.snapshot.State = Preparing
	} else if !c.active {
		c.snapshot.State = Stopping
	}
	c.pending = cmd.receipt
	c.pendingConfig = c.snapshot.Processing && (cmd.action == volume || cmd.action == preamp)
	c.revision++
	ctx, cancel := context.WithCancel(c.h.ctx)
	c.cancelJob = cancel
	cfg := c.h.config
	cfg.Next = c.next
	cfg.InitialVolume = c.snapshot.Volume
	cfg.DSP.PreampDB = c.snapshot.PreampDB
	j := job{ctx: ctx, revision: c.revision, generation: c.snapshot.RequestedGeneration, active: c.active, playing: c.snapshot.DesiredPlaying, position: c.snapshot.SeekTarget, volume: c.snapshot.Volume, config: cfg, configAccepted: c.configAccepted}
	if c.active {
		j.media = c.queue[c.snapshot.Index]
	}
	c.publish()
	// There is only one current job and one replaceable queued job, even if
	// a kernel/device call or a source opener is slow to observe cancellation.
	select {
	case <-c.h.jobs:
	default:
	}
	c.h.jobs <- j
}
func (c *control) observe(r workerReport) {
	if r.revision != c.revision {
		return
	} // canceled sources and late device samples
	previous := c.snapshot
	c.snapshot.State = r.observation.state
	c.snapshot.Current = r.item
	if m := r.observation.media; m.Path != "" && c.snapshot.Index >= 0 && r.err == nil {
		c.snapshot.Current = m.Item
		if r.observation.transition != 0 && c.snapshot.ContinuationToken != r.observation.transition {
			c.snapshot.ContinuationToken = r.observation.transition
			c.queue[c.snapshot.Index] = m
			c.view = slices.Clone(c.view)
			c.view[c.snapshot.Index] = m.Item
			c.snapshot.Selected = m.Item
		}
	}
	c.snapshot.EffectivePlaying = r.observation.playing
	c.snapshot.Source = r.observation.source
	c.snapshot.Output = r.observation.output
	if r.err == nil {
		c.snapshot.AppliedGeneration = r.generation
		if c.snapshot.Processing {
			c.snapshot.AppliedGeneration.Config, c.snapshot.AppliedVolume = r.observation.config, r.observation.volume
			c.snapshot.AppliedPreampDB = r.observation.preamp
			c.snapshot.ParameterDelay = r.observation.parameterDelay
			if r.observation.config != 0 {
				c.snapshot.DSP = c.h.config.DSP
				c.snapshot.DSP.PreampDB = r.observation.preamp
				c.snapshot.TrackGain = c.snapshot.DSP.TrackGain(r.observation.media.Gains)
			}
		} else if r.final {
			c.snapshot.AppliedVolume = r.volume
			if c.snapshot.Shared && previous.AppliedGeneration.Config != r.generation.Config && !c.configAccepted.IsZero() && r.observation.output.Format.Rate > 0 {
				c.snapshot.ParameterDelay = time.Since(c.configAccepted)
			}
		}
	}
	c.snapshot.PositionFrames = r.observation.position
	c.snapshot.SourceStartFrame = r.observation.start
	c.snapshot.PositionExact = r.observation.exact
	c.snapshot.SubmittedFrames = r.observation.submitted
	c.snapshot.ContentSubmittedFrames = r.observation.content
	c.snapshot.Underruns = r.observation.underruns
	c.snapshot.LatencyFrames, c.snapshot.TailFrames = r.observation.latency, r.observation.tail
	c.snapshot.OutputStats, c.snapshot.RenderProgress = r.observation.outputStats, r.observation.progress
	c.snapshot.RingLowWater, c.snapshot.MaxService = c.snapshot.OutputStats.RingLowWater, c.snapshot.OutputStats.MaxService
	c.snapshot.Measurement, c.snapshot.AnalysisDropped = r.observation.measurement, r.observation.analysisDropped
	if r.err != nil {
		// A failed device must not erase the last confirmed source anchor.
		c.snapshot.PositionFrames, c.snapshot.SourceStartFrame = previous.PositionFrames, previous.SourceStartFrame
		c.snapshot.Source, c.snapshot.Output = previous.Source, previous.Output
		if c.snapshot.OutputStats.Epoch == 0 {
			c.snapshot.OutputStats, c.snapshot.RenderProgress = previous.OutputStats, previous.RenderProgress
			c.snapshot.RingLowWater, c.snapshot.MaxService = previous.RingLowWater, previous.MaxService
			c.snapshot.AnalysisDropped = previous.AnalysisDropped
		}
		c.snapshot.Measurement.Valid = false
		c.snapshot.ErrorKind = output.FailureKind(r.err)
		c.snapshot.ErrorCode = output.FailureCode(r.err)
		c.snapshot.Error = r.err.Error()
		c.snapshot.State = Failed
		c.snapshot.DesiredPlaying = false
		c.active = false
	}
	if c.snapshot.State == Ended {
		c.snapshot.DesiredPlaying = false
	}
	confirmed := c.pending != nil && (r.final && !c.pendingConfig ||
		c.pendingConfig && (c.snapshot.AppliedGeneration.Config == c.snapshot.RequestedGeneration.Config || r.err != nil || c.snapshot.State == Ended))
	applyErr := r.err
	if confirmed && c.pendingConfig && applyErr == nil && c.snapshot.AppliedGeneration.Config != c.snapshot.RequestedGeneration.Config {
		applyErr = ErrEnded
	}
	if confirmed && applyErr == nil {
		c.snapshot.AppliedCommand = c.pending.ID()
	}
	// Poll boundaries and applied revisions promptly, but coalesce ordinary
	// position updates so a large application playlist isn't copied at 100 Hz.
	important := c.snapshot.State != previous.State || c.snapshot.ContinuationToken != previous.ContinuationToken ||
		c.snapshot.AppliedGeneration != previous.AppliedGeneration || c.snapshot.Error != previous.Error
	visual := c.snapshot.Measurement.Valid != previous.Measurement.Valid ||
		c.snapshot.Measurement.Valid && c.snapshot.Measurement.Through != previous.Measurement.Through
	if r.final || confirmed || important || visual && time.Since(c.lastPublish) >= time.Second/30 ||
		c.snapshot != previous && time.Since(c.lastPublish) >= 100*time.Millisecond {
		c.publish()
	}
	if confirmed {
		status := Applied
		if applyErr != nil {
			status = CommandFailed
		}
		c.pending.finish(status, c.snapshot, applyErr)
		c.pending = nil
	}
}
