//go:build windows && amd64

package output

import (
	"bytes"
	"encoding/binary"
	"math"
	"testing"

	"mygo-music/internal/audioengine/pcm"
)

func TestASIOPlanarConversion(t *testing.T) {
	in := pcm.Format{Kind: pcm.Integer, ValidBits: 24, ContainerBits: 32}
	src := []byte{0, 0x00, 0x00, 0x80, 0, 0xff, 0xff, 0x7f, 0, 0x01, 0x00, 0x00, 0, 0xff, 0xff, 0xff}
	var left, right [16]byte
	dst := [2][]byte{left[:], right[:]}
	out := pcm.Format{Kind: pcm.Integer, ValidBits: 24, ContainerBits: 24}
	convertASIO(src, dst, in, out)
	if !bytes.Equal(left[:6], []byte{0, 0, 0x80, 1, 0, 0}) || !bytes.Equal(right[:6], []byte{0xff, 0xff, 0x7f, 0xff, 0xff, 0xff}) {
		t.Fatalf("24-bit ASIO buffers: %x, %x", left, right)
	}
	out = pcm.Format{Kind: pcm.Float32, ContainerBits: 32}
	convertASIO(src, dst, in, out)
	read := func(b []byte) float32 { return math.Float32frombits(binary.LittleEndian.Uint32(b)) }
	if read(left[:]) != -1 || read(right[:]) != 1-1.0/(1<<23) || read(left[4:]) != 1.0/(1<<23) || read(right[4:]) != -1.0/(1<<23) {
		t.Fatalf("float32 ASIO buffers: %x, %x", left, right)
	}
	if got := testing.AllocsPerRun(100, func() { convertASIO(src, dst, in, out) }); got != 0 {
		t.Fatalf("ASIO deinterleave allocates %g times", got)
	}
}
