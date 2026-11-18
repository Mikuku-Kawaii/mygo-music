package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
	"time"

	"mygo-music/internal/audioengine/decode"
	"mygo-music/internal/audioengine/fixture"
	"mygo-music/internal/audioengine/pcm"
	"mygo-music/internal/audioengine/render"
)

// Rapid seeks replace the old epoch only after its transport closes, and a
// pause received during that close governs the final prepared transport.
func TestSeekSupersededAndPause(t *testing.T) {
	ctx := context.Background()
	closing, release := make(chan struct{}, 1), make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	first := &fakeTransport{state: Paused, position: 1200, closing: closing, closeGate: release}
	opened := make(chan *fakeTransport, 8)
	count := 0
	h := newTestHandle(t, func(_ context.Context, _ Config, _ Media, _ pcm.Generation, pos time.Duration) (transport, error) {
		count++
		if count == 1 {
			return first, nil
		}
		if closed, _ := first.values(); closed != 1 {
			t.Error("opened before old epoch closed")
		}
		f := &fakeTransport{state: Paused, position: uint64(pos/time.Millisecond) * 48}
		opened <- f
		return f, nil
	})
	r, e := h.SetQueue(ctx, []Media{media("a")}, 0)
	initial := await(t, r, e).Snapshot
	r, e = h.Play(ctx)
	await(t, r, e)
	a, e := h.Seek(ctx, 2*time.Second)
	if e != nil {
		t.Fatal(e)
	}
	waitSignal(t, closing)
	s := h.Snapshot()
	if s.State != Preparing || s.AppliedGeneration != initial.AppliedGeneration || s.PositionFrames != 1200 {
		t.Fatal(s)
	}
	b, e := h.Seek(ctx, 3*time.Second)
	if e != nil {
		t.Fatal(e)
	}
	r, e = h.Pause(ctx)
	if e != nil {
		t.Fatal(e)
	}
	last, e := h.Seek(ctx, 4*time.Second)
	if e != nil {
		t.Fatal(e)
	}
	waitSnapshot(t, h, func(s Snapshot) bool { return s.RequestedCommand == last.ID() })
	close(release)
	s = await(t, last, nil).Snapshot
	for _, old := range []*Receipt{a, b, r} {
		if _, err := old.Wait(ctx); !errors.Is(err, ErrSuperseded) {
			t.Fatal(err)
		}
	}
	f := <-opened
	if s.State != Paused || s.DesiredPlaying || s.EffectivePlaying || s.PositionFrames != 192000 || s.AppliedGeneration.Seek != 3 || s.AppliedGeneration.Session != initial.AppliedGeneration.Session || s.AppliedGeneration.Queue != initial.AppliedGeneration.Queue {
		t.Fatal(s)
	}
	if _, starts := f.values(); starts != 0 {
		t.Fatal("paused seek started output")
	}
	r, e = h.Play(ctx)
	await(t, r, e)
	r, e = h.Seek(ctx, 500*time.Millisecond)
	s = await(t, r, e).Snapshot
	if !s.EffectivePlaying || s.PositionFrames != 24000 || s.AppliedGeneration.Seek != 4 {
		t.Fatal(s)
	}
}

// A failed seek never advances the applied generation; another explicit seek
// can recover the same selection. Invalid and empty-queue seeks are rejected.
func TestSeekFailureAndRecovery(t *testing.T) {
	ctx := context.Background()
	h := newTestHandle(t, func(_ context.Context, _ Config, _ Media, _ pcm.Generation, pos time.Duration) (transport, error) {
		if pos == time.Second {
			return nil, errors.New("seek failed")
		}
		return &fakeTransport{state: Paused, position: uint64(pos/time.Millisecond) * 48}, nil
	})
	if _, err := h.Seek(ctx, -1); !errors.Is(err, ErrSeekRange) {
		t.Fatal(err)
	}
	r, e := h.Seek(ctx, 0)
	if e != nil {
		t.Fatal(e)
	}
	if result, err := r.Wait(ctx); !errors.Is(err, ErrEmptyQueue) || result.Status != Rejected {
		t.Fatal(result, err)
	}
	r, e = h.SetQueue(ctx, []Media{media("a")}, 0)
	initial := await(t, r, e).Snapshot
	r, e = h.Seek(ctx, time.Second)
	if e != nil {
		t.Fatal(e)
	}
	failed, err := r.Wait(ctx)
	if err == nil || failed.Status != CommandFailed || failed.Snapshot.AppliedGeneration != initial.AppliedGeneration || failed.Snapshot.State != Failed {
		t.Fatal(failed, err)
	}
	r, e = h.Seek(ctx, 2*time.Second)
	s := await(t, r, e).Snapshot
	if s.AppliedGeneration.Seek != 2 || s.PositionFrames != 96000 || s.EffectivePlaying || s.Current.ID != "a" {
		t.Fatal(s)
	}
}

// A source returning an obsolete seek after selection changed is closed; its
// position and end status cannot be assigned to the new track.
func TestSeekSupersededBySelection(t *testing.T) {
	entered, release := make(chan struct{}, 1), make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	late := &fakeTransport{state: Ended, position: 999999}
	h := newTestHandle(t, func(_ context.Context, _ Config, m Media, _ pcm.Generation, pos time.Duration) (transport, error) {
		if pos > 0 {
			entered <- struct{}{}
			<-release
			return late, nil
		}
		if m.ID == "b" {
			if closed, _ := late.values(); closed != 1 {
				t.Error("late seek not closed")
			}
		}
		return &fakeTransport{state: Paused}, nil
	})
	ctx := context.Background()
	r, e := h.SetQueue(ctx, []Media{media("a"), media("b")}, 0)
	await(t, r, e)
	r, e = h.Seek(ctx, time.Second)
	if e != nil {
		t.Fatal(e)
	}
	waitSignal(t, entered)
	next, e := h.Select(ctx, 1)
	if e != nil {
		t.Fatal(e)
	}
	waitSnapshot(t, h, func(s Snapshot) bool { return s.RequestedCommand == next.ID() })
	close(release)
	s := await(t, next, nil).Snapshot
	if s.Current.ID != "b" || s.PositionFrames != 0 || s.SeekTarget != 0 || s.State != Paused {
		t.Fatal(s)
	}
	if _, err := r.Wait(ctx); !errors.Is(err, ErrSuperseded) {
		t.Fatal(err)
	}
}

// Native seek → render → integer packing yields exactly the reference suffix,
// including backward, non-block-aligned, last-frame and EOF positions. These
// full-scale vectors are offline only and never passed to a hardware output.
func TestSeekNativeSamples(t *testing.T) {
	for _, codec := range []string{"wav16", "wav24", "wav32", "flac16", "flac24"} {
		for _, rate := range []int{44100, 48000, 192000} {
			t.Run(fmt.Sprintf("%s/%d", codec, rate), func(t *testing.T) {
				bits := 16
				if codec == "wav24" || codec == "flac24" {
					bits = 24
				}
				if codec == "wav32" {
					bits = 32
				}
				f := pcm.Format{Kind: pcm.Integer, Rate: rate, Channels: 2, ChannelMask: 3, ValidBits: bits, ContainerBits: bits}
				path := filepath.Join(t.TempDir(), codec)
				write := fixture.WriteWAV
				if codec[:4] == "flac" {
					write = fixture.WriteFLAC
				}
				want := fixture.Samples(bits, 0, 6152)
				if err := write(path, f, want); err != nil {
					t.Fatal(err)
				}
				for _, target := range []int64{2053, 17, 6151, 6152, 99999} {
					ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
					d, err := decode.Open(ctx, path)
					if err != nil {
						cancel()
						t.Fatal(err)
					}
					position := time.Duration((target*int64(time.Second) + int64(rate) - 1) / int64(rate))
					at, err := seekDecoder(ctx, d, position)
					if err != nil {
						d.Close()
						cancel()
						t.Fatal(err)
					}
					if at != uint64(min(target, 6152)) {
						d.Close()
						cancel()
						t.Fatal(at, target)
					}
					p, err := render.StartAt(ctx, []decode.Decoder{d}, f, pcm.Generation{Session: 1, Seek: 2, Format: 3}, []int64{int64(at)})
					if err != nil {
						d.Close()
						cancel()
						t.Fatal(err)
					}
					buf := make([]byte, 113*f.FrameBytes())
					var wire []byte
					for !(p.Ring.Finished() && p.Ring.Buffered() == 0) {
						n := p.Ring.Read(buf)
						wire = append(wire, buf[:n*f.FrameBytes()]...)
						if ctx.Err() != nil {
							p.Close()
							cancel()
							t.Fatal("seek pipeline stalled")
						}
						runtime.Gosched()
					}
					if err = p.Wait(); err != nil {
						cancel()
						t.Fatal(err)
					}
					got := make([]int32, len(wire)/(bits/8))
					pack, _ := pcm.NewPacker(f)
					if err = pack.Unpack(got, wire); err != nil {
						cancel()
						t.Fatal(err)
					}
					cancel()
					if !slices.Equal(got, want[at*2:]) {
						t.Fatalf("suffix differs at %d", at)
					}
				}
			})
		}
	}
}

// Silent real-device verification distinguishes absolute source position from
// epoch submission and confirms paused/backward/rapid/EOF/playing seeks.
func TestSeekHardware(t *testing.T) {
	if os.Getenv("PRISM_NATIVE_SMOKE") != "1" {
		t.Skip("opt-in silent WASAPI test")
	}
	for _, codec := range []string{"wav", "flac"} {
		t.Run(codec, func(t *testing.T) {
			f := pcm.Format{Kind: pcm.Integer, Rate: 48000, Channels: 2, ChannelMask: 3, ValidBits: 16, ContainerBits: 16}
			path := filepath.Join(t.TempDir(), "silent."+codec)
			write := fixture.WriteWAV
			if codec == "flac" {
				write = fixture.WriteFLAC
			}
			if err := write(path, f, make([]int32, 48000*20*2)); err != nil {
				t.Fatal(err)
			}
			h, err := New(Config{EndpointID: os.Getenv("PRISM_NATIVE_ENDPOINT")})
			if err != nil {
				t.Fatal(err)
			}
			defer h.Close()
			ctx := context.Background()
			r, e := h.SetQueue(ctx, []Media{{Item: Item{Source: "local", ID: codec}, Path: path}}, 0)
			await(t, r, e)
			r, e = h.Seek(ctx, 1234500*time.Microsecond)
			s := await(t, r, e).Snapshot
			if s.PositionFrames != 59256 || s.SourceStartFrame != 59256 || s.EffectivePlaying || s.ContentSubmittedFrames != 0 || !s.PositionExact {
				t.Fatal(s)
			}
			r, e = h.Play(ctx)
			await(t, r, e)
			waitSnapshot(t, h, func(s Snapshot) bool { return s.PositionFrames > 59256 })
			r, e = h.Seek(ctx, 17*time.Second)
			if e != nil {
				t.Fatal(e)
			}
			r, e = h.Pause(ctx)
			s = await(t, r, e).Snapshot
			if s.SourceStartFrame != 816000 || s.EffectivePlaying || s.DesiredPlaying {
				t.Fatal(s)
			}
			r, e = h.Seek(ctx, 250*time.Millisecond)
			s = await(t, r, e).Snapshot
			if s.PositionFrames != 12000 || s.EffectivePlaying {
				t.Fatal(s)
			}
			for _, at := range []time.Duration{3 * time.Second, 7 * time.Second, time.Second} {
				r, e = h.Seek(ctx, at)
				if e != nil {
					t.Fatal(e)
				}
			}
			s = await(t, r, e).Snapshot
			if s.PositionFrames != 48000 || s.EffectivePlaying {
				t.Fatal(s)
			}
			r, e = h.Seek(ctx, 30*time.Second)
			s = await(t, r, e).Snapshot
			if s.PositionFrames != 960000 || s.State != Paused {
				t.Fatal(s)
			}
			r, e = h.Play(ctx)
			await(t, r, e)
			waitSnapshot(t, h, func(s Snapshot) bool { return s.State == Ended })
			r, e = h.Seek(ctx, 0)
			s = await(t, r, e).Snapshot
			if s.State != Paused || s.PositionFrames != 0 {
				t.Fatal(s)
			}
			r, e = h.Play(ctx)
			await(t, r, e)
			r, e = h.Seek(ctx, 19800*time.Millisecond)
			await(t, r, e)
			s = waitSnapshot(t, h, func(s Snapshot) bool { return s.State == Ended })
			if s.SourceStartFrame != 950400 || s.PositionFrames != 960000 || s.ContentSubmittedFrames != 9600 || s.Underruns != 0 || !s.PositionExact {
				t.Fatal(s)
			}
			b, _ := json.Marshal(s)
			t.Log(string(b))
		})
	}
}
