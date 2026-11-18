package decode

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"os"

	"github.com/pion/opus"
	"github.com/pion/opus/pkg/oggreader"
	"mygo-music/internal/audioengine/pcm"
)

var errOpusContainer = errors.New("Opus: invalid or incomplete single-stream Ogg container")

// ProbeOpus checks the Ogg page structure and granule range without decoding
// audio. The decoder verifies each page checksum as its packets are consumed.
func ProbeOpus(ctx context.Context, file io.ReaderAt, size int64) (Info, error) {
	var h [27]byte
	if size < int64(len(h)) {
		return Info{}, ErrUnsupported
	}
	if _, err := file.ReadAt(h[:], 0); err != nil {
		return Info{}, err
	}
	if string(h[:4]) != "OggS" || h[26] == 0 {
		return Info{}, ErrUnsupported
	}
	var id [19]byte
	if _, err := file.ReadAt(id[:], 27+int64(h[26])); err != nil || string(id[:8]) != "OpusHead" {
		return Info{}, ErrUnsupported
	}
	if id[8] != 1 || (id[9] != 1 && id[9] != 2) || id[18] != 0 {
		return Info{}, errors.New("Opus: expected mono or stereo mapping family 0")
	}
	preSkip := int64(binary.LittleEndian.Uint16(id[10:12]))
	serial := binary.LittleEndian.Uint32(h[14:])
	total, err := opusGranuleRange(ctx, file, size, serial)
	if err != nil {
		return Info{}, err
	}
	if total < preSkip {
		return Info{}, errOpusContainer
	}
	return Info{
		Container: "Ogg Opus", Frames: total - preSkip, ExactSeek: true, Trimmed: true,
		SourceChannels: int(id[9]),
		SeekMethod:     "cancellable-linear-scan",
		Format:         pcm.Format{Kind: pcm.Float32, Rate: 48000, Channels: 2, ChannelMask: 3, ContainerBits: 32},
	}, nil
}

func opusGranuleRange(ctx context.Context, file io.ReaderAt, size int64, serial uint32) (int64, error) {
	var header [27]byte
	var lacing [255]byte
	var toc [2]byte
	var packets, bytesInPacket int
	var firstAudioSamples, origin, lastGranule int64
	var haveOrigin bool
	for off, seq := int64(0), uint32(0); off < size; seq++ {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		if _, err := file.ReadAt(header[:], off); err != nil || string(header[:4]) != "OggS" || header[4] != 0 {
			return 0, errOpusContainer
		}
		flags, n := header[5], int(header[26])
		if binary.LittleEndian.Uint32(header[14:]) != serial || binary.LittleEndian.Uint32(header[18:]) != seq ||
			(flags&2 != 0) != (seq == 0) || (flags&1 != 0) != (bytesInPacket != 0) || flags&^7 != 0 {
			return 0, errOpusContainer
		}
		if _, err := file.ReadAt(lacing[:n], off+27); err != nil {
			return 0, errOpusContainer
		}
		body := off + 27 + int64(n)
		completedAudio := false
		for _, count := range lacing[:n] {
			if int64(count) > size-body {
				return 0, errOpusContainer
			}
			if packets >= 2 && !haveOrigin && bytesInPacket == 0 && count > 0 {
				clear(toc[:])
				if _, err := file.ReadAt(toc[:min(int(count), 2)], body); err != nil {
					return 0, err
				}
			}
			body += int64(count)
			bytesInPacket += int(count)
			if bytesInPacket > 16<<20 || packets >= 2 && bytesInPacket > 64<<10 {
				return 0, errOpusContainer
			}
			if count == 255 {
				continue
			}
			if packets == 0 && (bytesInPacket != 19 || seq != 0 || n != 1) ||
				packets == 1 && bytesInPacket < 16 || packets >= 2 && bytesInPacket == 0 {
				return 0, errOpusContainer
			}
			if packets >= 2 {
				completedAudio = true
				if !haveOrigin {
					samples := opusPacketFrames(toc[0], toc[1], bytesInPacket)
					if samples == 0 {
						return 0, errOpusContainer
					}
					firstAudioSamples += int64(samples)
				}
			}
			bytesInPacket = 0
			packets++
		}
		granule := int64(binary.LittleEndian.Uint64(header[6:]))
		if completedAudio {
			if granule < lastGranule || (!haveOrigin && granule < firstAudioSamples && flags&4 == 0) {
				return 0, errOpusContainer
			}
			if !haveOrigin {
				origin, haveOrigin = max(0, granule-firstAudioSamples), true
			}
			lastGranule = granule
		}
		if flags&4 != 0 {
			if !haveOrigin || !completedAudio || bytesInPacket != 0 || body != size {
				return 0, errOpusContainer
			}
			return granule - origin, nil
		}
		off = body
	}
	return 0, errOpusContainer
}

func opusPacketFrames(toc, next byte, length int) int {
	config := toc >> 3
	var frames int
	switch {
	case config < 12:
		frames = [4]int{480, 960, 1920, 2880}[config%4]
	case config < 16:
		frames = 480 << (config % 2)
	default:
		frames = 120 << (config % 4)
	}
	switch toc & 3 {
	case 1, 2:
		frames *= 2
	case 3:
		if length < 2 {
			return 0
		}
		frames *= int(next & 63)
	}
	if frames > 5760 {
		return 0
	}
	return frames
}

type opusDecoder struct {
	file      *localReader
	info      Info
	packets   *oggreader.OggReader
	decoder   opus.Decoder
	decoded   [5760 * 2]float32
	available []float32
	position  int64
	skip      int
	gain      float32
}

func openOpus(ctx context.Context, f *os.File) (Decoder, error) {
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	p, err := ProbeOpus(ctx, f, info.Size())
	if err != nil {
		return nil, err
	}
	d := &opusDecoder{file: &localReader{File: f, ctx: ctx}, info: p}
	if err := d.rewind(ctx); err != nil {
		return nil, err
	}
	return d, nil
}

func (d *opusDecoder) rewind(ctx context.Context) error {
	d.file.ctx = ctx
	if _, err := d.file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	r, h, err := oggreader.NewWith(bufio.NewReaderSize(d.file, 64<<10))
	if err != nil {
		return err
	}
	comment, _, err := r.ParseNextPacket()
	if err != nil || len(comment) < 16 || string(comment[:8]) != "OpusTags" {
		return errOpusContainer
	}
	decoder, err := opus.NewDecoderWithOutput(48000, 2)
	if err != nil {
		return err
	}
	d.packets, d.decoder = r, decoder
	d.position, d.skip, d.available = 0, int(h.PreSkip), nil
	d.gain = float32(math.Pow(10, float64(int16(h.OutputGain))/(256*20)))
	return nil
}

func (d *opusDecoder) Info() Info   { return d.info }
func (d *opusDecoder) Close() error { return d.file.Close() }

func (d *opusDecoder) Read(ctx context.Context, dst []int32) (int, error) {
	if len(dst)%2 != 0 {
		return 0, pcm.ErrBuffer
	}
	d.file.ctx = ctx
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	want := int(min(int64(len(dst)/2), d.info.Frames-d.position))
	for done := 0; done < want; {
		if err := ctx.Err(); err != nil {
			return done, err
		}
		if len(d.available) == 0 {
			packet, _, err := d.packets.ParseNextPacket()
			if err == io.EOF {
				return done, io.ErrUnexpectedEOF
			}
			if err != nil {
				return done, err
			}
			n, err := d.decoder.DecodeToFloat32(packet, d.decoded[:])
			if err != nil {
				return done, err
			}
			discard := min(d.skip, n)
			d.skip -= discard
			d.available = d.decoded[2*discard : 2*n]
		}
		n := min(want-done, len(d.available)/2)
		for i, v := range d.available[:2*n] {
			dst[2*done+i] = int32(math.Float32bits(v * d.gain))
		}
		d.available = d.available[2*n:]
		d.position += int64(n)
		done += n
	}
	if d.position == d.info.Frames {
		return want, io.EOF
	}
	return want, nil
}

func (d *opusDecoder) Seek(ctx context.Context, frame int64) error {
	if frame < 0 || frame > d.info.Frames {
		return errors.New("Opus: seek beyond decoded range")
	}
	if frame < d.position {
		if err := d.rewind(ctx); err != nil {
			return err
		}
	}
	var scratch [2048]int32
	for d.position < frame {
		n := min(int64(len(scratch)/2), frame-d.position)
		if _, err := d.Read(ctx, scratch[:2*n]); err != nil && !(err == io.EOF && d.position == frame) {
			return err
		}
	}
	return ctx.Err()
}
