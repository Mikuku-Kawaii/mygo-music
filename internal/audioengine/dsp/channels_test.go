package dsp

import (
	"math"
	"testing"
)

func TestChannelRoutingAndBypass(t *testing.T) {
	c := Config{SwapChannels: true, Balance: -1}
	input := Frame{.125, .25}
	for _, bypass := range []bool{false, true} {
		c.Bypass = bypass
		p, err := Prepare(c, 48000, 48000, 24, Gains{}, 1, 42)
		if err != nil {
			t.Fatal(err)
		}
		var got [2]int32
		if err := p.Process(input, func(v [2]int32) error { got = v; return nil }); err != nil {
			t.Fatal(err)
		}
		want := [2]int32{1 << 21, 0}
		if bypass {
			want = [2]int32{1 << 20, 1 << 21}
		}
		if got != want || p.LatencyFrames() != 0 || p.TailFrames() != 0 {
			t.Fatal(got, p.LatencyFrames(), p.TailFrames())
		}
	}
}

func TestCrossfeedResetAndGraphicEQ(t *testing.T) {
	p, err := Prepare(Config{Crossfeed: .25}, 48000, 48000, 24, Gains{}, 1, 42)
	if err != nil {
		t.Fatal(err)
	}
	var got [2]int32
	emit := func(v [2]int32) error { got = v; return nil }
	for range 512 {
		if err := p.Process(Frame{.5, 0}, emit); err != nil {
			t.Fatal(err)
		}
	}
	if got != [2]int32{3 << 20, 1 << 20} {
		t.Fatal("crossfeed DC gain", got)
	}
	p.Reset()
	if err := p.Process(Frame{}, emit); err != nil || got != [2]int32{} {
		t.Fatal("crossfeed history survived reset", got, err)
	}
	c := Config{}
	c.Graphic[5] = 6
	p, err = Prepare(c, 48000, 48000, 24, Gains{}, 1, 42)
	if err != nil {
		t.Fatal(err)
	}
	var energy float64
	for i := range 4800 {
		v := .125 * math.Sin(2*math.Pi*float64(i)/48)
		if err := p.Process(Frame{v, v}, emit); err != nil {
			t.Fatal(err)
		}
		if i >= 2400 {
			s := float64(got[0]) / (1 << 23)
			energy += s * s
		}
	}
	gain := 10 * math.Log10(energy/(1200*.125*.125))
	if math.Abs(gain-6) > .01 || p.LatencyFrames() != 0 {
		t.Fatal("graphic EQ gain", gain, p.LatencyFrames())
	}
	c.Graphic[9] = 1
	if err := c.Validate(32000); err == nil {
		t.Fatal("band at Nyquist was accepted")
	}
}
