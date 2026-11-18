package dsp

import (
	"errors"
	"math"
)

type Frame [2]float64

var ErrSample = errors.New("DSP: non-finite processed sample")

// Processor owns preallocated, stereo state. natively aligned float64 frames
// arrive in source order. The callback receives final int32 PCM at OutputRate.
type Processor struct {
	filters                            []biquad
	src                                *resampler
	limiter                            *limiter
	gain, target, volume, targetVolume float64
	dither                             bool
	losslessInput                      bool
	bits                               int
	random                             uint64
	finished                           bool
	channels                           *channelProcessor
}

func Prepare(c Config, inputRate, outputRate, bits int, g Gains, volume float64, seed uint64) (*Processor, error) {
	if inputRate < 8000 || inputRate > 384000 || outputRate < 8000 || outputRate > 384000 ||
		(bits != 16 && bits != 24 && bits != 32) || !between(volume, 0, 1) {
		return nil, errors.New("DSP: unsupported rate, precision or gain")
	}
	if err := c.Validate(inputRate); err != nil {
		return nil, err
	}
	p := &Processor{dither: c.Dither && !c.Bypass, bits: bits, random: max(seed, 1)}
	p.gain, p.target = c.TrackGain(g), c.TrackGain(g)
	p.volume, p.targetVolume = volume, volume
	if !c.Bypass {
		c.eachEQ(func(f Filter) {
			b, _ := coefficients(f, inputRate)
			p.filters = append(p.filters, b)
		})
		if c.SwapChannels || c.Balance != 0 || c.Crossfeed != 0 {
			p.channels = &channelProcessor{
				swap: c.SwapChannels, cross: c.Crossfeed,
				balance: Frame{1 - max(c.Balance, 0), 1 + min(c.Balance, 0)},
				alpha:   1 - math.Exp(-2*math.Pi*700/float64(inputRate)),
			}
		}
		if c.Limit {
			p.limiter = newLimiter(outputRate)
		}
	}
	if inputRate != outputRate {
		p.src = newResampler(inputRate, outputRate)
	}
	return p, nil
}

// SetGain and SetVolume are called by Render at a block boundary. A smooth
// approach to the target keeps changes continuous across short blocks.
func (p *Processor) SetGain(gain float64)     { p.target = gain }
func (p *Processor) SetVolume(volume float64) { p.targetVolume = volume }

func (p *Processor) PendingFrames() uint64 {
	var pending uint64
	if p.src != nil {
		pending += uint64((p.src.written*int64(p.src.out) - p.src.phase + int64(p.src.in) - 1) / int64(p.src.in))
	}
	if p.limiter != nil {
		pending += uint64(p.limiter.written - p.limiter.read)
	}
	return pending
}

func (p *Processor) SetInitialVolume(volume float64) {
	p.volume, p.targetVolume = volume, volume
}
func (p *Processor) SetInitialGain(gain float64) { p.gain, p.target = gain, gain }

func (p *Processor) SetInputBits(bits int) {
	p.losslessInput = bits > 0 && bits <= p.bits
}

// LatencyFrames is algorithmic lookahead expressed at the output rate.
// Drain compensates this lookahead without adding source-duration silence.
func (p *Processor) LatencyFrames() int {
	n := 0
	if p.src != nil {
		n = (p.src.half*p.src.out + p.src.in - 1) / p.src.in
	}
	if p.limiter != nil {
		n += p.limiter.delay
	}
	return n
}

// TailFrames reports the bounded number of effective frames Drain may emit.
// The PEQ ring-out past the declared end is intentionally cropped.
func (p *Processor) TailFrames() int { return p.LatencyFrames() }

func (p *Processor) Reset() {
	for i := range p.filters {
		p.filters[i].z1, p.filters[i].z2 = Frame{}, Frame{}
	}
	if p.channels != nil {
		p.channels.low = Frame{}
	}
	if p.src != nil {
		p.src.reset()
	}
	if p.limiter != nil {
		p.limiter.reset()
	}
	p.gain, p.volume, p.finished = p.target, p.targetVolume, false
}

func (p *Processor) Process(v Frame, emit func([2]int32) error) error {
	if p.finished {
		return errors.New("DSP: process after drain")
	}
	if !finite(v) {
		return ErrSample
	}
	p.gain += (p.target - p.gain) / 64
	v[0], v[1] = v[0]*p.gain, v[1]*p.gain
	for i := range p.filters {
		v = p.filters[i].apply(v)
	}
	if p.channels != nil {
		v = p.channels.apply(v)
	}
	if !finite(v) {
		return ErrSample
	}
	if p.src == nil {
		return p.post(v, emit)
	}
	p.src.push(v)
	for p.src.ready() {
		if err := p.post(p.src.next(), emit); err != nil {
			return err
		}
	}
	return nil
}

func (p *Processor) Drain(emit func([2]int32) error) error {
	if p.finished {
		return nil
	}
	p.finished = true
	if p.src != nil {
		end := p.src.written * int64(p.src.out)
		for p.src.phase < end {
			if !p.src.ready() {
				p.src.push(Frame{})
				continue
			}
			if err := p.post(p.src.next(), emit); err != nil {
				return err
			}
		}
	}
	if p.limiter != nil {
		for p.limiter.read < p.limiter.written {
			if err := p.quantize(p.limiter.pop(), emit); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *Processor) post(v Frame, emit func([2]int32) error) error {
	if !finite(v) {
		return ErrSample
	}
	p.volume += (p.targetVolume - p.volume) / 64
	v[0], v[1] = v[0]*p.volume, v[1]*p.volume
	if p.limiter != nil {
		p.limiter.push(v)
		if p.limiter.written-p.limiter.read <= int64(p.limiter.delay) {
			return nil
		}
		v = p.limiter.pop()
	}
	return p.quantize(v, emit)
}

func (p *Processor) quantize(v Frame, emit func([2]int32) error) error {
	var result [2]int32
	scale := float64(int64(1) << (p.bits - 1))
	widening := p.losslessInput && p.src == nil && p.limiter == nil && p.channels == nil && len(p.filters) == 0 && p.gain == 1 && p.volume == 1
	for i, s := range v {
		x := s * scale
		if p.dither && !widening && (p.targetVolume != 0 || p.volume > 1e-12) {
			x += p.uniform() - p.uniform()
		}
		result[i] = int32(math.RoundToEven(max(-scale, min(scale-1, x))))
	}
	return emit(result)
}

// channelProcessor runs at the source rate after EQ. Swap, crossfeed and
// balance have no lookahead. The low-pass history is reset with the DSP.
type channelProcessor struct {
	swap         bool
	cross, alpha float64
	balance, low Frame
}

func (p *channelProcessor) apply(v Frame) Frame {
	if p.swap {
		v[0], v[1] = v[1], v[0]
	}
	if p.cross != 0 {
		for ch := range v {
			p.low[ch] += p.alpha * (v[ch] - p.low[ch])
		}
		v = Frame{(1-p.cross)*v[0] + p.cross*p.low[1], (1-p.cross)*v[1] + p.cross*p.low[0]}
	}
	return Frame{v[0] * p.balance[0], v[1] * p.balance[1]}
}

func (p *Processor) uniform() float64 {
	x := p.random
	x ^= x >> 12
	x ^= x << 25
	x ^= x >> 27
	p.random = x
	return float64((x*2685821657736338717)>>11) / (1 << 53)
}

func finite(v Frame) bool {
	return !math.IsNaN(v[0]) && !math.IsNaN(v[1]) && !math.IsInf(v[0], 0) && !math.IsInf(v[1], 0)
}

type peak struct {
	index int64
	level float64
}
type limiter struct {
	delay                     int
	release, gain             float64
	frames                    []Frame
	peaks                     []peak
	read, written, head, tail int64
}

func newLimiter(rate int) *limiter {
	delay := max(1, rate/200) // 5 ms
	return &limiter{delay: delay, release: 1 - math.Exp(-1/(.05*float64(rate))), gain: 1,
		frames: make([]Frame, delay+1), peaks: make([]peak, delay+1)}
}

func (l *limiter) reset() {
	l.read, l.written, l.head, l.tail, l.gain = 0, 0, 0, 0, 1
}

func (l *limiter) push(v Frame) {
	n := int64(len(l.frames))
	l.frames[l.written%n] = v
	level := max(math.Abs(v[0]), math.Abs(v[1]))
	for l.tail > l.head && l.peaks[(l.tail-1)%n].level <= level {
		l.tail--
	}
	l.peaks[l.tail%n] = peak{l.written, level}
	l.tail++
	l.written++
}

func (l *limiter) pop() Frame {
	n := int64(len(l.frames))
	p := l.peaks[l.head%n]
	const ceiling = .8912509381337456 // -1 dBFS sample peak
	target := 1.0
	if p.level > ceiling {
		target = ceiling / p.level
	}
	l.gain = min(target, l.gain+(1-l.gain)*l.release)
	v := l.frames[l.read%n]
	if p.index == l.read {
		l.head++
	}
	l.read++
	return Frame{v[0] * l.gain, v[1] * l.gain}
}
