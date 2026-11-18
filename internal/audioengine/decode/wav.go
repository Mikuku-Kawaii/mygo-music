package decode

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"os"

	"mygo-music/internal/audioengine/pcm"
)

type wav struct {
	f          *os.File
	info       Info
	start, pos int64
	buf        []byte
	pack       *pcm.Packer
}

// M0 deliberately accepts finite RIFF integer PCM, including extensible
// valid-bit containers. RF64, floating-point and multichannel playback are
// separate capabilities, never silently narrowed to stereo/integer PCM.
func openWAV(ctx context.Context, f *os.File) (Decoder, error) {
	stat, err := f.Stat()
	if err != nil {
		return nil, err
	}
	var head [12]byte
	if _, err = f.ReadAt(head[:], 0); err != nil {
		return nil, err
	}
	end := int64(binary.LittleEndian.Uint32(head[4:])) + 8
	if end < 12 || end > stat.Size() {
		return nil, io.ErrUnexpectedEOF
	}
	var format pcm.Format
	haveFormat := false
	for off := int64(12); off+8 <= end; {
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		var h [8]byte
		if _, err = f.ReadAt(h[:], off); err != nil {
			return nil, err
		}
		n := int64(binary.LittleEndian.Uint32(h[4:]))
		off += 8
		if n > end-off {
			return nil, io.ErrUnexpectedEOF
		}
		switch string(h[:4]) {
		case "fmt ":
			if haveFormat || n < 16 {
				return nil, errors.New("wav: invalid fmt chunk")
			}
			var b [40]byte
			if _, err = f.ReadAt(b[:min(n, 40)], off); err != nil {
				return nil, err
			}
			tag := binary.LittleEndian.Uint16(b[:])
			format = pcm.Format{Kind: pcm.Integer, Channels: int(binary.LittleEndian.Uint16(b[2:])), Rate: int(binary.LittleEndian.Uint32(b[4:])), ContainerBits: int(binary.LittleEndian.Uint16(b[14:]))}
			format.ValidBits = format.ContainerBits
			switch format.Channels {
			case 1:
				format.ChannelMask = 4
			case 2:
				format.ChannelMask = 3
			}
			if tag == 0xfffe {
				pcmGUID := []byte{1, 0, 0, 0, 0, 0, 0x10, 0, 0x80, 0, 0, 0xaa, 0, 0x38, 0x9b, 0x71}
				if n < 40 || binary.LittleEndian.Uint16(b[16:]) < 22 || int64(binary.LittleEndian.Uint16(b[16:]))+18 > n || !bytes.Equal(b[24:40], pcmGUID) {
					return nil, ErrUnsupported
				}
				format.ValidBits = int(binary.LittleEndian.Uint16(b[18:]))
				format.ChannelMask = binary.LittleEndian.Uint32(b[20:])
			} else if tag != 1 {
				return nil, ErrUnsupported
			}
			if err = format.Validate(); err != nil {
				return nil, err
			}
			if format.Channels > 2 || (format.Channels == 2 && format.ChannelMask != 3) || (format.Channels == 1 && format.ChannelMask != 4) {
				return nil, ErrUnsupported
			}
			if int(binary.LittleEndian.Uint16(b[12:])) != format.FrameBytes() || int(binary.LittleEndian.Uint32(b[8:])) != format.Rate*format.FrameBytes() {
				return nil, errors.New("wav: inconsistent block alignment/byte rate")
			}
			haveFormat = true
		case "data":
			if !haveFormat || n%int64(format.FrameBytes()) != 0 {
				return nil, errors.New("wav: missing format or partial frame")
			}
			p, err := pcm.NewPacker(format)
			if err != nil {
				return nil, err
			}
			return &wav{f: f, info: Info{Container: "WAV", Format: format, Frames: n / int64(format.FrameBytes()),
				NativePrecision: true, ExactSeek: true, SeekMethod: "direct-frame-offset", Trimmed: true},
				start: off, buf: make([]byte, 2048*format.FrameBytes()), pack: p}, nil
		}
		off += n + (n & 1)
	}
	return nil, errors.New("wav: missing data chunk")
}

func (d *wav) Info() Info   { return d.info }
func (d *wav) Close() error { return d.f.Close() }
func (d *wav) Seek(ctx context.Context, frame int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if frame < 0 || frame > d.info.Frames {
		return errors.New("wav: seek outside valid frames")
	}
	d.pos = frame
	return nil
}
func (d *wav) Read(ctx context.Context, dst []int32) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	ch := d.info.Format.Channels
	if len(dst) == 0 || len(dst)%ch != 0 {
		return 0, pcm.ErrBuffer
	}
	if d.pos == d.info.Frames {
		return 0, io.EOF
	}
	n := int(min(int64(len(dst)/ch), d.info.Frames-d.pos, 2048))
	b := d.buf[:n*d.info.Format.FrameBytes()]
	if _, err := d.f.ReadAt(b, d.start+d.pos*int64(d.info.Format.FrameBytes())); err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return 0, err
	}
	if err := d.pack.Unpack(dst[:n*ch], b); err != nil {
		return 0, err
	}
	d.pos += int64(n)
	return n, nil
}
