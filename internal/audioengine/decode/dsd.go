package decode

import (
	"context"
	"errors"
	"io"
	"os"

	"mygo-music/internal/audioengine/dsd"
	"mygo-music/internal/audioengine/pcm"
)

// OpenDoP exposes the DSD payload as 16-bit pairs in a 24-bit carrier slot.
// The device thread inserts the markers, including around idle frames.
func OpenDoP(ctx context.Context, path string) (Decoder, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	d, err := openDSD(ctx, f, true)
	if err != nil {
		f.Close()
	}
	return d, err
}

func openDSD(ctx context.Context, file *os.File, carrier bool) (Decoder, error) {
	st, err := file.Stat()
	if err != nil {
		return nil, err
	}
	source, err := dsd.Open(ctx, file, st.Size())
	if err != nil {
		return nil, err
	}
	raw := source.Info()
	base := dsdDecoder{file: file, source: source, info: Info{
		Container: raw.Container, DSDRate: raw.BitRate, DSDBits: raw.Bits,
		Trimmed: true, ExactSeek: true, SeekMethod: "dsd-block-offset",
		Format: pcm.Format{Channels: 2, ChannelMask: 3},
	}}
	if carrier {
		base.info.Format.Kind, base.info.Format.Rate = pcm.Integer, raw.BitRate/16
		base.info.Format.ValidBits, base.info.Format.ContainerBits = 24, 24
		base.info.Frames = 1 + (raw.Bits-1)/16
		return &carrierDecoder{dsdDecoder: base}, nil
	}
	filter := dsd.NewDecimator(source)
	base.info.Format.Kind, base.info.Format.Rate, base.info.Format.ContainerBits = pcm.Float64, dsd.PCMRate, 64
	base.info.Frames, base.info.DecodeLookahead = filter.Frames(), dsd.LookaheadFrames
	return &dsdPCMDecoder{dsdDecoder: base, filter: filter}, nil
}

type dsdDecoder struct {
	file     *os.File
	source   *dsd.Reader
	info     Info
	position int64
}

func (d *dsdDecoder) Info() Info   { return d.info }
func (d *dsdDecoder) Close() error { return d.file.Close() }
func (d *dsdDecoder) Seek(ctx context.Context, position int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if position < 0 || position > d.info.Frames {
		return errors.New("DSD: seek beyond effective range")
	}
	d.position = position
	return nil
}

type carrierDecoder struct {
	dsdDecoder
	scratch [8192]byte
}

func (d *carrierDecoder) Read(ctx context.Context, dst []int32) (int, error) {
	if len(dst)%2 != 0 {
		return 0, pcm.ErrBuffer
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if d.position == d.info.Frames {
		return 0, io.EOF
	}
	count := int(min(int64(min(len(dst)/2, len(d.scratch)/4)), d.info.Frames-d.position))
	n, err := d.source.ReadBytes(ctx, d.scratch[:count*4], d.position*2)
	if err != nil && err != io.EOF {
		return 0, err
	}
	if n%2 != 0 {
		d.scratch[2*n], d.scratch[2*n+1] = dsd.Idle, dsd.Idle
	}
	frames := (n + 1) / 2
	for i := range frames {
		b := d.scratch[4*i : 4*i+4]
		dst[2*i], dst[2*i+1] = int32(b[0])<<8|int32(b[2]), int32(b[1])<<8|int32(b[3])
	}
	d.position += int64(frames)
	if d.position == d.info.Frames {
		return frames, io.EOF
	}
	return frames, nil
}

type dsdPCMDecoder struct {
	dsdDecoder
	filter *dsd.Decimator
}

func (d *dsdPCMDecoder) Read(context.Context, []int32) (int, error) {
	return 0, pcm.ErrBuffer
}

func (d *dsdPCMDecoder) ReadFloat64(ctx context.Context, dst []float64) (int, error) {
	n, err := d.filter.Read(ctx, dst, d.position)
	d.position += int64(n)
	return n, err
}
