package decode

import (
	"context"
	"errors"
	"io"
	"math"
	"os"

	"github.com/egoist/mygo/audio"
	_ "github.com/egoist/mygo/audio/aac"
	_ "github.com/egoist/mygo/audio/mp3"
	"github.com/egoist/mygo/audio/mp4"
	_ "github.com/egoist/mygo/audio/vorbis"
	"mygo-music/internal/audioengine/pcm"
)

// OpenProcessed retains the native WAV/FLAC decoders. Compressed decoders
// with a float32 API enter the DSP path at their actual decoder precision.
func OpenProcessed(ctx context.Context, path string) (Decoder, error) {
	d, err := Open(ctx, path)
	if !errors.Is(err, ErrUnsupported) {
		return d, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	var magic [4]byte
	if _, err := f.ReadAt(magic[:], 0); err == nil {
		switch string(magic[:]) {
		case "DSD ", "FRM8":
			next, err := openDSD(ctx, f, false)
			if err != nil {
				f.Close()
			}
			return next, err
		case "OggS":
			next, err := openOpus(ctx, f)
			if !errors.Is(err, ErrUnsupported) {
				if err != nil {
					f.Close()
				}
				return next, err
			}
		}
	}
	r := &localReader{File: f, ctx: ctx}
	a, format, err := audio.NewDecoder(r)
	if err != nil {
		f.Close()
		return nil, err
	}
	if a.Channels() != 2 {
		f.Close()
		return nil, errors.New("DSP: this decoder requires a stereo source")
	}
	info := Info{
		Container: format, Frames: a.Len(),
		Format:    pcm.Format{Kind: pcm.Float32, Rate: a.SampleRate(), Channels: 2, ChannelMask: 3, ContainerBits: 32},
		ExactSeek: true, SeekMethod: "cancellable-linear-scan",
	}
	var first int64
	switch format {
	case "vorbis":
		// oggvorbis respects the granule end position.
		info.Trimmed = info.Frames >= 0
	case "mp4":
		st, statErr := f.Stat()
		if statErr != nil {
			f.Close()
			return nil, statErr
		}
		t, trackErr := mp4.ReadTrack(r, st.Size())
		if trackErr != nil {
			f.Close()
			return nil, trackErr
		}
		info.Container = t.Codec + " / MP4"
		// MyGo applies the supported priming edit and packet-table duration.
		// A single rate-1 edit can establish the effective range. Files
		// with ambiguous edit lists keep the decoder's ordinary duration.
		if !t.Fragmented {
			info.Frames, info.Trimmed = mp4Trim(r, st.Size(), info.Frames, info.Format.Rate, t.Codec == "alac" || t.Codec == "fLaC")
		}
	case "mp3":
		if start, length, ok := mp3Trim(r, info.Frames); ok {
			first, info.Frames, info.Trimmed = start, length, true
		}
	case "aac":
		// ADTS has no reliable encoder padding metadata.
	default:
		f.Close()
		return nil, ErrUnsupported
	}
	if err := info.Format.Validate(); err != nil {
		f.Close()
		return nil, err
	}
	return &floatDecoder{local: r, audio: a, info: info, first: first, skip: first, scratch: make([]float32, 4096)}, nil
}

type localReader struct {
	*os.File
	ctx context.Context
}

func (r *localReader) Read(b []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.File.Read(b)
}

func (r *localReader) ReadAt(b []byte, off int64) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.File.ReadAt(b, off)
}

type floatDecoder struct {
	local                 *localReader
	audio                 audio.Decoder
	info                  Info
	scratch               []float32
	first, skip, position int64
}

func (d *floatDecoder) Info() Info   { return d.info }
func (d *floatDecoder) Close() error { return d.local.Close() }

func (d *floatDecoder) Read(ctx context.Context, dst []int32) (int, error) {
	if len(dst)%2 != 0 {
		return 0, pcm.ErrBuffer
	}
	d.local.ctx = ctx
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if err := d.discard(); err != nil {
		return 0, err
	}
	count := int64(min(len(dst), len(d.scratch)) / 2)
	if count == 0 {
		return 0, nil
	}
	if d.info.Trimmed {
		count = min(count, d.info.Frames-d.position)
		if count == 0 {
			return 0, io.EOF
		}
	}
	n, err := d.read(d.scratch[:2*count])
	d.position += int64(n)
	if d.info.Trimmed && err == io.EOF && d.position < d.info.Frames {
		err = io.ErrUnexpectedEOF
	}
	for i, x := range d.scratch[:2*n] {
		dst[i] = int32(math.Float32bits(x))
	}
	return n, err
}

func (d *floatDecoder) Seek(ctx context.Context, frame int64) error {
	// Replay the decoder's preroll and discard to the exact effective frame.
	d.local.ctx = ctx
	if frame < 0 || d.info.Frames >= 0 && frame > d.info.Frames {
		return errors.New("decode: seek beyond decoded range")
	}
	if frame < d.position {
		if _, err := d.local.Seek(0, io.SeekStart); err != nil {
			return err
		}
		next, _, err := audio.NewDecoder(d.local)
		if err != nil {
			return err
		}
		d.audio, d.position, d.skip = next, 0, d.first
	}
	d.skip += frame - d.position
	if err := d.discard(); err != nil {
		return err
	}
	d.position = frame
	return ctx.Err()
}

func (d *floatDecoder) discard() error {
	for d.skip > 0 {
		if err := d.local.ctx.Err(); err != nil {
			return err
		}
		n, err := d.read(d.scratch[:2*min(int64(len(d.scratch)/2), d.skip)])
		d.skip -= int64(n)
		if d.skip == 0 {
			return nil
		}
		if err == io.EOF {
			return io.ErrUnexpectedEOF
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrNoProgress
		}
	}
	return nil
}

func (d *floatDecoder) read(dst []float32) (int, error) {
	n, err := d.audio.Read(dst)
	// The pinned MyGo codec implementations return interleaved samples.
	if n < 0 || n > len(dst) || n%2 != 0 {
		return 0, pcm.ErrBuffer
	}
	return n / 2, err
}
