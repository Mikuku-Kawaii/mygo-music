// Package fixture generates deterministic offline-only native PCM vectors.
// Full-scale edge/LSB vectors are NOT listening material.
package fixture

import (
	"crypto/sha256"
	_ "embed"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/mewkiz/flac"
	"github.com/mewkiz/flac/frame"
	"github.com/mewkiz/flac/meta"
	"mygo-music/internal/audioengine/pcm"
)

// Unicode file names are fixture data, not untranslated application labels.
//
//go:embed filenames.json
var filenameData []byte

type Entry struct {
	File         string
	Format       pcm.Format
	Frames       int
	StartFrame   int
	Pattern      string
	SampleSHA256 string
}
type Manifest struct {
	Version int
	Warning string
	Entries []Entry
}

// Samples uses integer arithmetic only, including the full int32 extrema and
// LSBs. Each channel has a different sequence. start supports split continuity.
func Samples(validBits, start, frames int) []int32 {
	b := make([]int32, frames*2)
	limit := int64(1) << (validBits - 1)
	edges := []int64{-limit, limit - 1, -1, 0, 1, -limit + 1, limit - 2}
	for i := range frames {
		for c := range 2 {
			k := (start+i)*2 + c
			if k < len(edges) {
				b[i*2+c] = int32(edges[k])
				continue
			}
			v := (uint64(k)*2654435761 + uint64(c)*2246822519) & uint64(2*limit-1)
			b[i*2+c] = int32(int64(v) - limit)
		}
	}
	return b
}

// Wave is a continuous periodic integer triangle, independently split at any
// frame. Its amplitude is 1/128 full scale; edge fixtures remain offline-only.
func Wave(validBits, start, frames int) []int32 {
	b := make([]int32, frames*2)
	for i := range frames {
		for c := range 2 {
			phase := (start + i + c*37) % 256
			v := phase
			if phase >= 128 {
				v = 256 - phase
			}
			b[i*2+c] = int32(int64(v-64) * (int64(1) << (validBits - 1)) / 8192)
		}
	}
	return b
}

func Reference(e Entry) []int32 {
	if e.Pattern == "triangle-v1" {
		return Wave(e.Format.ValidBits, e.StartFrame, e.Frames)
	}
	return Samples(e.Format.ValidBits, e.StartFrame, e.Frames)
}
func Hash(samples []int32) string {
	h := sha256.New()
	var b [4]byte
	for _, v := range samples {
		binary.LittleEndian.PutUint32(b[:], uint32(v))
		h.Write(b[:])
	}
	return hex.EncodeToString(h.Sum(nil))
}
func WriteWAV(path string, f pcm.Format, samples []int32) error {
	p, err := pcm.NewPacker(f)
	if err != nil {
		return err
	}
	data := make([]byte, len(samples)*f.ContainerBits/8)
	if err = p.Pack(data, samples); err != nil {
		return err
	}
	b := make([]byte, 68+len(data)+(len(data)&1))
	copy(b, "RIFF")
	binary.LittleEndian.PutUint32(b[4:], uint32(len(b)-8))
	copy(b[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(b[16:], 40)
	binary.LittleEndian.PutUint16(b[20:], 0xfffe)
	binary.LittleEndian.PutUint16(b[22:], uint16(f.Channels))
	binary.LittleEndian.PutUint32(b[24:], uint32(f.Rate))
	binary.LittleEndian.PutUint32(b[28:], uint32(f.Rate*f.FrameBytes()))
	binary.LittleEndian.PutUint16(b[32:], uint16(f.FrameBytes()))
	binary.LittleEndian.PutUint16(b[34:], uint16(f.ContainerBits))
	binary.LittleEndian.PutUint16(b[36:], 22)
	binary.LittleEndian.PutUint16(b[38:], uint16(f.ValidBits))
	binary.LittleEndian.PutUint32(b[40:], f.ChannelMask)
	copy(b[44:], []byte{1, 0, 0, 0, 0, 0, 0x10, 0, 0x80, 0, 0, 0xaa, 0, 0x38, 0x9b, 0x71})
	copy(b[60:], "data")
	binary.LittleEndian.PutUint32(b[64:], uint32(len(data)))
	copy(b[68:], data)
	return writeNew(path, b)
}
func writeNew(path string, b []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	return joinClose(err, f)
}
func joinClose(err error, f io.Closer) error {
	e := f.Close()
	if err != nil {
		return err
	}
	return e
}
func WriteFLAC(path string, f pcm.Format, samples []int32) error {
	if f.Kind != pcm.Integer || f.Channels != 2 || (f.ValidBits != 16 && f.ValidBits != 24) {
		return fmt.Errorf("fixture: unsupported FLAC format")
	}
	w, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	enc, err := flac.NewEncoder(w, &meta.StreamInfo{SampleRate: uint32(f.Rate), NChannels: 2, BitsPerSample: uint8(f.ValidBits), BlockSizeMin: 16, BlockSizeMax: 1024})
	if err != nil {
		w.Close()
		return err
	}
	enc.EnablePredictionAnalysis(false)
	for at := 0; at < len(samples)/2; {
		n := min(1024, len(samples)/2-at)
		// The encoder writes the shortest block into STREAMINFO, whose
		// minimum is 16; leave a legal final block instead of an 8-frame tail.
		if left := len(samples)/2 - at - n; left > 0 && left < 16 {
			n -= 16 - left
		}
		f := &frame.Frame{Header: frame.Header{SampleRate: uint32(f.Rate), BitsPerSample: uint8(f.ValidBits), BlockSize: uint16(n), Channels: frame.ChannelsLR}}
		for c := range 2 {
			s := &frame.Subframe{SubHeader: frame.SubHeader{Pred: frame.PredVerbatim}, NSamples: n, Samples: make([]int32, n)}
			for i := range n {
				s.Samples[i] = samples[(at+i)*2+c]
			}
			f.Subframes = append(f.Subframes, s)
		}
		if err = enc.WriteFrame(f); err != nil {
			enc.Close()
			return err
		}
		at += n
	}
	return enc.Close()
}
func Generate(dir string) (Manifest, error) {
	m := Manifest{Version: 1, Warning: "Full-scale offline precision vectors; do not audition. SHA-256 covers interleaved right-aligned signed int32 LE samples."}
	var names struct{ WAV, FLAC, Wave string }
	if err := json.Unmarshal(filenameData, &names); err != nil {
		return m, err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return m, err
	}
	for _, rate := range []int{44100, 48000, 96000, 192000} {
		for _, pair := range [][2]int{{16, 16}, {24, 24}, {24, 32}, {32, 32}} {
			f := pcm.Format{Kind: pcm.Integer, Rate: rate, Channels: 2, ChannelMask: 3, ValidBits: pair[0], ContainerBits: pair[1]}
			for part := range 2 {
				start, n := 0, 2053
				if part == 1 {
					start, n = 2053, 4099
				}
				name := fmt.Sprintf(names.WAV, rate, pair[0], pair[1], part+1)
				s := Samples(pair[0], start, n)
				if err := WriteWAV(filepath.Join(dir, name), f, s); err != nil {
					return m, err
				}
				m.Entries = append(m.Entries, Entry{name, f, n, start, "integer-sequence-v1", Hash(s)})
			}
			if pair[0] != 32 && pair[0] == pair[1] {
				s := Samples(pair[0], 0, 6152)
				name := fmt.Sprintf(names.FLAC, rate, pair[0])
				if err := WriteFLAC(filepath.Join(dir, name), f, s); err != nil {
					return m, err
				}
				m.Entries = append(m.Entries, Entry{name, f, 6152, 0, "integer-sequence-v1", Hash(s)})
			}
		}
	}
	f := pcm.Format{Kind: pcm.Integer, Rate: 48000, Channels: 2, ChannelMask: 3, ValidBits: 24, ContainerBits: 24}
	for part := range 2 {
		start, n := 0, 2053
		if part == 1 {
			start, n = 2053, 4099
		}
		s := Wave(24, start, n)
		for _, ext := range []string{"wav", "flac"} {
			name := fmt.Sprintf(names.Wave, part+1, ext)
			var err error
			if ext == "wav" {
				err = WriteWAV(filepath.Join(dir, name), f, s)
			} else {
				err = WriteFLAC(filepath.Join(dir, name), f, s)
			}
			if err != nil {
				return m, err
			}
			m.Entries = append(m.Entries, Entry{name, f, n, start, "triangle-v1", Hash(s)})
		}
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return m, err
	}
	return m, writeNew(filepath.Join(dir, "manifest.json"), b)
}
