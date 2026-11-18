package decode

import (
	"context"
	"io"
	"path/filepath"
	"slices"
	"testing"

	"mygo-music/internal/audioengine/pcm"
)

// Compressed adapters deliver effective frames and seek to an identical
// decoded suffix, including MP3 and MP4 priming compensation.
func TestProcessedCodecEffectiveFramesAndSeek(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		path    string
		frames  int
		trimmed bool
	}{
		{"seek.mp3", 220500, true},
		{"seek.ogg", 220544, true},
		{"seek.m4a", 88200, true},
		{"seek-aac.m4a", 132300, true},
		{"seek.aac", 0, false},
		{"seek-aac-frag.m4a", 0, false},
	}
	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			path := filepath.Join("..", "..", "player", "testdata", c.path)
			d, err := OpenProcessed(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			info := d.Info()
			if info.Format.Kind != pcm.Float32 || info.NativePrecision || !info.ExactSeek || info.Trimmed != c.trimmed {
				t.Fatal(info)
			}
			block := make([]int32, 1534)
			full := make([]int32, 0, 220500*2)
			for {
				n, err := d.Read(ctx, block)
				full = append(full, block[:n*2]...)
				if err == io.EOF {
					break
				}
				if err != nil || n == 0 {
					t.Fatal(n, err)
				}
			}
			if c.frames != 0 && (len(full) != c.frames*2 || info.Frames != int64(c.frames)) {
				t.Fatal(len(full)/2, info)
			}
			if len(full) < 2*18000 {
				t.Fatal(len(full))
			}
			const target = 17111
			if err := d.Seek(ctx, target); err != nil {
				t.Fatal(err)
			}
			n, err := d.Read(ctx, block)
			if n == 0 || err != nil || !slices.Equal(block[:n*2], full[target*2:(target+n)*2]) {
				t.Fatal("decoded seek suffix differs", n, err)
			}
			t.Logf("%s frames=%d effective=%t", info.Container, len(full)/2, info.Trimmed)
		})
	}
}
