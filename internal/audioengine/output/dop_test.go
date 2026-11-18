package output

import (
	"bytes"
	"path/filepath"
	"testing"

	"mygo-music/internal/audioengine/decode"
	"mygo-music/internal/audioengine/fixture"
	"mygo-music/internal/audioengine/pcm"
)

func TestDoPCarrierPayloadAndIdle(t *testing.T) {
	for _, rate := range []int{2822400, 5644800} {
		path := filepath.Join(t.TempDir(), "carrier.dsf")
		left, right := []byte{0x12, 0x34, 0x99}, []byte{0x56, 0x78, 0xe1}
		if err := fixture.WriteDSF(path, rate, 19, func(i int) (byte, byte) { return left[i], right[i] }); err != nil {
			t.Fatal(err)
		}
		d, err := decode.OpenDoP(t.Context(), path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { d.Close() })
		info := d.Info()
		if info.Format.Rate != rate/16 || info.Frames != 2 || info.DSDBits != 19 {
			t.Fatal(info)
		}
		var words [4]int32
		if n, _ := d.Read(t.Context(), words[:]); n != 2 || words != [4]int32{0x1234, 0x5678, 0x8969, 0xe969} {
			t.Fatal(n, words)
		}
		for _, width := range []int{3, 4} {
			f := info.Format
			f.ContainerBits = 8 * width
			b := make([]byte, 4*2*width)
			pack, err := pcm.NewPacker(f)
			if err != nil {
				t.Fatal(err)
			}
			if err := pack.Pack(b[:4*width], words[:]); err != nil {
				t.Fatal(err)
			}
			frameDoP(b[:6*width], width, 2, 0)
			frameDoP(b[6*width:], width, 0, 3)
			want := []byte{0x34, 0x12, 5, 0x78, 0x56, 5, 0x69, 0x89, 0xfa, 0x69, 0xe9, 0xfa,
				0x69, 0x69, 5, 0x69, 0x69, 5, 0x69, 0x69, 0xfa, 0x69, 0x69, 0xfa}
			for i := range len(want) / 3 {
				if width == 4 && b[i*width] != 0 || !bytes.Equal(b[(i+1)*width-3:(i+1)*width], want[3*i:3*i+3]) {
					t.Fatalf("%d-byte carrier: got %x", width, b)
				}
			}
		}
		t.Logf("%d Hz carrier: payload, 24/32-bit containers and continuous idle markers", info.Format.Rate)
	}
}
