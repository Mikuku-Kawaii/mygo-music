// audiolab is the independent audio-engine harness. It never starts MyGo UI
// or reads the user's library, accounts or application configuration.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/metrics"
	"runtime/pprof"
	"slices"
	"time"

	"mygo-music/internal/audioengine/analysis"
	"mygo-music/internal/audioengine/buffer"
	"mygo-music/internal/audioengine/controller"
	"mygo-music/internal/audioengine/decode"
	"mygo-music/internal/audioengine/fixture"
	"mygo-music/internal/audioengine/output"
	"mygo-music/internal/audioengine/pcm"
	"mygo-music/internal/audioengine/render"
)

type report struct {
	Mode, Go, OS, Arch string
	Started            time.Time
	Result             any
	Error              string `json:",omitempty"`
}

func main() {
	mode := flag.String("mode", "verify", "verify | fixtures | probe | play | smoke | handle-smoke | endurance | asio-list | asio-smoke | loudness")
	dir := flag.String("dir", "", "new fixture directory (fixtures), existing manifest directory (verify)")
	device := flag.String("device", "", "fixed endpoint ID; empty follows default at open, never silently reopens")
	asio := flag.String("asio", "", "registered ASIO driver ID")
	file := flag.String("file", "", "local audio file for play or loudness")
	next := flag.String("next", "", "optional same-format next file")
	container := flag.Int("container", 0, "output container bits; 0 uses source container")
	rate := flag.Int("rate", 48000, "smoke sample rate")
	valid := flag.Int("valid", 16, "smoke effective integer bits")
	period := flag.Duration("period", 0, "exclusive period; 0 uses device default")
	duration := flag.Duration("duration", 3*time.Second, "silent playback duration for smoke or endurance")
	stress := flag.Bool("gc-stress", false, "allocation pressure (smoke also forces GC)")
	stableBuffer := flag.Bool("stable-buffer", false, "use the high-load WASAPI buffer profile for endurance")
	enduranceMode := flag.String("endurance-output", "shared", "endurance: shared | exclusive | processed")
	evidence := flag.String("evidence", "", "endurance: new JSONL evidence file with periodic observations")
	interval := flag.Duration("interval", time.Minute, "endurance evidence interval")
	cpuProfile := flag.String("cpuprofile", "", "write CPU profile to a new file")
	heapProfile := flag.String("memprofile", "", "write allocation profile to a new file")
	flag.Parse()
	var cpuFile *os.File
	if *cpuProfile != "" {
		var err error
		cpuFile, err = os.OpenFile(*cpuProfile, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if err = pprof.StartCPUProfile(cpuFile); err != nil {
			cpuFile.Close()
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	r := report{Mode: *mode, Go: runtime.Version(), OS: runtime.GOOS, Arch: runtime.GOARCH, Started: time.Now().UTC()}
	var err error
	fixtureFormat := pcm.Format{Kind: pcm.Integer, Rate: *rate, Channels: 2, ChannelMask: 3, ValidBits: *valid, ContainerBits: *valid}
	switch *mode {
	case "fixtures":
		if *dir == "" {
			err = errors.New("fixtures requires -dir")
		} else {
			r.Result, err = fixture.Generate(*dir)
		}
	case "verify":
		r.Result, err = verify(ctx, *dir)
	case "probe":
		r.Result, err = output.Probe(ctx, formats())
	case "asio-list":
		r.Result, err = output.ListASIO()
	case "loudness":
		r.Result, err = analysis.ScanLoudness(ctx, *file)
	case "asio-smoke":
		if *asio == "" {
			err = errors.New("asio-smoke requires -asio")
		} else {
			r.Result, err = handleSmoke(ctx, controller.Config{ASIO: *asio, ContainerBits: *container}, fixtureFormat)
		}
	case "play":
		r.Result, err = play(ctx, *file, *next, *device, *container, *period)
	case "handle-smoke":
		r.Result, err = handleSmoke(ctx, controller.Config{EndpointID: *device, ContainerBits: *container, Period: *period}, fixtureFormat)
	case "smoke":
		bits := *container
		if bits == 0 {
			bits = *valid
		}
		f := pcm.Format{Kind: pcm.Integer, Rate: *rate, Channels: 2, ChannelMask: 3, ValidBits: *valid, ContainerBits: bits}
		r.Result, err = smoke(ctx, output.Config{EndpointID: *device, Format: f, Period: *period, Epoch: 1}, *duration, *stress)
	case "endurance":
		r.Result, err = endurance(ctx, enduranceOptions{
			mode: *enduranceMode, device: *device, evidence: *evidence, duration: *duration, interval: *interval,
			rate: *rate, bits: *valid, container: *container, period: *period, stress: *stress, stableBuffer: *stableBuffer,
		})
	default:
		err = errors.New("unknown mode")
	}
	if err != nil {
		r.Error = err.Error()
	}
	if cpuFile != nil {
		pprof.StopCPUProfile()
		cpuFile.Close()
	}
	if *heapProfile != "" {
		f, e := os.OpenFile(*heapProfile, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if e == nil {
			runtime.GC()
			e = pprof.Lookup("allocs").WriteTo(f, 0)
			f.Close()
		}
		if e != nil {
			fmt.Fprintln(os.Stderr, e)
			os.Exit(1)
		}
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if e := enc.Encode(r); e != nil {
		fmt.Fprintln(os.Stderr, e)
		os.Exit(1)
	}
	if err != nil {
		os.Exit(1)
	}
}
func formats() []pcm.Format {
	var fs []pcm.Format
	for _, rate := range []int{44100, 48000, 96000, 176400, 192000, 352800} {
		for _, b := range [][2]int{{16, 16}, {24, 24}, {24, 32}, {32, 32}} {
			fs = append(fs, pcm.Format{Kind: pcm.Integer, Rate: rate, Channels: 2, ChannelMask: 3, ValidBits: b[0], ContainerBits: b[1]})
		}
	}
	return fs
}

type verification struct {
	Files, Frames, ComparedSamples int
	Evidence                       string
}

func verify(ctx context.Context, dir string) (verification, error) {
	v := verification{Evidence: "Offline decode and unpacked submission buffers compared to deterministic native int32 reference; no device or digital loopback claim."}
	if dir == "" {
		var err error
		dir, err = os.MkdirTemp("", "prism-m0-")
		if err != nil {
			return v, err
		}
		defer os.RemoveAll(dir)
		if _, err = fixture.Generate(dir); err != nil {
			return v, err
		}
	}
	b, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return v, err
	}
	var m fixture.Manifest
	if err = json.Unmarshal(b, &m); err != nil {
		return v, err
	}
	if m.Version != 1 || len(m.Entries) == 0 {
		return v, errors.New("unsupported or empty fixture manifest")
	}
	for _, e := range m.Entries {
		if e.Frames < 0 || e.Frames > 1000000 || e.StartFrame < 0 || e.StartFrame > 1000000 {
			return v, errors.New("invalid fixture bounds")
		}
		d, err := decode.Open(ctx, filepath.Join(dir, e.File))
		if err != nil {
			return v, err
		}
		if !e.Format.NativeEqual(d.Info().Format) || d.Info().Frames != int64(e.Frames) {
			d.Close()
			return v, fmt.Errorf("%s: metadata differs", e.File)
		}
		var got []int32
		buf := make([]int32, 4096)
		for {
			n, readErr := d.Read(ctx, buf)
			got = append(got, buf[:n*2]...)
			if readErr == io.EOF {
				break
			}
			if readErr != nil {
				d.Close()
				return v, readErr
			}
			if len(got) > e.Frames*2 {
				d.Close()
				return v, errors.New("frame count exceeds manifest")
			}
		}
		d.Close()
		want := fixture.Reference(e)
		if !slices.Equal(got, want) || fixture.Hash(got) != e.SampleSHA256 {
			return v, fmt.Errorf("%s: sample mismatch", e.File)
		}
		p, err := pcm.NewPacker(e.Format)
		if err != nil {
			return v, err
		}
		wire := make([]byte, e.Frames*e.Format.FrameBytes())
		back := make([]int32, len(got))
		if err = p.Pack(wire, got); err != nil {
			return v, err
		}
		if err = p.Unpack(back, wire); err != nil {
			return v, err
		}
		if !slices.Equal(want, back) {
			return v, errors.New("submission buffer mismatch")
		}
		v.Files++
		v.Frames += e.Frames
		v.ComparedSamples += len(got)
	}
	return v, nil
}

type playback struct {
	Opened   output.Opened
	Final    output.Snapshot
	Evidence string
}

func play(ctx context.Context, file, next, device string, container int, period time.Duration) (playback, error) {
	result := playback{Evidence: "Native exclusive application submission and driver clock only; audible playback and external digital capture are not verified."}
	if file == "" {
		return result, errors.New("play requires -file")
	}
	d, err := decode.Open(ctx, file)
	if err != nil {
		return result, err
	}
	ds := []decode.Decoder{d}
	if next != "" {
		n, err := decode.Open(ctx, next)
		if err != nil {
			d.Close()
			return result, err
		}
		ds = append(ds, n)
	}
	f := d.Info().Format
	if container != 0 {
		f.ContainerBits = container
	}
	p, err := render.Start(ctx, ds, f, pcm.Generation{Session: 1, Format: 1})
	if err != nil {
		for _, d := range ds {
			d.Close()
		}
		return result, err
	}
	defer p.Close()
	s, err := output.Open(ctx, output.Config{EndpointID: device, Format: f, Period: period, Epoch: 1}, p.Ring)
	if err != nil {
		return result, err
	}
	defer s.Close()
	result.Opened = s.Info
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		if p.Ring.Buffered() >= s.Info.BufferFrames || p.Ring.Finished() {
			break
		}
		select {
		case <-p.Done():
			if err = p.Wait(); err != nil {
				return result, err
			}
		case <-ticker.C:
		case <-ctx.Done():
			return result, ctx.Err()
		}
	}
	if err = s.Start(ctx); err != nil {
		return result, err
	}
	for s.Snapshot().State != output.Ended {
		select {
		case <-ticker.C:
		case <-p.Done():
			if err = p.Wait(); err != nil {
				return result, err
			} // nil means all frames queued, still drain the device
			select {
			case <-ticker.C:
			case <-ctx.Done():
				return result, ctx.Err()
			}
		case <-s.Done():
			return result, s.Err()
		case <-ctx.Done():
			return result, ctx.Err()
		}
	}
	result.Final = s.Snapshot()
	return result, s.Close()
}

type runtimeStats struct {
	AllocBytes, Mallocs, GCs         uint64
	GCPauseSeconds, SchedulerSeconds []uint64
	GCPauseBuckets, SchedulerBuckets []float64
}

func runtimeSnapshot() runtimeStats {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	s := []metrics.Sample{{Name: "/gc/pauses:seconds"}, {Name: "/sched/latencies:seconds"}}
	metrics.Read(s)
	r := runtimeStats{AllocBytes: m.TotalAlloc, Mallocs: m.Mallocs, GCs: uint64(m.NumGC)}
	for i := range s {
		if s[i].Value.Kind() != metrics.KindFloat64Histogram {
			continue
		}
		h := s[i].Value.Float64Histogram()
		b := append([]float64(nil), h.Buckets...) // encode finite bucket edges only (outer buckets are infinities)
		if len(b) > 1 {
			b = b[1 : len(b)-1]
		}
		if i == 0 {
			r.GCPauseSeconds = append([]uint64(nil), h.Counts...)
			r.GCPauseBuckets = b
		} else {
			r.SchedulerSeconds = append([]uint64(nil), h.Counts...)
			r.SchedulerBuckets = b
		}
	}
	return r
}

type smokeReport struct {
	Opened                                                    output.Opened
	BeforePause, Paused, Resumed, Drained, Reset, SecondDrain output.Snapshot
	Before, After                                             runtimeStats
	Duration                                                  time.Duration
	GCStress                                                  bool
	Evidence                                                  string
}

func smoke(ctx context.Context, c output.Config, duration time.Duration, stress bool) (smokeReport, error) {
	r := smokeReport{Duration: duration, GCStress: stress, Evidence: "Silent exclusive device lifecycle only; no audible verification or external digital capture. Runtime histograms are process-wide cumulative snapshots."}
	if duration < time.Second || duration > time.Hour {
		return r, errors.New("smoke duration must be 1s..1h")
	}
	if err := c.Format.Validate(); err != nil {
		return r, err
	}
	ctx, cancel := context.WithTimeout(ctx, duration+15*time.Second)
	defer cancel()
	ring, err := buffer.NewRing(c.Format.Rate/12, c.Format.FrameBytes())
	if err != nil {
		return r, err
	}
	total := uint64(duration) * uint64(c.Format.Rate) / uint64(time.Second)
	// Prime before opening; the producer continues on an ordinary worker.
	chunk := make([]byte, ring.Capacity()*c.Format.FrameBytes())
	initial := ring.Write(chunk)
	producerDone := make(chan struct{})
	go func() {
		defer close(producerDone)
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		written := uint64(initial)
		for written < total {
			n := min(total-written, uint64(ring.Capacity()))
			written += uint64(ring.Write(chunk[:int(n)*c.Format.FrameBytes()]))
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
		ring.Finish()
	}()
	defer func() { cancel(); <-producerDone }()
	s, err := output.Open(ctx, c, ring)
	if err != nil {
		return r, err
	}
	defer s.Close()
	r.Opened = s.Info
	stressDone := make(chan struct{})
	if stress {
		go func() {
			defer close(stressDone)
			ticker := time.NewTicker(100 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					b := make([]byte, 1<<20)
					for i := range b {
						b[i] = byte(i)
					}
					runtime.KeepAlive(b)
					runtime.GC()
				}
			}
		}()
		defer func() { cancel(); <-stressDone }()
	}
	r.Before = runtimeSnapshot()
	if err = s.Start(ctx); err != nil {
		return r, err
	}
	if err = delay(ctx, 250*time.Millisecond); err != nil {
		return r, err
	}
	r.BeforePause = s.Snapshot()
	if err = s.Pause(ctx); err != nil {
		return r, err
	}
	paused := s.Snapshot()
	if err = delay(ctx, 120*time.Millisecond); err != nil {
		return r, err
	}
	r.Paused = s.Snapshot()
	if r.Paused.Presented != paused.Presented {
		return r, errors.New("presentation advanced during pause")
	}
	if err = s.Start(ctx); err != nil {
		return r, err
	}
	r.Resumed = s.Snapshot()
	if err = s.Drain(ctx); err != nil {
		return r, err
	}
	if err = waitEnded(ctx, s); err != nil {
		return r, err
	}
	r.Drained = s.Snapshot()
	r.After = runtimeSnapshot()
	if r.Drained.ContentSubmitted != total || r.Drained.Presented < total {
		return r, errors.New("silent stream did not fully present")
	}
	second, err := buffer.NewRing(max(s.Info.BufferFrames*4, ring.Capacity()), c.Format.FrameBytes())
	if err != nil {
		return r, err
	}
	second.Write(make([]byte, s.Info.BufferFrames*2*c.Format.FrameBytes()))
	second.Finish()
	if err = s.Reset(ctx, second, c.Epoch+1); err != nil {
		return r, err
	}
	r.Reset = s.Snapshot()
	if r.Reset.State != output.Ready || r.Reset.Presented != 0 {
		return r, errors.New("reset did not clear clock/retain pause")
	}
	if err = s.Start(ctx); err != nil {
		return r, err
	}
	if err = waitEnded(ctx, s); err != nil {
		return r, err
	}
	r.SecondDrain = s.Snapshot()
	if r.Drained.Underruns > 0 || r.SecondDrain.Underruns > 0 {
		return r, errors.New("device lifecycle ran but supply underruns invalidate continuity")
	}
	return r, s.Close()
}
func delay(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func waitEnded(ctx context.Context, s *output.Stream) error {
	t := time.NewTicker(5 * time.Millisecond)
	defer t.Stop()
	for {
		snp := s.Snapshot()
		if snp.State == output.Ended {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.Done():
			return s.Err()
		case <-t.C:
		}
	}
}
