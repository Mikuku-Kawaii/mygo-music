package output

import (
	"context"
	"errors"
	"math"
	"mygo-music/internal/audioengine/buffer"
	"testing"
)

type gainDevice struct {
	*fakeDevice
	gain float64
}

func (d *gainDevice) open(c Config) (Opened, error) {
	i, e := d.fakeDevice.open(c)
	i.Shared = c.Shared
	return i, e
}
func (d *gainDevice) volume(v float64) error { d.gain = v; return nil }

func TestSharedGainAndPartialPacket(t *testing.T) {
	d := &gainDevice{fakeDevice: newFake()}
	r, _ := buffer.NewRing(16, 8)
	r.Write(make([]byte, 64))
	r.Finish()
	s, err := openWith(context.Background(), Config{Shared: true, Format: testFormat()}, r, d)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SetVolume(context.Background(), .25); err != nil || d.gain != .25 || s.Snapshot().State != Ready {
		t.Fatal(err, d.gain, s.Snapshot())
	}
	for _, v := range []float64{-1, 2, math.NaN()} {
		if s.SetVolume(context.Background(), v) == nil {
			t.Fatal("invalid gain admitted", v)
		}
	}
	s.Close()
	// A shared event can offer fewer frames than GetBufferSize, or none.
	f := newFake()
	info, _ := f.open(Config{Format: testFormat()})
	stream := &Stream{n: f}
	var end uint64
	known := false
	for _, frames := range []int{0, 2, 3, 4} {
		f.packet = make([]byte, frames*8)
		if err = stream.fill(r, info, &end, &known); err != nil {
			t.Fatal(err)
		}
	}
	snap := stream.Snapshot()
	if snap.Submitted != 9 || snap.ContentSubmitted != 8 || snap.TrailingSilence != 1 || snap.Underruns != 0 || !known || end != 8 {
		t.Fatal(snap, end, known)
	}
}

func TestFailureKinds(t *testing.T) {
	for _, v := range []struct {
		err  error
		kind string
	}{
		{&DeviceError{Code: 0x88890004}, "device-lost"}, {&DeviceError{Code: 0x88890026}, "device-lost"},
		{&DeviceError{Code: 0x88890010}, "service-stopped"}, {&DeviceError{Code: 0x8889000a}, "device-busy"},
		{&DeviceError{Code: 0x88890008}, "format-unsupported"}, {ErrDefaultChanged, "default-changed"},
		{ErrResumeRequired, "resume-required"}, {ErrClockStopped, "clock-stopped"},
	} {
		if got := FailureKind(errors.Join(v.err, errors.New("close"))); got != v.kind {
			t.Fatal(got, v.kind)
		}
	}
}
