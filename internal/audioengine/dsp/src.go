package dsp

import "math"

// A centered Blackman-windowed sinc, with a source/output rational phase
// accumulator. The table trades finite phase resolution for bounded render
// work. Input lookahead is withheld, then compensated by draining exactly
// the declared duration. The caller supplies zeros only outside that range.
const phases = 512

type resampler struct {
	in, out, half, taps int
	table               []float64
	frames              []Frame
	written, phase      int64
}

func newResampler(in, out int) *resampler {
	ratio := min(1, float64(out)/float64(in))
	half := int(math.Ceil(48 / ratio))
	taps := 2*half + 1
	r := &resampler{in: in, out: out, half: half, taps: taps,
		table: make([]float64, (phases+1)*taps), frames: make([]Frame, taps+1)}
	cutoff := .92 * ratio
	for phase := 0; phase <= phases; phase++ {
		row := r.table[phase*taps : (phase+1)*taps]
		frac := float64(phase) / phases
		sum := 0.0
		for i := range row {
			x := float64(i-half) - frac
			a := math.Pi * x / float64(half)
			if math.Abs(x) > float64(half) {
				continue
			}
			window := .42 + .5*math.Cos(a) + .08*math.Cos(2*a)
			y := math.Pi * cutoff * x
			sinc := cutoff
			if y != 0 {
				sinc *= math.Sin(y) / y
			}
			row[i] = window * sinc
			sum += row[i]
		}
		for i := range row {
			row[i] /= sum
		}
	}
	return r
}

func (r *resampler) reset() { r.written, r.phase = 0, 0; clear(r.frames) }
func (r *resampler) push(v Frame) {
	r.frames[r.written%int64(len(r.frames))] = v
	r.written++
}
func (r *resampler) ready() bool {
	return r.phase/int64(r.out)+int64(r.half) < r.written
}
func (r *resampler) next() Frame {
	center, rem := r.phase/int64(r.out), r.phase%int64(r.out)
	row := int((rem*phases + int64(r.out)/2) / int64(r.out))
	weights := r.table[row*r.taps : (row+1)*r.taps]
	base := center - int64(r.half)
	start := int(max(0, -base))
	var v Frame
	index := int((base + int64(start)) % int64(len(r.frames)))
	for i := start; i < len(weights); i++ {
		f := r.frames[index]
		v[0] += f[0] * weights[i]
		v[1] += f[1] * weights[i]
		index++
		if index == len(r.frames) {
			index = 0
		}
	}
	r.phase += int64(r.in)
	return v
}
