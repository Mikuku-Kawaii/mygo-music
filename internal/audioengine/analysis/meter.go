package analysis

import (
	"math"
	"math/cmplx"

	"mygo-music/internal/audioengine/pcm"
)

type meter struct {
	width             int
	scale             float64
	window            []float64
	fft               [2][]complex128
	edges             [Bands + 1]int
	at                int
	expect, expectPCM uint64
	frame             Measurement
	sumSquares        [2]float64
}

func newMeter(format pcm.Format) *meter {
	n := windowSize(format.Rate)
	m := &meter{width: format.ContainerBits / 8,
		scale: math.Ldexp(1, 1-format.ContainerBits), window: make([]float64, n)}
	for i := range m.window {
		m.window[i] = .5 - .5*math.Cos(2*math.Pi*float64(i)/float64(n-1))
	}
	for ch := range m.fft {
		m.fft[ch] = make([]complex128, n)
	}
	hi := min(14000., float64(format.Rate)/2)
	lo := min(40., hi/2)
	for i := range m.edges {
		f := lo * math.Pow(hi/lo, float64(i)/Bands)
		m.edges[i] = min(n/2, max(1, int(f*float64(n)/float64(format.Rate))))
	}
	return m
}

func (m *meter) feed(p *packet, publish func(Measurement)) {
	if p.token != m.frame.Token || p.epoch != m.frame.Epoch || p.first != m.expect || p.content != m.expectPCM {
		m.at = 0
	}
	data := p.data[:p.length]
	frameBytes := m.width * 2
	total := len(data) / frameBytes
	m.expect, m.expectPCM = p.first+uint64(total), p.content+uint64(total)
	for i := 0; i < total; {
		if m.at == 0 {
			m.frame = Measurement{Valid: true, Token: p.token, Epoch: p.epoch,
				First: p.first + uint64(i), ContentStart: p.content + uint64(i)}
			m.sumSquares = [2]float64{}
		}
		for ch := range 2 {
			value := sample(data[i*frameBytes+ch*m.width:][:m.width]) * m.scale
			m.frame.Peak[ch] = max(m.frame.Peak[ch], math.Abs(value))
			m.sumSquares[ch] += value * value
			m.fft[ch][m.at] = complex(value*m.window[m.at], 0)
		}
		i++
		m.at++
		if m.at == len(m.window) {
			m.frame.Through = p.first + uint64(i)
			publish(m.measure())
			m.at = 0
		}
	}
}

func sample(b []byte) float64 {
	var raw uint32
	for i, v := range b {
		raw |= uint32(v) << (8 * i)
	}
	shift := 32 - 8*len(b)
	return float64(int32(raw<<shift) >> shift)
}

func (m *meter) measure() Measurement {
	for ch := range m.fft {
		m.frame.RMS[ch] = math.Sqrt(m.sumSquares[ch] / float64(len(m.window)))
		FFT(m.fft[ch])
	}
	for b := range Bands {
		// Adjacent low bands may resolve to the same bin. Reuse that bin
		// rather than shifting every higher edge away from its frequency.
		lo, hi := m.edges[b], m.edges[b+1]
		lo = min(lo, len(m.window)/2-1)
		hi = max(hi, lo+1)
		var sum float64
		for k := lo; k < hi; k++ {
			sum += (cmplx.Abs(m.fft[0][k]) + cmplx.Abs(m.fft[1][k])) / 2
		}
		mag := sum / float64(hi-lo) / float64(len(m.window)/4)
		// Match the compatible visualizer's presentation scale.
		db := 20 * math.Log10(mag*(1+float64(b)/6)+1e-9)
		m.frame.Bands[b] = float32(max(0., min(1., (db+60)/52)))
	}
	return m.frame
}

// FFT transforms x in place. Its length must be a power of two.
func FFT(x []complex128) {
	n := len(x)
	for i, j := 1, 0; i < n; i++ {
		bit := n >> 1
		for ; j&bit != 0; bit >>= 1 {
			j ^= bit
		}
		j ^= bit
		if i < j {
			x[i], x[j] = x[j], x[i]
		}
	}
	for size := 2; size <= n; size <<= 1 {
		w := cmplx.Exp(complex(0, -2*math.Pi/float64(size)))
		for start := 0; start < n; start += size {
			wk := complex(1, 0)
			for k := range size / 2 {
				a, b := x[start+k], x[start+k+size/2]*wk
				x[start+k], x[start+k+size/2] = a+b, a-b
				wk *= w
			}
		}
	}
}
