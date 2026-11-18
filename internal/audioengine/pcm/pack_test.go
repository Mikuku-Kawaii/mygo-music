package pcm

import (
	"bytes"
	"encoding/hex"
	"slices"
	"testing"
)

// Known wire bytes catch sign, endian and valid-bit alignment mistakes even
// if the packer and unpacker were to acquire the same bug.
func TestKnownPacking(t *testing.T) {
	for _, tc := range []struct {
		valid, container int
		wire             string
		src              []int32
	}{
		{16, 16, "0080ff7fffff00000100", []int32{-32768, 32767, -1, 0, 1}},
		{24, 24, "000080ffff7fffffff000000010000", []int32{-8388608, 8388607, -1, 0, 1}},
		{24, 32, "0000008000ffff7f00ffffff0000000000010000", []int32{-8388608, 8388607, -1, 0, 1}},
		{32, 32, "00000080ffffff7fffffffff0000000001000000", []int32{-2147483648, 2147483647, -1, 0, 1}},
	} {
		p, err := NewPacker(Format{Integer, 48000, 1, 4, tc.valid, tc.container})
		if err != nil {
			t.Fatal(err)
		}
		want, _ := hex.DecodeString(tc.wire)
		got := make([]byte, len(want))
		if err = p.Pack(got, tc.src); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("%d/%d: %x want %x", tc.valid, tc.container, got, want)
		}
		back := make([]int32, len(tc.src))
		if err = p.Unpack(back, want); err != nil || !slices.Equal(back, tc.src) {
			t.Fatalf("unpack %v: %v", back, err)
		}
	}
}

// Out-of-range samples cannot silently saturate or partially overwrite a packet.
func TestPackRejectsPrecisionLoss(t *testing.T) {
	p, _ := NewPacker(Format{Integer, 48000, 2, 3, 24, 32})
	b := bytes.Repeat([]byte{0xa5}, 8)
	if err := p.Pack(b, []int32{1, 8388608}); err != ErrRange || !bytes.Equal(b, bytes.Repeat([]byte{0xa5}, 8)) {
		t.Fatal("partial write or precision loss", err)
	}
	if err := p.Unpack(make([]int32, 2), []byte{1, 0, 0, 0, 0, 0, 0, 0}); err != ErrRange {
		t.Fatal("nonzero padding accepted")
	}
}

// The prepared hot path must not allocate as packet count increases.
func TestPackAllocations(t *testing.T) {
	p, _ := NewPacker(Format{Integer, 192000, 2, 3, 32, 32})
	src := make([]int32, 2048)
	dst := make([]byte, 8192)
	if n := testing.AllocsPerRun(100, func() {
		if err := p.Pack(dst, src); err != nil {
			panic(err)
		}
	}); n != 0 {
		t.Fatalf("%g allocations", n)
	}
}
func BenchmarkPack32(b *testing.B) {
	p, _ := NewPacker(Format{Integer, 192000, 2, 3, 32, 32})
	src := make([]int32, 4096)
	dst := make([]byte, 16384)
	b.ReportAllocs()
	b.SetBytes(int64(len(dst)))
	for b.Loop() {
		_ = p.Pack(dst, src)
	}
}
