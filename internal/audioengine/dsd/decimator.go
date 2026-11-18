package dsd

import (
	"context"
	"io"
	"math"
)

const PCMRate = 88200
const LookaheadFrames = 12
const decodeFrames = 512

// Decimator is a centered Blackman-windowed low-pass FIR. Its phase is tied
// to the source sample, so a random seek reconstructs the same float64 frame
// as a linear read. Outside the source it convolves with DSD idle bits.
type Decimator struct {
	source  *Reader
	step    int // DSD byte pairs per output frame
	weights [][256]float64
	scratch []byte
}

func NewDecimator(source *Reader) *Decimator {
	step := source.Info().BitRate / PCMRate / 8
	taps := step * 8 * 2 * LookaheadFrames
	m := &Decimator{
		source: source, step: step, weights: make([][256]float64, taps/8),
		scratch: make([]byte, 2*(decodeFrames*step+taps/8)),
	}
	weights := make([]float64, taps)
	normalized := 2 * 28000.0 / float64(source.Info().BitRate)
	var sum float64
	for i := range weights {
		x := float64(i) - float64(taps-1)/2
		y := math.Pi * normalized * x
		a := 2 * math.Pi * float64(i) / float64(taps-1)
		weights[i] = normalized * math.Sin(y) / y * (.42 - .5*math.Cos(a) + .08*math.Cos(2*a))
		sum += weights[i]
	}
	for group := range m.weights {
		for value := range m.weights[group] {
			for bit := range 8 {
				w := weights[8*group+bit] / sum
				if value&(0x80>>bit) == 0 {
					w = -w
				}
				m.weights[group][value] += w
			}
		}
	}
	return m
}

func (m *Decimator) Frames() int64 {
	return ceilUnits(m.source.Info().Bits, int64(m.step*8))
}

func ceilUnits(count, unit int64) int64 {
	return 1 + (count-1)/unit
}

func (m *Decimator) Read(ctx context.Context, dst []float64, frame int64) (int, error) {
	if frame < 0 || frame > m.Frames() || len(dst)%2 != 0 {
		return 0, ErrContainer
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if frame == m.Frames() {
		return 0, io.EOF
	}
	count := int(min(int64(min(len(dst)/2, decodeFrames)), m.Frames()-frame))
	if count == 0 {
		return 0, nil
	}
	bytes := m.scratch[:2*((count-1)*m.step+len(m.weights))]
	for i := range bytes {
		bytes[i] = Idle
	}
	first := frame*int64(m.step) - int64(len(m.weights)/2)
	skip := max(-first, 0)
	_, err := m.source.ReadBytes(ctx, bytes[2*skip:], max(first, 0))
	if err != nil && err != io.EOF {
		return 0, err
	}
	for i := range count {
		src := bytes[2*i*m.step:]
		var l, r float64
		for k := range m.weights {
			l += m.weights[k][src[2*k]]
			r += m.weights[k][src[2*k+1]]
		}
		dst[2*i], dst[2*i+1] = l, r
	}
	if frame+int64(count) == m.Frames() {
		return count, io.EOF
	}
	return count, nil
}
