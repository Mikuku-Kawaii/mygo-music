package dsp

import (
	"errors"
	"math"
	"strings"
	"testing"
)

// Import retains disabled filters and identifies malformed parameters by line.
func TestAutoEQAndResponse(t *testing.T) {
	raw := "# preset\nPreamp: -3 dB\nFilter 1: ON PK Fc 1000 Hz Gain 6 dB Q 1.2\nFilter 2: OFF HS Fc 4000 Hz Gain -3 dB"
	c, err := ParseAutoEQ(raw, Config{Limit: true})
	if err != nil || !c.Limit || c.Filters[1].Enabled || c.Filters[1].Kind != HighShelf {
		t.Fatal(c, err)
	}
	for _, rate := range []int{44100, 48000, 96000, 192000} {
		if v, err := c.Response(rate, 1000); err != nil || math.Abs(v-3) > 1e-9 {
			t.Fatal(rate, v, err)
		}
		for _, kind := range []FilterKind{LowShelf, HighShelf} {
			f := Filter{Kind: kind, Enabled: true, Hz: 1000, DB: 6, Q: .7071067811865476}
			c := Config{}
			c.Filters[0] = f
			low, _ := c.Response(rate, 0)
			high, _ := c.Response(rate, float64(rate)/2)
			if kind == HighShelf {
				low, high = high, low
			}
			if math.Abs(low-6) > 1e-8 || math.Abs(high) > 1e-8 {
				t.Fatal(rate, kind, low, high)
			}
		}
	}
	for _, invalid := range []string{"Preamp: NaN dB", "Filter 1: ON PK Fc 8000 Hz Gain 2 dB Q Inf", "Filter 17: ON PK Fc 800 Hz Gain 2 dB Q 1"} {
		if _, err := ParseAutoEQ("\n"+invalid, c); err == nil || !strings.Contains(err.Error(), "line 2") {
			t.Fatal(invalid, err)
		}
	}
	c.Filters[0].Hz = 22050
	if c.Validate(44100) == nil {
		t.Fatal("accepted a Nyquist filter")
	}
}

// Album playback uses one gain and its peak for every tagged album member.
func TestReplayGainSelection(t *testing.T) {
	g := Gains{Track: Gain{DB: -6, Peak: .6, Present: true}, Album: Gain{DB: 4, Peak: 1.2, Present: true}}
	for _, v := range []struct {
		c    Config
		want float64
	}{
		{Config{}, 1}, {Config{ReplayGain: "track"}, db(-6)}, {Config{ReplayGain: "album"}, 1 / 1.2},
		{Config{ReplayGain: "album", Bypass: true, PreampDB: 9}, 1},
	} {
		if got := v.c.TrackGain(g); math.Abs(got-v.want) > 1e-14 {
			t.Fatal(got, v.want)
		}
	}
	g.Album = Gain{}
	if c := (Config{ReplayGain: "album"}); c.TrackGain(g) != 1 {
		t.Fatal("unexpected track fallback")
	}
	if c := (Config{ReplayGain: "album", AlbumFallback: true}); c.TrackGain(g) != db(-6) {
		t.Fatal("track fallback absent")
	}
}

// Converter length, DC gain, passband and rejection are measured from samples.
func TestSRCSampleResponse(t *testing.T) {
	for _, v := range []struct {
		in, out    int
		pass, stop float64
	}{
		{96000, 48000, 20000, 26000}, {48000, 44100, 18000, 23000}, {44100, 96000, 18000, 0},
		{192000, 48000, 20000, 30000},
	} {
		for _, hz := range []float64{0, v.pass, v.stop} {
			if hz == 0 && v.stop == 0 {
				continue
			}
			p, err := Prepare(Config{}, v.in, v.out, 32, Gains{}, 1, 1)
			if err != nil {
				t.Fatal(err)
			}
			var count int
			var sum float64
			input := v.in/5 + 13
			want := (input*v.out + v.in - 1) / v.in
			emit := func(x [2]int32) error {
				count++
				if x[0] != -x[1] {
					t.Fatal("channels diverged")
				}
				if count > 512 && count <= want-512 {
					f := float64(x[0]) / (1 << 31)
					sum += f * f
				}
				return nil
			}
			for i := range input {
				x := .5
				if hz != 0 {
					x *= math.Sin(2 * math.Pi * hz * float64(i) / float64(v.in))
				}
				if err := p.Process(Frame{x, -x}, emit); err != nil {
					t.Fatal(err)
				}
			}
			if err := p.Drain(emit); err != nil {
				t.Fatal(err)
			}
			if count != want || p.LatencyFrames() == 0 {
				t.Fatal(count, want, p.LatencyFrames())
			}
			rms := math.Sqrt(sum / float64(want-1024))
			ref := .5
			if hz > 0 {
				ref /= math.Sqrt2
			}
			gain := 20 * math.Log10(rms/ref)
			t.Logf("%d → %d Hz at %.0f Hz: %.3f dB, %d frames, %d lookahead", v.in, v.out, hz, gain, count, p.LatencyFrames())
			if hz > 0 && hz == v.stop {
				if gain > -74 {
					t.Fatal("stopband too weak", gain)
				}
			} else if math.Abs(gain) > .1 {
				t.Fatal("passband differs", gain)
			}
		}
	}
}

// A lookahead transient is limited on both channels and all effective frames
// are emitted after drain, with a fresh state on seek.
func TestLimiterLatencyAndReset(t *testing.T) {
	p, err := Prepare(Config{Limit: true}, 48000, 48000, 32, Gains{}, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	var frames [][2]int32
	emit := func(v [2]int32) error { frames = append(frames, v); return nil }
	for i := range 4000 {
		in := Frame{.25, .125}
		if i == 250 {
			in = Frame{2, 1}
		}
		if err := p.Process(in, emit); err != nil {
			t.Fatal(err)
		}
		if i < 240 && len(frames) != 0 {
			t.Fatal("ahead of lookahead")
		}
	}
	if err := p.Drain(emit); err != nil {
		t.Fatal(err)
	}
	if len(frames) != 4000 || p.LatencyFrames() != 240 || p.TailFrames() != 240 {
		t.Fatal(len(frames), p.LatencyFrames())
	}
	peak := float64(frames[250][0]) / (1 << 31)
	if math.Abs(peak-.8912509381337456) > 1e-9 || frames[250][0] != 2*frames[250][1] && frames[250][0] != 2*frames[250][1]+1 {
		t.Fatal(frames[250])
	}
	if frames[249][0] >= frames[0][0] || frames[3999][0] <= frames[251][0] {
		t.Fatal("missing lookahead or release")
	}
	p.Reset()
	frames = nil
	if err := p.Process(Frame{.25, .25}, emit); err != nil {
		t.Fatal(err)
	}
	if err := p.Drain(emit); err != nil {
		t.Fatal(err)
	}
	if len(frames) != 1 || frames[0] != [2]int32{1 << 29, 1 << 29} {
		t.Fatal(frames)
	}
}

// Quantization rounds ties, saturates and generates independent TPDF samples.
func TestQuantizationAndDither(t *testing.T) {
	p, err := Prepare(Config{}, 48000, 48000, 16, Gains{}, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	var frame [2]int32
	emit := func(v [2]int32) error { frame = v; return nil }
	if err := p.Process(Frame{-2, 2}, emit); err != nil || frame != [2]int32{-32768, 32767} {
		t.Fatal(frame, err)
	}
	if err := p.Process(Frame{.5 / 32768, 1.5 / 32768}, emit); err != nil || frame != [2]int32{0, 2} {
		t.Fatal(frame, err)
	}
	if err := p.Process(Frame{math.NaN(), 0}, emit); !errors.Is(err, ErrSample) {
		t.Fatal(err)
	}
	p, err = Prepare(Config{Dither: true}, 48000, 48000, 16, Gains{}, 1, 4242)
	if err != nil {
		t.Fatal(err)
	}
	var sum, square, product float64
	const n = 100000
	for range n {
		if err := p.Process(Frame{}, emit); err != nil {
			t.Fatal(err)
		}
		x, y := float64(frame[0]), float64(frame[1])
		sum += x
		square += x * x
		product += x * y
	}
	if math.Abs(sum/n) > .005 || math.Abs(square/n-.25) > .01 || math.Abs(product/n) > .006 {
		t.Fatal(sum/n, square/n, product/n)
	}
}

// The fixed-rate DSP chain does not allocate in steady-state processing.
func TestDSPProcessingAllocations(t *testing.T) {
	c := Config{Limit: true, Dither: true}
	c.Filters[0] = Filter{Kind: Peak, Enabled: true, Hz: 2000, DB: 5, Q: .9}
	p, err := Prepare(c, 96000, 48000, 32, Gains{}, .7, 2)
	if err != nil {
		t.Fatal(err)
	}
	emit := func([2]int32) error { return nil }
	if n := testing.AllocsPerRun(100, func() {
		for range 256 {
			if err := p.Process(Frame{.3, -.3}, emit); err != nil {
				t.Fatal(err)
			}
		}
	}); n != 0 {
		t.Fatal(n)
	}
}
