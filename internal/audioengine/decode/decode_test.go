package decode_test

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"mygo-music/internal/audioengine/decode"
	"mygo-music/internal/audioengine/fixture"
	"mygo-music/internal/audioengine/pcm"
)

// Every declared native rate/precision preserves edge samples, channel order,
// valid frame count and exact forward/backward seeks across decoder blocks.
func TestNativeMatrix(t *testing.T) {
	dir := t.TempDir()
	m, err := fixture.Generate(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range m.Entries {
		t.Run(e.File, func(t *testing.T) {
			d, err := decode.Open(context.Background(), filepath.Join(dir, e.File))
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			if d.Info().Frames != int64(e.Frames) || !d.Info().Format.NativeEqual(e.Format) {
				t.Fatal(d.Info())
			}
			var got []int32
			buf := make([]int32, 514)
			for {
				n, err := d.Read(context.Background(), buf)
				got = append(got, buf[:n*2]...)
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			want := fixture.Reference(e)
			if !slices.Equal(got, want) || fixture.Hash(got) != e.SampleSHA256 {
				t.Fatal("native samples differ")
			}
			for _, at := range []int{1023, 0, e.Frames - 1, e.Frames} {
				if err := d.Seek(context.Background(), int64(at)); err != nil {
					t.Fatal(err)
				}
				n, err := d.Read(context.Background(), buf)
				if at == e.Frames {
					if n != 0 || err != io.EOF {
						t.Fatal("end seek", n, err)
					}
					continue
				}
				if err != nil || !slices.Equal(buf[:n*2], want[at*2:(at+n)*2]) {
					t.Fatal("seek samples differ", at, err)
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if _, err := d.Read(ctx, buf); !errors.Is(err, context.Canceled) {
				t.Fatal("canceled read", err)
			}
			if err := d.Seek(ctx, 0); !errors.Is(err, context.Canceled) {
				t.Fatal("canceled seek", err)
			}
		})
	}
}

// Unsupported floating WAV and 32-bit FLAC must fail explicitly; truncated
// or CRC-damaged FLAC must not turn into a clean EOF.
func TestRejectedAndBrokenFormats(t *testing.T) {
	dir := t.TempDir()
	f := pcm.Format{Kind: pcm.Integer, Rate: 48000, Channels: 2, ChannelMask: 3, ValidBits: 24, ContainerBits: 24}
	wavPath := filepath.Join(dir, "float.wav")
	if err := fixture.WriteWAV(wavPath, f, fixture.Samples(24, 0, 40)); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(wavPath)
	if err != nil {
		t.Fatal(err)
	}
	binary.LittleEndian.PutUint32(b[44:], 3)
	if err = os.WriteFile(wavPath, b, 0600); err != nil {
		t.Fatal(err)
	}
	if d, err := decode.Open(context.Background(), wavPath); !errors.Is(err, decode.ErrUnsupported) {
		if d != nil {
			d.Close()
		}
		t.Fatal("float WAV accepted", err)
	}
	flacPath := filepath.Join(dir, "source.flac")
	if err = fixture.WriteFLAC(flacPath, f, fixture.Samples(24, 0, 100)); err != nil {
		t.Fatal(err)
	}
	b, err = os.ReadFile(flacPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, variant := range []string{"crc", "truncated", "32-bit"} {
		t.Run(variant, func(t *testing.T) {
			data := append([]byte(nil), b...)
			switch variant {
			case "crc":
				data[len(data)-1] ^= 1
			case "truncated":
				data = data[:len(data)-1]
			case "32-bit":
				v := binary.BigEndian.Uint64(data[18:26])
				v = v&^(uint64(31)<<36) | (uint64(31) << 36)
				binary.BigEndian.PutUint64(data[18:26], v)
			}
			path := filepath.Join(dir, variant+".flac")
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			d, err := decode.Open(context.Background(), path)
			if variant == "32-bit" {
				if !errors.Is(err, decode.ErrUnsupported) {
					if d != nil {
						d.Close()
					}
					t.Fatal("32-bit FLAC accepted", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			if _, err = d.Read(context.Background(), make([]int32, 200)); err == nil || err == io.EOF {
				t.Fatal("damaged data appeared valid", err)
			}
		})
	}
}

// Truncated audio must fail, not be mistaken for a clean end of a short song.
func TestTruncatedWAV(t *testing.T) {
	p := filepath.Join(t.TempDir(), "broken.wav")
	f := pcm.Format{Kind: pcm.Integer, Rate: 48000, Channels: 2, ChannelMask: 3, ValidBits: 32, ContainerBits: 32}
	if err := fixture.WriteWAV(p, f, fixture.Samples(32, 0, 80)); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(p, 100); err != nil {
		t.Fatal(err)
	}
	if d, err := decode.Open(context.Background(), p); err == nil {
		d.Close()
		t.Fatal("truncated WAV accepted")
	}
}
