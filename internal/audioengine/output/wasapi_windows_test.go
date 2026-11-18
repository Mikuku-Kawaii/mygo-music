//go:build windows && amd64

package output

import (
	"context"
	"errors"
	"mygo-music/internal/audioengine/buffer"
	"mygo-music/internal/audioengine/pcm"
	"os"
	"runtime"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// COM callbacks stand in for a driver, exercising the real syscall wrappers
// without opening hardware. The former variadic wrapper allocated once per
// GetBuffer/ReleaseBuffer/GetPosition, which a Go-only fake could not detect.
func TestWASAPISyscallAllocations(t *testing.T) {
	packet := make([]byte, 32)
	get := syscall.NewCallback(func(_ uintptr, _ uintptr, out uintptr) uintptr {
		*(*unsafe.Pointer)(pointer(out)) = unsafe.Pointer(&packet[0])
		return 0
	})
	release := syscall.NewCallback(func(_, _, _ uintptr) uintptr { return 0 })
	position := syscall.NewCallback(func(_ uintptr, pos, qpc uintptr) uintptr {
		*(*uint64)(pointer(pos)) = 480
		*(*uint64)(pointer(qpc)) = 100000
		return 0
	})
	d := &wasapi{frames: 4, frameBytes: 8, getBuffer: get, releaseBuffer: release, getPosition: position}
	if n := testing.AllocsPerRun(100, func() {
		b, err := d.buffer()
		if err != nil {
			panic(err)
		}
		clear(b)
		if err = d.release(); err != nil {
			panic(err)
		}
		if _, _, err = d.position(); err != nil {
			panic(err)
		}
	}); n != 0 {
		t.Fatalf("%g allocations per native packet", n)
	}
	runtime.KeepAlive(packet)
}

// A power epoch invalidates both a playing and paused client before Start;
// unrelated power messages do not invalidate audio. This never sleeps the PC.
func TestPowerEpoch(t *testing.T) {
	d := &wasapi{powerEpoch: powerWatch.epoch.Load()}
	notePowerEvent(10)
	if d.powerChanged() {
		t.Fatal("unrelated power notification")
	}
	notePowerEvent(4)
	if !d.powerChanged() || !errors.Is(d.start(), ErrResumeRequired) {
		t.Fatal("old stream can resume")
	}
	if _, err := d.wait(time.Millisecond); !errors.Is(err, ErrResumeRequired) {
		t.Fatal(err)
	}
}

// Register the actual OS callback and inject its suspend event into a silent
// shared stream. Real sleep/driver resume remains a separate hardware check.
func TestPowerInterruptionHardware(t *testing.T) {
	if os.Getenv("PRISM_NATIVE_SMOKE") != "1" {
		t.Skip("opt-in silent power interruption")
	}
	f := pcm.Format{Kind: pcm.Integer, Rate: 48000, Channels: 2, ChannelMask: 3, ValidBits: 16, ContainerBits: 16}
	open := func() *Stream {
		r, _ := buffer.NewRing(65536, 4)
		r.Write(make([]byte, 48000*4))
		r.Finish()
		s, err := Open(context.Background(), Config{Shared: true, Format: f}, r)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	s := open()
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	notePowerEvent(4)
	select {
	case <-s.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("power event did not stop stream")
	}
	if !errors.Is(s.Err(), ErrResumeRequired) || s.Snapshot().State != Failed {
		t.Fatal(s.Err(), s.Snapshot())
	}
	s.Close()
	notePowerEvent(18)
	next := open()
	defer next.Close()
	if next.Snapshot().State != Ready || next.Snapshot().Presented != 0 {
		t.Fatal("reopen resumed", next.Snapshot())
	}
}

// Contention returns a typed device error until the original stream releases its lease.
func TestExclusiveContentionAndReopenHardware(t *testing.T) {
	if os.Getenv("PRISM_NATIVE_SMOKE") != "1" {
		t.Skip("opt-in silent exclusive contention")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 6*time.Second)
	defer cancel()
	format := pcm.Format{Kind: pcm.Integer, Rate: 48000, Channels: 2, ChannelMask: 3, ValidBits: 16, ContainerBits: 16}
	config := Config{EndpointID: os.Getenv("PRISM_NATIVE_ENDPOINT"), Format: format}
	open := func() (*Stream, error) {
		r, err := buffer.NewRing(24000, format.FrameBytes())
		if err != nil {
			t.Fatal(err)
		}
		r.Write(make([]byte, 24000*format.FrameBytes()))
		return Open(ctx, config, r)
	}
	first, err := open()
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if err := first.Start(ctx); err != nil {
		t.Fatal(err)
	}
	second, err := open()
	if second != nil {
		second.Close()
	}
	if FailureKind(err) != "device-busy" {
		t.Fatalf("expected a typed contention error, got %v", err)
	}
	t.Logf("competing exclusive open: %s (0x%08x)", FailureKind(err), FailureCode(err))
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	recovered, err := open()
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	if s := recovered.Snapshot(); s.State != Ready || s.Presented != 0 {
		t.Fatal("reopened device did not stay ready and stopped", s)
	}
}

// A deliberately stalled shared writer still has valid PCM queued with the
// high-load buffer. The test plays only zero samples on the specified device.
func TestSharedBufferDuringWriterStall(t *testing.T) {
	endpoint := os.Getenv("PRISM_STABLE_BUFFER_ENDPOINT")
	if endpoint == "" {
		t.Skip("opt-in silent WASAPI shared buffer test")
	}
	f := pcm.Format{Kind: pcm.Integer, Rate: 48000, Channels: 2, ChannelMask: 3, ValidBits: 16, ContainerBits: 16}
	for _, profile := range []struct {
		name   string
		stable bool
	}{{"standard", false}, {"high-load", true}} {
		t.Run(profile.name, func(t *testing.T) {
			runtime.LockOSThread()
			defer runtime.UnlockOSThread()
			d := new(wasapi)
			defer func() { d.close(); d.closeWake() }()
			info, err := d.open(Config{Shared: true, EndpointID: endpoint, Format: f, StableBuffer: profile.stable})
			if err != nil {
				t.Fatal(err)
			}
			if info.StableBuffer != profile.stable || profile.stable && info.BufferFrames < f.Rate/4 {
				t.Fatal(info)
			}
			b, err := d.buffer()
			if err != nil || len(b) != info.BufferFrames*f.FrameBytes() {
				t.Fatal(len(b), err)
			}
			clear(b)
			if err := d.release(); err != nil {
				t.Fatal(err)
			}
			if err := d.start(); err != nil {
				t.Fatal(err)
			}
			const stall = 150 * time.Millisecond
			time.Sleep(stall)
			pos, accurate, err := d.position()
			if err != nil || !accurate || pos == 0 {
				t.Fatal("device did not advance during the stall", pos, accurate, err)
			}
			b, err = d.buffer()
			if err != nil {
				t.Fatal(err)
			}
			remaining := d.padding
			if len(b) > 0 {
				clear(b)
				if err := d.release(); err != nil {
					t.Fatal(err)
				}
			}
			t.Logf("device buffer %d frames, %d still queued after %s without servicing", info.BufferFrames, remaining, stall)
			if profile.stable && remaining == 0 {
				t.Fatal("the high-load device buffer emptied")
			}
		})
	}
}

// A pending device command cannot delay a shared-buffer refill that is already due.
func TestSharedBufferControlDoesNotDelayPolling(t *testing.T) {
	handle, err := windows.CreateEvent(nil, 0, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(handle)
	d := &wasapi{control: handle, pollShared: true, pollAt: time.Now().Add(-time.Second), powerEpoch: powerWatch.epoch.Load()}
	if ready, err := d.wait(20 * time.Millisecond); !ready || err != nil {
		t.Fatal(ready, err)
	}
}
