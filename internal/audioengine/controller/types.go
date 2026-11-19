// Package controller exposes the PP-M1-09 asynchronous engine handle. Its
// control goroutine is the sole owner of this engine's media selection, queue
// and playback intent. The application adapter supplies a single current item;
// its PlaybackController remains the sole owner of the application queue.
package controller

import (
	"context"
	"errors"
	"time"

	"mygo-music/internal/audioengine/analysis"
	"mygo-music/internal/audioengine/decode"
	"mygo-music/internal/audioengine/dsp"
	"mygo-music/internal/audioengine/output"
	"mygo-music/internal/audioengine/pcm"
	"mygo-music/internal/audioengine/render"
)

// Item is safe to publish. Path belongs to the command's private Media, never
// the queue view or snapshots. Source/ID retain the application's identity.
type Item struct{ Source, ID, Title string }
type Media struct {
	Item
	Path             string
	Gains            dsp.Gains
	DecodeCompressed bool // admit decoded PCM for a compressed playback fallback
}
type Config struct {
	Shared        bool
	DoP           bool
	ASIO          string
	EndpointID    string
	ContainerBits int
	StableBuffer  bool
	Period        time.Duration
	Processing    bool
	DSP           dsp.Config
	Next          *render.NextSlot
	InitialVolume float64
	Analysis      *analysis.Subscription
}

type State string

const (
	Idle      State = "idle"
	Preparing State = "preparing"
	Paused    State = "paused"
	Playing   State = "playing"
	Draining  State = "draining"
	Ended     State = "ended"
	Stopping  State = "stopping"
	Failed    State = "failed"
	Closed    State = "closed"
)

// Snapshot contains values only. Requested selection/generation can differ
// from the last confirmed current item during asynchronous preparation.
// No mutable decoder, output stream, queue slice or location is exposed.
type Snapshot struct {
	Shared                                                  bool
	Volume                                                  float64
	AppliedVolume                                           float64
	PreampDB, AppliedPreampDB                               float64
	EventSeq                                                uint64
	RequestedCommand, AppliedCommand                        uint64
	RequestedGeneration, AppliedGeneration                  pcm.Generation
	State                                                   State
	DesiredPlaying, EffectivePlaying                        bool
	Selected, Current                                       Item
	Index, QueueLength                                      int
	Source                                                  decode.Info
	Output                                                  output.Opened
	PositionFrames, SubmittedFrames, ContentSubmittedFrames uint64
	PositionExact                                           bool
	// SeekTarget is requested media time; SourceStartFrame is the confirmed
	// source-frame anchor of the current output epoch. PositionFrames is an
	// absolute source position; submitted counters remain epoch-relative.
	SeekTarget                time.Duration
	SourceStartFrame          uint64
	Underruns                 uint64
	ContinuationToken         uint64
	Processing                bool
	DSP                       dsp.Config
	TrackGain                 float64
	LatencyFrames, TailFrames int
	RingLowWater              int
	MaxService                time.Duration
	OutputStats               output.Snapshot
	RenderProgress            render.Progress
	Measurement               analysis.Measurement
	AnalysisDropped           uint64
	ParameterDelay            time.Duration
	Reconfigure               string
	Error                     string
	ErrorKind                 string
	ErrorCode                 uint32
}

type Status string

const (
	Applied       Status = "applied"
	Superseded    Status = "superseded"
	Canceled      Status = "canceled"
	Rejected      Status = "rejected"
	CommandFailed Status = "failed"
)

var (
	ErrClosed     = errors.New("engine: closed")
	ErrBusy       = errors.New("engine: command queue full")
	ErrEmptyQueue = errors.New("engine: no selected media")
	ErrIndex      = errors.New("engine: invalid queue index")
	ErrSuperseded = errors.New("engine: command superseded")
	ErrNotPending = errors.New("engine: command is no longer pending")
	ErrEnded      = errors.New("engine: select the track again to restart an ended stream")
	ErrSeekRange  = errors.New("engine: seek position must not be negative")
)

type Result struct {
	CommandID uint64
	Status    Status
	Snapshot  Snapshot
}

// Receipt means admission to the bounded control queue, not device success.
// Wait may be called more than once or concurrently. Cancel a pending operation
// through EngineHandle.Cancel; canceling Wait's context only stops that wait.
type Receipt struct {
	id     uint64
	done   chan struct{}
	result Result
	err    error
}

func (r *Receipt) ID() uint64            { return r.id }
func (r *Receipt) Done() <-chan struct{} { return r.done }
func (r *Receipt) Wait(ctx context.Context) (Result, error) {
	select {
	case <-r.done:
		return r.result, r.err
	default:
	}
	select {
	case <-r.done:
		return r.result, r.err
	case <-ctx.Done():
		return Result{}, ctx.Err()
	}
}
func (r *Receipt) finish(status Status, s Snapshot, err error) {
	r.result = Result{r.id, status, s}
	r.err = err
	close(r.done)
}
