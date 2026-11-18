// Package diagnostics exports allowlisted playback evidence. Platform error
// strings may contain paths, so callers never serialize the engine snapshot.
package diagnostics

import (
	"crypto/sha256"
	"fmt"
	"runtime"
	"time"

	"mygo-music/internal/audioengine/analysis"
	"mygo-music/internal/audioengine/controller"
	"mygo-music/internal/audioengine/output"
	"mygo-music/internal/audioengine/pcm"
	"mygo-music/internal/audioengine/render"
)

type Report struct {
	Version           int
	CapturedUTC       time.Time
	OS, Arch, Go      string
	RequestedMode     string
	RequestedEndpoint string `json:",omitempty"`
	EffectiveMode     string `json:",omitempty"`
	Phase             string
	PlaybackIntent    bool
	Failure           *Failure `json:",omitempty"`
	Native            *Native  `json:",omitempty"`
	Runtime           Runtime
}

type Failure struct {
	Kind    string
	HRESULT string `json:",omitempty"`
}

type Native struct {
	Endpoint                           string
	Live                               bool // previous confirmed device is retained on a failure
	SourcePCM, SubmittedPCM            pcm.Format
	SourceDSDRate                      int         `json:",omitempty"`
	SourceDSDBits                      int64       `json:",omitempty"`
	DoP                                bool        `json:",omitempty"`
	StableBuffer                       bool        `json:",omitempty"`
	WindowsMixPCM                      *pcm.Format `json:",omitempty"`
	DriverPCM                          *pcm.Format `json:",omitempty"`
	HardwarePacketFrames               int
	PeriodNS, EndpointLatencyNS        int64
	ClockFrequency                     uint64
	MMCSS                              bool
	DeviceCounters                     output.Snapshot
	Render                             render.Progress
	RequestedRevision, AppliedRevision pcm.Generation
	LastReconfigure                    string
	EffectiveVolume                    float64
	DSPDelayFrames                     int
	LastParameterNS                    int64  `json:",omitempty"`
	ParameterEndpoint                  string `json:",omitempty"`
	SourcePositionFrame                uint64
	SourceFrameExact                   bool
	Levels                             *analysis.Measurement `json:",omitempty"`
	DroppedAnalysisPackets             uint64
}

type Playback struct {
	Mode, Endpoint                          string
	Remote, Ready, Loading, Playing, Failed bool
	Native                                  controller.Snapshot
}

// Capture includes only selected numeric and enum fields. Endpoint IDs are
// hashed to correlate reports from the same device without publishing IDs.
func Capture(p Playback) Report {
	n := p.Native
	r := Report{Version: 1, CapturedUTC: time.Now().UTC(), OS: runtime.GOOS, Arch: runtime.GOARCH, Go: runtime.Version(),
		RequestedMode: mode(p.Mode), RequestedEndpoint: endpoint(p.Endpoint), PlaybackIntent: p.Playing,
		Runtime: SampleRuntime()}
	switch {
	case p.Failed || n.State == controller.Failed:
		r.Phase = "failed"
		r.Failure = &Failure{Kind: failureKind(n.ErrorKind)}
		if n.ErrorCode != 0 {
			r.Failure.HRESULT = fmt.Sprintf("0x%08x", n.ErrorCode)
		}
	case p.Loading:
		r.Phase = "preparing"
	case n.State == controller.Ended:
		r.Phase = "ended"
	case p.Ready && p.Playing && (r.RequestedMode == "mygo-compatible" || n.EffectivePlaying):
		r.Phase = "playing"
	case p.Ready:
		r.Phase = "paused"
	default:
		r.Phase = "idle"
	}
	switch {
	case p.Remote:
		r.EffectiveMode = "remote"
	case r.RequestedMode == "mygo-compatible" && p.Ready:
		r.EffectiveMode = r.RequestedMode
	case n.Output.Format.Rate > 0:
		live := p.Ready && !p.Loading && r.Failure == nil && n.OutputStats.Epoch != 0 && n.State != controller.Ended
		x := Native{
			Endpoint: endpoint(n.Output.EndpointID), Live: live, SourcePCM: format(n.Source.Format),
			SubmittedPCM: format(n.Output.Format), HardwarePacketFrames: n.Output.BufferFrames,
			PeriodNS: int64(n.Output.Period), EndpointLatencyNS: int64(n.Output.Latency),
			ClockFrequency: n.Output.ClockFrequency, MMCSS: n.Output.MMCSS,
			DeviceCounters: n.OutputStats, Render: n.RenderProgress,
			RequestedRevision: n.RequestedGeneration, AppliedRevision: n.AppliedGeneration,
			LastReconfigure: reconfigure(n.Reconfigure), EffectiveVolume: n.AppliedVolume,
			DSPDelayFrames: n.LatencyFrames, DroppedAnalysisPackets: n.AnalysisDropped,
			SourcePositionFrame: n.PositionFrames, SourceFrameExact: n.PositionExact,
			SourceDSDRate: n.Source.DSDRate, SourceDSDBits: n.Source.DSDBits, DoP: n.Output.DoP, StableBuffer: n.Output.StableBuffer,
		}
		if x.DoP {
			x.SourcePCM = pcm.Format{}
		}
		if live {
			r.EffectiveMode = nativeMode(n)
		}
		if n.Output.MixFormat.Rate > 0 {
			f := format(n.Output.MixFormat)
			x.WindowsMixPCM = &f
		}
		if n.Output.ASIO {
			f := format(n.Output.DriverFormat)
			x.DriverPCM = &f
		}
		if n.ParameterDelay > 0 {
			x.LastParameterNS = int64(n.ParameterDelay)
			x.ParameterEndpoint = "render-enqueue"
			if n.Shared {
				x.ParameterEndpoint = "shared-gain-confirmation"
			}
		}
		if live && n.Measurement.Valid {
			m := n.Measurement
			x.Levels = &m
		}
		r.Native = &x
	}
	return r
}

func nativeMode(n controller.Snapshot) string {
	if n.Output.ASIO {
		return "asio-pcm"
	}
	if n.Output.DoP {
		return "wasapi-dop"
	}
	if n.Shared {
		return "wasapi-shared"
	}
	if n.Processing {
		return "wasapi-processed"
	}
	return "wasapi-exclusive"
}

func mode(raw string) string {
	switch raw {
	case "", "mygo-compatible":
		return "mygo-compatible"
	case "wasapi-exclusive", "wasapi-shared", "wasapi-processed", "wasapi-dop", "asio-pcm":
		return raw
	}
	return "unknown"
}

func format(f pcm.Format) pcm.Format {
	if f.Validate() != nil {
		return pcm.Format{}
	}
	return f
}

func endpoint(id string) string {
	if id == "" {
		return ""
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte(id)))
}

func failureKind(kind string) string {
	switch kind {
	case "device-lost", "service-stopped", "device-busy", "format-unsupported", "device-error",
		"default-changed", "resume-required", "clock-stopped", "source-error":
		return kind
	}
	return "playback-error"
}

func reconfigure(reason string) string {
	switch reason {
	case "open", "seek":
		return reason
	}
	return ""
}
