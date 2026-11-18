package dsp

import (
	"errors"
	"math"
	"math/cmplx"
)

type biquad struct {
	b0, b1, b2, a1, a2 float64
	z1, z2             [2]float64
}

// Audio EQ Cookbook biquads. Shelf Q uses the equivalent shelf slope in
// alpha, preserving the Q convention used by parametric AutoEQ entries.
func coefficients(f Filter, rate int) (biquad, error) {
	if !validFilter(f) || f.Hz >= float64(rate)/2 {
		return biquad{}, errors.New("filter frequency must be below Nyquist and parameters must be finite")
	}
	w := 2 * math.Pi * f.Hz / float64(rate)
	s, c := math.Sincos(w)
	a := math.Pow(10, f.DB/40)
	alpha := s / (2 * f.Q)
	var b0, b1, b2, a0, a1, a2 float64
	switch f.Kind {
	case Peak:
		b0, b1, b2 = 1+alpha*a, -2*c, 1-alpha*a
		a0, a1, a2 = 1+alpha/a, -2*c, 1-alpha/a
	case LowShelf:
		beta := 2 * math.Sqrt(a) * alpha
		b0, b1, b2 = a*((a+1)-(a-1)*c+beta), 2*a*((a-1)-(a+1)*c), a*((a+1)-(a-1)*c-beta)
		a0, a1, a2 = (a+1)+(a-1)*c+beta, -2*((a-1)+(a+1)*c), (a+1)+(a-1)*c-beta
	case HighShelf:
		beta := 2 * math.Sqrt(a) * alpha
		b0, b1, b2 = a*((a+1)+(a-1)*c+beta), -2*a*((a-1)+(a+1)*c), a*((a+1)+(a-1)*c-beta)
		a0, a1, a2 = (a+1)-(a-1)*c+beta, 2*((a-1)-(a+1)*c), (a+1)-(a-1)*c-beta
	}
	b := biquad{b0: b0 / a0, b1: b1 / a0, b2: b2 / a0, a1: a1 / a0, a2: a2 / a0}
	if !(math.Abs(b.a2) < 1 && 1+b.a1+b.a2 > 0 && 1-b.a1+b.a2 > 0) {
		return biquad{}, errors.New("unstable biquad")
	}
	return b, nil
}

func (b *biquad) apply(v Frame) Frame {
	for ch, x := range v {
		y := b.b0*x + b.z1[ch]
		b.z1[ch], b.z2[ch] = b.b1*x-b.a1*y+b.z2[ch], b.b2*x-b.a2*y
		v[ch] = y
	}
	return v
}

// Response returns only the preamp and EQ response at the actual input rate.
func (c Config) Response(rate int, hz float64) (float64, error) {
	if err := c.Validate(rate); err != nil {
		return 0, err
	}
	if !between(hz, 0, float64(rate)/2) || rate <= 0 {
		return 0, errors.New("DSP: invalid response frequency")
	}
	if c.Bypass {
		return 0, nil
	}
	z := cmplx.Exp(complex(0, -2*math.Pi*hz/float64(rate)))
	db := c.PreampDB
	c.eachEQ(func(f Filter) {
		b, _ := coefficients(f, rate)
		h := (complex(b.b0, 0) + z*(complex(b.b1, 0)+z*complex(b.b2, 0))) /
			(1 + z*(complex(b.a1, 0)+z*complex(b.a2, 0)))
		db += 20 * math.Log10(cmplx.Abs(h))
	})
	return db, nil
}
