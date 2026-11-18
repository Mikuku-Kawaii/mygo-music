package decode

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/mewkiz/flac"
	"github.com/mewkiz/flac/frame"
	"mygo-music/internal/audioengine/pcm"
)

type nativeFLAC struct {
	r       reader
	s       *flac.Stream
	info    Info
	frame   *frame.Frame
	at      int
	pos     int64
	scratch []int32
}

func openFLAC(ctx context.Context, f *os.File) (Decoder, error) {
	d := &nativeFLAC{r: reader{f, ctx}}
	if err := d.reopen(ctx); err != nil {
		return nil, err
	}
	i := d.s.Info
	// v1.0.14 reserves the 32-bit frame-header code and uses int32 stereo
	// residuals. Do not certify 32-bit FLAC using a float32 fallback.
	if i.BitsPerSample < 8 || i.BitsPerSample > 24 || i.NChannels < 1 || i.NChannels > 2 {
		return nil, ErrUnsupported
	}
	fmtPCM := pcm.Format{Kind: pcm.Integer, Rate: int(i.SampleRate), Channels: int(i.NChannels), ValidBits: int(i.BitsPerSample), ContainerBits: 32, ChannelMask: 3}
	if i.NChannels == 1 {
		fmtPCM.ChannelMask = 4
	}
	if err := fmtPCM.Validate(); err != nil {
		return nil, err
	}
	total := int64(i.NSamples)
	if total == 0 {
		total = -1
	}
	d.info = Info{Container: "FLAC", Format: fmtPCM, Frames: total,
		NativePrecision: true, ExactSeek: true, SeekMethod: "cancellable-linear-scan", Trimmed: true}
	d.scratch = make([]int32, 2048*fmtPCM.Channels)
	return d, nil
}
func (d *nativeFLAC) reopen(ctx context.Context) error {
	d.r.ctx = ctx
	if _, err := d.r.f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	s, err := flac.New(&d.r)
	if err != nil {
		return err
	}
	d.s = s
	d.frame = nil
	d.at = 0
	d.pos = 0
	return nil
}
func (d *nativeFLAC) Info() Info   { return d.info }
func (d *nativeFLAC) Close() error { return d.r.f.Close() }
func (d *nativeFLAC) Read(ctx context.Context, dst []int32) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	d.r.ctx = ctx
	ch := d.info.Format.Channels
	if len(dst) == 0 || len(dst)%ch != 0 {
		return 0, pcm.ErrBuffer
	}
	if d.frame == nil || d.at == int(d.frame.BlockSize) {
		f, err := d.s.Next()
		if err != nil {
			if err == io.EOF && d.info.Frames >= 0 && d.pos != d.info.Frames {
				err = io.ErrUnexpectedEOF
			}
			return 0, err
		}
		// Frame headers may inherit rate and precision from STREAMINFO.
		if f.BitsPerSample == 0 {
			f.BitsPerSample = d.s.Info.BitsPerSample
		}
		if f.SampleRate == 0 {
			f.SampleRate = d.s.Info.SampleRate
		}
		if f.BlockSize == 0 || f.BitsPerSample != uint8(d.info.Format.ValidBits) || f.SampleRate != uint32(d.info.Format.Rate) || f.Channels.Count() != ch {
			return 0, errors.New("flac: frame format changed")
		}
		if err = f.Parse(); err != nil {
			return 0, err
		}
		if d.info.Frames >= 0 && d.pos+int64(f.BlockSize) > d.info.Frames {
			return 0, errors.New("flac: frames exceed STREAMINFO length")
		}
		for _, s := range f.Subframes {
			if len(s.Samples) != int(f.BlockSize) {
				return 0, errors.New("flac: incomplete channel")
			}
		}
		d.frame = f
		d.at = 0
	}
	n := min(len(dst)/ch, int(d.frame.BlockSize)-d.at)
	for i := range n {
		for c := range ch {
			dst[i*ch+c] = d.frame.Subframes[c].Samples[d.at+i]
		}
	}
	d.at += n
	d.pos += int64(n)
	return n, nil
}
func (d *nativeFLAC) Seek(ctx context.Context, target int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if target < 0 || (d.info.Frames >= 0 && target > d.info.Frames) {
		return errors.New("flac: seek outside valid frames")
	}
	if target < d.pos {
		if err := d.reopen(ctx); err != nil {
			return err
		}
	}
	for d.pos < target {
		n := int(min(target-d.pos, int64(len(d.scratch)/d.info.Format.Channels)))
		_, err := d.Read(ctx, d.scratch[:n*d.info.Format.Channels])
		if err != nil {
			return fmt.Errorf("flac seek: %w", err)
		}
	}
	return nil
}
