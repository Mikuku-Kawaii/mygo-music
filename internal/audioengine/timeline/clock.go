// Package timeline maps device clock units to submitted output frames.
package timeline

import (
	"errors"
	"math/bits"
)

// Clock must be rebuilt after Reset/reopen. It measures presentation at the
// driver-reported endpoint, not conversion inside the DAC.
type Clock struct {
	Epoch, AnchorPosition, AnchorFrame, Frequency uint64
	Rate                                          uint32
}

func NewClock(epoch, position, frame, frequency uint64, rate uint32) (Clock, error) {
	if frequency == 0 || rate == 0 {
		return Clock{}, errors.New("timeline: zero frequency or rate")
	}
	return Clock{epoch, position, frame, frequency, rate}, nil
}
func (c Clock) Presented(epoch, position, submitted uint64) (uint64, error) {
	if epoch != c.Epoch || position < c.AnchorPosition {
		return 0, errors.New("timeline: stale epoch or regressed device clock")
	}
	hi, lo := bits.Mul64(position-c.AnchorPosition, uint64(c.Rate))
	if hi >= c.Frequency {
		return 0, errors.New("timeline: clock overflow")
	}
	frames, _ := bits.Div64(hi, lo, c.Frequency)
	if frames > ^uint64(0)-c.AnchorFrame {
		return 0, errors.New("timeline: frame overflow")
	}
	return min(c.AnchorFrame+frames, submitted), nil
}
