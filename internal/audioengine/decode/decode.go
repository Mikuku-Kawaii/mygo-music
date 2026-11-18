// Package decode performs cancellable local I/O on decoder workers only.
package decode

import (
	"context"
	"errors"
	"io"
	"os"

	"mygo-music/internal/audioengine/pcm"
)

type Info struct {
	Container       string
	Format          pcm.Format
	Frames          int64 // -1 means unknown, never zero as a substitute for unknown
	NativePrecision bool
	ExactSeek       bool
	SeekMethod      string
	Trimmed         bool
	SourceChannels  int   // set when decoded channel layout can differ from the source
	DSDRate         int   // bit rate per channel before conversion or framing
	DSDBits         int64 // effective source bits per channel
	DecodeLookahead int   // compensated lookahead at Format.Rate
}

// Decoder belongs to one worker. Read returns frames, not interleaved samples.
// For a Float32 format the same storage carries IEEE 754 sample bit patterns.
// A non-nil error may accompany frames; only io.EOF is a clean end. Cancellation
// is checked before reads and frame work; local kernel I/O is not preemptible.
type Decoder interface {
	Info() Info
	Read(context.Context, []int32) (int, error)
	Seek(context.Context, int64) error
	Close() error
}

// Float64Decoder delivers float64 directly to the processed pipeline.
type Float64Decoder interface {
	Decoder
	ReadFloat64(context.Context, []float64) (int, error)
}

var ErrUnsupported = errors.New("decode: unsupported native format")

func Open(ctx context.Context, path string) (Decoder, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	var magic [12]byte
	n, err := f.ReadAt(magic[:], 0)
	if err != nil && err != io.EOF {
		f.Close()
		return nil, err
	}
	var d Decoder
	switch {
	case n >= 12 && string(magic[:4]) == "RIFF" && string(magic[8:]) == "WAVE":
		d, err = openWAV(ctx, f)
	case n >= 4 && string(magic[:4]) == "fLaC":
		d, err = openFLAC(ctx, f)
	default:
		err = ErrUnsupported
	}
	if err != nil {
		f.Close()
	}
	return d, err
}

// reader makes cancellation observable even while scanning FLAC metadata or
// seeking through frames. It is only used by its owning decoder worker.
type reader struct {
	f   *os.File
	ctx context.Context
}

func (r *reader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.f.Read(p)
}
