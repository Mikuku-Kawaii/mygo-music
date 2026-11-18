package fixture

import (
	"encoding/binary"
	"math/bits"
)

// WriteDSF writes stereo DSF test data. sample returns a chronological
// MSB-first byte for each channel before the file's planar/LSB conversion.
func WriteDSF(path string, rate, bitCount int, sample func(int) (byte, byte)) error {
	const header, block = 92, 4096
	pairs := (bitCount + 7) / 8
	payload := (pairs + block - 1) / block * 2 * block
	b := make([]byte, header+payload)
	u32, u64 := binary.LittleEndian.PutUint32, binary.LittleEndian.PutUint64
	copy(b, "DSD ")
	u64(b[4:], 28)
	u64(b[12:], uint64(len(b)))
	copy(b[28:], "fmt ")
	u64(b[32:], 52)
	u32(b[40:], 1)
	u32(b[48:], 2)
	u32(b[52:], 2)
	u32(b[56:], uint32(rate))
	u32(b[60:], 1)
	u64(b[64:], uint64(bitCount))
	u32(b[72:], block)
	copy(b[80:], "data")
	u64(b[84:], uint64(payload+12))
	for i := range pairs {
		l, r := sample(i)
		at := header + 2*block*(i/block) + i%block
		b[at], b[at+block] = bits.Reverse8(l), bits.Reverse8(r)
	}
	return writeNew(path, b)
}
