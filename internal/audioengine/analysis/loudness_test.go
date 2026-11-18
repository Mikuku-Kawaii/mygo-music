package analysis

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"testing"

	"mygo-music/internal/audioengine/fixture"
	"mygo-music/internal/audioengine/pcm"
)

func TestIntegratedLoudnessGates(t *testing.T) {
	for _, rate := range []int{44100, 48000} {
		// Exercise both gates. The lower plateaus contribute negligible energy
		// to boundary blocks and are excluded as complete blocks.
		levels := []float64{-72, -36, -23, -23, -23, -23, -36, -72}
		samples := make([]int32, len(levels)*rate*2)
		for i, level := range levels {
			a := math.Pow(10, level/20) * (1 << 23)
			for n := range rate {
				v := int32(math.Round(a * math.Sin(2*math.Pi*1000*float64(n)/float64(rate))))
				at := 2 * (i*rate + n)
				samples[at], samples[at+1] = v, v
			}
		}
		r := measureTestPCM(t, rate, samples)
		// There are 37 full loud windows and six mixed windows with a total
		// of three full windows of loud energy and three of quiet energy.
		want := -23 + 10*math.Log10((40+3*math.Pow(10, -1.3))/43)
		if r.Frames != int64(len(samples)/2) || r.IntegratedLUFS == nil || math.Abs(*r.IntegratedLUFS-want) > .08 {
			t.Fatalf("%d Hz weighted and gated result: %+v", rate, r)
		}
		t.Logf("%d Hz combined gates: %.4f LUFS, %.4f dBTP", rate, *r.IntegratedLUFS, *r.TruePeakDBTP)
	}
}

func TestTruePeakFractionalPhaseAndDigitalSilence(t *testing.T) {
	const rate = 48000
	samples := make([]int32, rate*2)
	for i := range rate {
		envelope := float64(min(i, rate-1-i, rate/100)) / (rate / 100)
		v := int32(math.Round(envelope * (1 << 22) * math.Sin(math.Pi/2*float64(i)+math.Pi/4)))
		samples[2*i], samples[2*i+1] = v, v
	}
	r := measureTestPCM(t, rate, samples)
	if r.SamplePeakDBFS == nil || r.TruePeakDBTP == nil || math.Abs(*r.SamplePeakDBFS+9.0309) > .03 || math.Abs(*r.TruePeakDBTP+6.0206) > .05 {
		t.Fatal(r)
	}
	t.Logf("12 kHz at 45 degrees: %.4f dBFS sample, %.4f dBTP", *r.SamplePeakDBFS, *r.TruePeakDBTP)
	r = measureTestPCM(t, rate, make([]int32, rate))
	if r.IntegratedLUFS != nil || r.SamplePeakDBFS != nil || r.TruePeakDBTP != nil {
		t.Fatal("digital zero should have absent dB values", r)
	}
}

func measureTestPCM(t *testing.T, rate int, samples []int32) ProgramLoudness {
	t.Helper()
	path := filepath.Join(t.TempDir(), "measurement.wav")
	f := pcm.Format{Kind: pcm.Integer, Rate: rate, Channels: 2, ChannelMask: 3, ValidBits: 24, ContainerBits: 24}
	if err := fixture.WriteWAV(path, f, samples); err != nil {
		t.Fatal(err)
	}
	r, err := ScanLoudness(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestLoudnessMissingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing.wav")
	if _, err := ScanLoudness(t.Context(), path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("a missing file was measured as silence")
	}
}

func TestLoudnessCanceled(t *testing.T) {
	path := filepath.Join(t.TempDir(), "canceled.wav")
	f := pcm.Format{Kind: pcm.Integer, Rate: 48000, Channels: 2, ChannelMask: 3, ValidBits: 16, ContainerBits: 16}
	if err := fixture.WriteWAV(path, f, make([]int32, 48000)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := ScanLoudness(ctx, path); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
