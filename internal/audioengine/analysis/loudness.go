package analysis

import (
	"context"
	"errors"
	"io"
	"math"
	"sync"

	"mygo-music/internal/audioengine/decode"
	"mygo-music/internal/audioengine/pcm"
)

// ProgramLoudness measures decoded source audio before the playback DSP.
// Values are absent for gated/too-short programmes and digital zero.
type ProgramLoudness struct {
	Rate, Channels int
	Frames         int64
	IntegratedLUFS *float64 `json:",omitempty"`
	SamplePeakDBFS *float64 `json:",omitempty"`
	TruePeakDBTP   *float64 `json:",omitempty"`
}

func ScanLoudness(ctx context.Context, path string) (ProgramLoudness, error) {
	d, err := decode.OpenProcessed(ctx, path)
	if err != nil {
		return ProgramLoudness{}, err
	}
	defer d.Close()
	info := d.Info()
	f := info.Format
	channels := f.Channels
	if info.SourceChannels != 0 {
		channels = info.SourceChannels
	}
	if channels < 1 || channels > 2 || channels > f.Channels || f.Rate < 8000 {
		return ProgramLoudness{}, errors.New("loudness: expected supported mono or stereo PCM")
	}
	a := newProgramMeter(f.Rate, math.Pow(10, (-70+.691)/10))
	p := new(peakMeter)
	r := programReader{d: d, format: f, channels: channels}
	if err := r.run(ctx, a, p); err != nil {
		return ProgramLoudness{}, err
	}
	// Flush fractional peaks near the effective end. These zeros do not
	// contribute to the duration or to the loudness gates.
	for range peakTaps {
		p.add([2]float64{})
	}
	result := ProgramLoudness{Rate: f.Rate, Channels: channels, Frames: a.frames, SamplePeakDBFS: dbAmplitude(p.sample), TruePeakDBTP: dbAmplitude(p.max)}
	if a.count == 0 {
		return result, nil
	}
	// The relative gate depends on the complete programme. A second
	// cancellable pass keeps the gate exact with duration-independent memory.
	if err := d.Seek(ctx, 0); err != nil {
		return ProgramLoudness{}, err
	}
	b := newProgramMeter(f.Rate, max(a.threshold, a.sum/float64(a.count)/10))
	if err := r.run(ctx, b, nil); err != nil {
		return ProgramLoudness{}, err
	}
	if b.frames != a.frames {
		return ProgramLoudness{}, errors.New("loudness: programme changed between passes")
	}
	if b.count > 0 {
		v := -.691 + 10*math.Log10(b.sum/float64(b.count))
		result.IntegratedLUFS = &v
	}
	return result, nil
}

func dbAmplitude(value float64) *float64 {
	if value == 0 {
		return nil
	}
	v := 20 * math.Log10(value)
	return &v
}

type programReader struct {
	d        decode.Decoder
	format   pcm.Format
	channels int
	ints     [4096]int32
	floats   [4096]float64
}

func (r *programReader) run(ctx context.Context, meter *programMeter, peak *peakMeter) error {
	f := r.format
	f64, _ := r.d.(decode.Float64Decoder)
	scale := math.Ldexp(1, 1-f.ValidBits)
	for ctx.Err() == nil {
		var n int
		var err error
		if f64 != nil {
			n, err = f64.ReadFloat64(ctx, r.floats[:])
		} else {
			n, err = r.d.Read(ctx, r.ints[:])
		}
		for i := range n {
			var v [2]float64
			for ch := range r.channels {
				at := i*f.Channels + ch
				switch f.Kind {
				case pcm.Float64:
					v[ch] = r.floats[at]
				case pcm.Float32:
					v[ch] = float64(math.Float32frombits(uint32(r.ints[at])))
				default:
					v[ch] = float64(r.ints[at]) * scale
				}
				if math.IsNaN(v[ch]) || math.IsInf(v[ch], 0) {
					return errors.New("loudness: non-finite decoded sample")
				}
			}
			meter.add(v)
			if peak != nil {
				peak.add(v)
			}
		}
		if err == io.EOF {
			return ctx.Err()
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrNoProgress
		}
	}
	return ctx.Err()
}

type weightFilter struct {
	b, a   [3]float64
	s1, s2 [2]float64
}

func (f *weightFilter) process(x [2]float64) [2]float64 {
	var y [2]float64
	for ch := range x {
		y[ch] = f.b[0]*x[ch] + f.s1[ch]
		f.s1[ch] = f.b[1]*x[ch] - f.a[1]*y[ch] + f.s2[ch]
		f.s2[ch] = f.b[2]*x[ch] - f.a[2]*y[ch]
	}
	return y
}

// ITU-R BS.1770-5 Annex 1, tables 1 and 2. For other sample rates, substitute
// the bilinear transform of the same analog prototype into each polynomial.
func weightAt(rate int, b, a [3]float64) weightFilter {
	r := float64(rate) / 48000
	p, q := 1-r, 1+r
	warp := func(c [3]float64) [3]float64 {
		return [3]float64{
			c[0]*q*q + c[1]*p*q + c[2]*p*p,
			2*(c[0]+c[2])*p*q + c[1]*(p*p+q*q),
			c[0]*p*p + c[1]*p*q + c[2]*q*q,
		}
	}
	x, y := warp(b), warp(a)
	for i := range x {
		x[i] /= y[0]
	}
	y[1], y[2], y[0] = y[1]/y[0], y[2]/y[0], 1
	return weightFilter{b: x, a: y}
}

type programMeter struct {
	rate, frames, steps, count int64
	history                    [4]float64
	shelf, highPass            weightFilter
	threshold, bucket, sum     float64
}

func newProgramMeter(rate int, threshold float64) *programMeter {
	return &programMeter{rate: int64(rate), threshold: threshold,
		shelf:    weightAt(rate, [3]float64{1.53512485958697, -2.69169618940638, 1.19839281085285}, [3]float64{1, -1.69065929318241, .73248077421585}),
		highPass: weightAt(rate, [3]float64{1, -2, 1}, [3]float64{1, -1.99004745483398, .99007225036621}),
	}
}

func (m *programMeter) add(v [2]float64) {
	y := m.highPass.process(m.shelf.process(v))
	m.bucket += y[0]*y[0] + y[1]*y[1]
	m.frames++
	if m.frames != ((m.steps+1)*m.rate+5)/10 {
		return
	}
	m.history[m.steps%4], m.bucket = m.bucket, 0
	m.steps++
	if m.steps < 4 {
		return
	}
	n := m.frames - ((m.steps-4)*m.rate+5)/10
	energy := (m.history[0] + m.history[1] + m.history[2] + m.history[3]) / float64(n)
	if energy > m.threshold {
		m.sum, m.count = m.sum+energy, m.count+1
	}
}

const peakTaps = 64

// Four-times oversampling includes the original samples and three
// fractional phases of a DC-normalized Blackman-windowed sinc.
var truePeakWeights = sync.OnceValue(func() *[3][peakTaps]float64 {
	var out [3][peakTaps]float64
	for phase := range out {
		var total float64
		for i := range peakTaps {
			t := float64(i-peakTaps/2+1) - float64(phase+1)/4
			x := math.Pi * t
			a := 2 * math.Pi * float64(i) / (peakTaps - 1)
			weight := math.Sin(x) / x * (.42 - .5*math.Cos(a) + .08*math.Cos(2*a))
			out[phase][i] = weight
			total += weight
		}
		for i := range out[phase] {
			out[phase][i] /= total
		}
	}
	return &out
})

type peakMeter struct {
	history     [peakTaps][2]float64
	at          int
	sample, max float64
}

func (m *peakMeter) add(v [2]float64) {
	m.sample = max(m.sample, math.Abs(v[0]), math.Abs(v[1]))
	m.max = max(m.max, m.sample)
	m.history[m.at] = v
	m.at = (m.at + 1) % peakTaps
	w := truePeakWeights()
	for phase := range w {
		var l, r float64
		for i, coeff := range w[phase][:] {
			x := m.history[(m.at+i)%peakTaps]
			l += coeff * x[0]
			r += coeff * x[1]
		}
		m.max = max(m.max, math.Abs(l), math.Abs(r))
	}
}
