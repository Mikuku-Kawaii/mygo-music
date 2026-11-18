// Package pcm defines native PCM independently of MyGo's float32 mixer.
package pcm

import (
	"errors"
	"math/bits"
)

type Kind string

const (
	Integer Kind = "signed-integer"
	Float32 Kind = "float32"
	Float64 Kind = "float64"
)

// Format describes interleaved PCM. Integer decoder samples are signed,
// right-aligned int32 values; wire samples are little-endian, left-aligned
// within ContainerBits. ChannelMask follows WAVEFORMATEXTENSIBLE ordering.
type Format struct {
	Kind          Kind
	Rate          int
	Channels      int
	ChannelMask   uint32
	ValidBits     int
	ContainerBits int
}

func (f Format) Validate() error {
	if f.Rate < 1 || f.Rate > 768000 || f.Channels < 1 || f.Channels > 8 ||
		bits.OnesCount32(f.ChannelMask) != f.Channels {
		return errors.New("pcm: invalid rate or channel layout")
	}
	switch f.Kind {
	case Integer:
		if (f.ContainerBits != 16 && f.ContainerBits != 24 && f.ContainerBits != 32) || f.ValidBits < 1 || f.ValidBits > f.ContainerBits {
			return errors.New("pcm: unsupported integer precision/container")
		}
	case Float32:
		if f.ContainerBits != 32 || f.ValidBits != 0 {
			return errors.New("pcm: invalid float32 format")
		}
	case Float64:
		if f.ContainerBits != 64 || f.ValidBits != 0 {
			return errors.New("pcm: invalid float64 format")
		}
	default:
		return errors.New("pcm: unknown sample kind")
	}
	return nil
}

func (f Format) FrameBytes() int { return f.Channels * (f.ContainerBits / 8) }

// NativeEqual permits lossless container repacking, but no rate, channel or
// effective precision changes. It does not by itself certify a device path.
func (f Format) NativeEqual(g Format) bool {
	return f.Kind == g.Kind && f.Rate == g.Rate && f.Channels == g.Channels && f.ChannelMask == g.ChannelMask && f.ValidBits == g.ValidBits
}

type Generation struct {
	Session uint64
	Seek    uint64
	Queue   uint64
	Format  uint64
	Config  uint64
}

// Block metadata travels with the samples, not with a mutable player object.
// A published block is read-only until its consumer explicitly releases it.
type Block struct {
	Generation    Generation
	SourceToken   uint64 // source identity within this session; never a file URL
	SourceFrame   int64
	Frames        int
	EOS           bool
	Discontinuity bool
	Samples       []int32
}

var (
	ErrWouldBlock = errors.New("pcm: data temporarily unavailable")
	ErrRange      = errors.New("pcm: sample outside effective precision")
	ErrBuffer     = errors.New("pcm: buffer is not a whole number of frames")
)
