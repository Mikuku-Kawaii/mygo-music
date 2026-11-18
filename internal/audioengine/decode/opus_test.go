package decode

import (
	"encoding/binary"
	"io"
	"math"
	"os"
	"slices"
	"testing"

	"mygo-music/internal/audioengine/pcm"
)

func TestOpusPacketDurations(t *testing.T) {
	for _, c := range []struct {
		toc, next byte
		size, n   int
	}{
		{0, 0, 4, 480}, {3<<3 | 1, 0, 4, 5760}, {13<<3 | 2, 0, 4, 1920},
		{16<<3 | 3, 48, 200, 5760}, {16<<3 | 3, 49, 200, 0}, {16<<3 | 3, 1, 1, 0},
		{31 << 3, 0, 200, 960},
	} {
		if n := opusPacketFrames(c.toc, c.next, c.size); n != c.n {
			t.Fatalf("TOC %x, count %d: got %d, want %d", c.toc, c.next, n, c.n)
		}
	}
}

// Use the same Ogg Opus file and an independent decoder's raw stereo f32le
// output from the beginning of the stream, after its preskip and end trim.
func TestOpusReference(t *testing.T) {
	path, ref := os.Getenv("PRISM_OPUS_FIXTURE"), os.Getenv("PRISM_OPUS_REFERENCE")
	if path == "" || ref == "" {
		t.Skip("opt-in reference file comparison")
	}
	b, err := os.ReadFile(ref)
	if err != nil {
		t.Fatal(err)
	}
	d, err := OpenProcessed(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	i := d.Info()
	if i.Container != "Ogg Opus" || !i.ExactSeek || !i.Trimmed || i.Format.Rate != 48000 || i.Format.Kind != pcm.Float32 || i.Frames*8 != int64(len(b)) {
		t.Fatalf("granule trim disagrees with reference: %+v, reference %d frames", i, len(b)/8)
	}
	all := make([]int32, 2*i.Frames)
	for off := 0; off < len(all); {
		n, err := d.Read(t.Context(), all[off:min(off+1950, len(all))])
		off += 2 * n
		if n == 0 || (err != nil && !(err == io.EOF && off == len(all))) {
			t.Fatal(off, n, err)
		}
	}
	if n, err := d.Read(t.Context(), make([]int32, 16)); n != 0 || err != io.EOF {
		t.Fatal(n, err)
	}
	const anchor = 1991
	if err := d.Seek(t.Context(), anchor); err != nil {
		t.Fatal(err)
	}
	suffix := make([]int32, 2000)
	if n, err := d.Read(t.Context(), suffix); err != nil || n != len(suffix)/2 || !slices.Equal(suffix, all[2*anchor:2*anchor+len(suffix)]) {
		t.Fatal("backward seek does not reproduce the linear decode", n, err)
	}
	var gotEnergy, wantEnergy, dot float64
	for j, sample := range all {
		got := float64(math.Float32frombits(uint32(sample)))
		want := float64(math.Float32frombits(binary.LittleEndian.Uint32(b[4*j:])))
		gotEnergy += got * got
		wantEnergy += want * want
		dot += got * want
	}
	corr := dot / math.Sqrt(gotEnergy*wantEnergy)
	level := 10 * math.Log10(gotEnergy/wantEnergy)
	t.Logf("%d trimmed frames, correlation %.8f, level difference %.4f dB, exact rewind/seek", i.Frames, corr, level)
	if !(corr >= .995 && math.Abs(level) <= .15) {
		t.Fatal("decoded programme differs from the reference")
	}
}
