package output

import (
	"errors"

	"mygo-music/internal/audioengine/dsd"
)

var ErrDoPFormat = errors.New("DoP requires an explicitly selected exclusive endpoint and a 24-bit carrier at 176.4 or 352.8 kHz")

// frameDoP adds the same marker to both channels. The marker sequence follows
// submitted frames, including valid DSD idle frames at underrun and EOF.
func frameDoP(packet []byte, sampleBytes, content int, start uint64) {
	frames := len(packet) / (2 * sampleBytes)
	for i := range frames {
		tag := byte(0x05)
		if (start+uint64(i))&1 != 0 {
			tag = 0xfa
		}
		for ch := range 2 {
			end := (2*i + ch + 1) * sampleBytes
			if i >= content {
				clear(packet[end-sampleBytes : end])
				packet[end-3], packet[end-2] = dsd.Idle, dsd.Idle
			}
			packet[end-1] = tag
		}
	}
}
