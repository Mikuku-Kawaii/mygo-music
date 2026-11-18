package analysis

import (
	"math"
	"testing"
	"testing/synctest"
	"time"

	"mygo-music/internal/audioengine/pcm"
)

// The analyzer measures the effective signed samples in the submitted
// container. Out-of-band sound remains out of band at high input rates.
func TestSubmittedPCMLevelsAndSpectrum(t *testing.T) {
	for _, format := range []pcm.Format{
		{Kind: pcm.Integer, Rate: 48000, Channels: 2, ChannelMask: 3, ValidBits: 24, ContainerBits: 32},
		{Kind: pcm.Integer, Rate: 192000, Channels: 2, ChannelMask: 3, ValidBits: 24, ContainerBits: 24},
	} {
		m := newMeter(format)
		n := len(m.window)
		for _, high := range []bool{false, true} {
			frequencyBin := 20
			if high {
				frequencyBin = n / 4 // higher than the display range at both tested rates except 48 kHz
				if format.Rate == 48000 {
					frequencyBin = n * 3 / 8
				}
			}
			data := sinePCM(t, format, n, frequencyBin)
			p := packet{token: 1, epoch: 3, first: 120, content: 80, data: data, length: len(data)}
			var got Measurement
			m.feed(&p, func(v Measurement) { got = v })
			if !got.Valid || got.First != 120 || got.Through != uint64(120+n) || got.ContentStart != 80 {
				t.Fatal("unexpected frame coordinates", got)
			}
			if math.Abs(got.Peak[0]-.5) > .001 || math.Abs(got.Peak[1]-.25) > .001 ||
				math.Abs(got.RMS[0]-.5/math.Sqrt2) > 1e-6 || math.Abs(got.RMS[1]-.25/math.Sqrt2) > 1e-6 {
				t.Fatal("wrong sample peaks or RMS", got.Peak, got.RMS)
			}
			var band float32
			for _, level := range got.Bands {
				band = max(band, level)
			}
			if high && band > .01 || !high && band < .5 {
				t.Fatal("frequency content is in the wrong display band", format.Rate, band, high)
			}
		}
	}
}

func sinePCM(t *testing.T, format pcm.Format, n, bin int) []byte {
	t.Helper()
	pack, err := pcm.NewPacker(format)
	if err != nil {
		t.Fatal(err)
	}
	samples := make([]int32, n*2)
	scale := math.Ldexp(1, format.ValidBits-1)
	for i := range n {
		v := math.Sin(float64(i)*float64(bin)*2*math.Pi/float64(n)) * scale
		samples[2*i], samples[2*i+1] = int32(math.Round(v/2)), int32(math.Round(-v/4))
	}
	data := make([]byte, n*format.FrameBytes())
	if err := pack.Pack(data, samples); err != nil {
		t.Fatal(err)
	}
	return data
}

// A copied packet is invisible until committed, cannot be shown ahead of the
// device clock, and expires on missing packets, seek and window visibility.
func TestTapUsesOnlyPresentedCurrentPCM(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		format := pcm.Format{Kind: pcm.Integer, Rate: 48000, Channels: 2, ChannelMask: 3, ValidBits: 24, ContainerBits: 32}
		var subscription Subscription
		subscription.SetEnabled(true)
		n := windowSize(format.Rate)
		tap := New(&subscription, format, n)
		defer tap.Close()
		data := sinePCM(t, format, n, 32)
		if !tap.Stage(data, 2, 0, 0) {
			t.Fatal("fresh tap rejected a packet")
		}
		clear(data) // caller loses its device buffer after ReleaseBuffer
		time.Sleep(10 * time.Millisecond)
		if tap.At(2, uint64(n), 0).Valid {
			t.Fatal("staged but uncommitted packet was used")
		}
		tap.Commit()
		time.Sleep(10 * time.Millisecond)
		if tap.At(2, uint64(n-1), 0).Valid {
			t.Fatal("samples were published ahead of presentation")
		}
		if m := tap.At(2, uint64(n), 0); !m.Valid || m.Peak[0] < .49 {
			t.Fatal("tap did not keep its own committed PCM copy", m)
		}
		if tap.At(2, uint64(n), 1).Valid || tap.At(3, uint64(n), 0).Valid || tap.At(2, uint64(n+4800), 0).Valid {
			t.Fatal("measurement leaked across a track, seek or long gap")
		}
		subscription.SetEnabled(false)
		if tap.Stage(data, 2, uint64(n), uint64(n)) || tap.At(2, uint64(n), 0).Valid {
			t.Fatal("hidden view kept its audio tap")
		}
		subscription.SetEnabled(true)
		if tap.At(2, uint64(n), 0).Valid {
			t.Fatal("reopening exposed the old subscription's samples")
		}
	})
}

// A full analyzer backlog drops its own copy with no allocation or write to
// unread slots. Analysis can never hold the producer at this point.
func TestFullTapDropsWithoutAllocation(t *testing.T) {
	var subscription Subscription
	subscription.SetEnabled(true)
	tap := &Tap{sub: &subscription}
	tap.tail.Store(packetSlots)
	packet := [8]byte{1, 2}
	if n := testing.AllocsPerRun(100, func() {
		if tap.Stage(packet[:], 1, 100, 100) {
			t.Fatal("full tap accepted another slot")
		}
	}); n != 0 || tap.Dropped() != 101 || tap.tail.Load() != packetSlots {
		t.Fatal(n, tap.Dropped(), tap.tail.Load())
	}
}
