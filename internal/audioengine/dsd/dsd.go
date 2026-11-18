// Package dsd reads uncompressed stereo DSD independently of PCM decoders.
package dsd

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"math/bits"
)

// Idle is a balanced DSD bit pattern used outside the effective source range.
const Idle = 0x69

var (
	ErrUnsupported = errors.New("DSD: expected stereo uncompressed DSD64 or DSD128")
	ErrDST         = errors.New("DSD: DST compression is unsupported")
	ErrContainer   = errors.New("DSD: invalid chunk or sample range")
)

type Info struct {
	Container string
	BitRate   int
	Bits      int64 // effective one-bit samples per channel
}

type Reader struct {
	info        Info
	file        io.ReaderAt
	data, bytes int64 // data offset and effective byte pairs
	group       int
	planar, lsb bool
	cached      int64
	buf         []byte
}

// Open validates the container before any PCM or carrier frame is produced.
// The caller owns the ReaderAt and closes it after the decode worker exits.
func Open(ctx context.Context, r io.ReaderAt, size int64) (*Reader, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var h [4]byte
	if size < 4 {
		return nil, ErrContainer
	}
	if _, err := r.ReadAt(h[:], 0); err != nil {
		return nil, err
	}
	d := &Reader{file: r, cached: -1}
	var err error
	switch string(h[:]) {
	case "DSD ":
		err = d.openDSF(size)
	case "FRM8":
		err = d.openDFF(ctx, size)
	default:
		err = ErrUnsupported
	}
	if err != nil {
		return nil, err
	}
	if d.info.BitRate != 2822400 && d.info.BitRate != 5644800 {
		return nil, ErrUnsupported
	}
	if d.info.Bits <= 0 {
		return nil, ErrContainer
	}
	d.buf = make([]byte, d.group*2)
	return d, nil
}

func (d *Reader) Info() Info { return d.info }

// ReadBytes reads chronological MSB-first DSD byte pairs [left, right].
// Partial final bytes contain idle bits after the declared sample count.
// The offset and result count are in byte pairs, regardless of disk layout.
func (d *Reader) ReadBytes(ctx context.Context, dst []byte, offset int64) (int, error) {
	if offset < 0 || len(dst)%2 != 0 {
		return 0, ErrContainer
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if len(dst) == 0 {
		return 0, nil
	}
	if offset >= d.bytes {
		return 0, io.EOF
	}
	count := int(min(int64(len(dst)/2), d.bytes-offset))
	for done := 0; done < count; {
		if err := ctx.Err(); err != nil {
			return done, err
		}
		group := int64(d.group)
		block, first := offset/group, int(offset%group)
		if d.cached != block {
			n := d.group
			if !d.planar {
				n = int(min(group, d.bytes-block*group))
			}
			if _, err := d.file.ReadAt(d.buf[:2*n], d.data+2*block*group); err != nil {
				if err == io.EOF {
					err = io.ErrUnexpectedEOF
				}
				return done, err
			}
			d.cached = block
		}
		n := min(count-done, d.group-first)
		out := dst[2*done : 2*(done+n)]
		if d.planar {
			for i := range n {
				l, r := d.buf[first+i], d.buf[d.group+first+i]
				if d.lsb {
					l, r = bits.Reverse8(l), bits.Reverse8(r)
				}
				out[2*i], out[2*i+1] = l, r
			}
		} else {
			copy(out, d.buf[2*first:2*(first+n)])
		}
		done += n
		offset += int64(n)
	}
	if offset == d.bytes {
		if tail := int(d.info.Bits % 8); tail != 0 {
			mask := byte(0xff << (8 - tail))
			for i := 2*count - 2; i < 2*count; i++ {
				dst[i] = dst[i]&mask | Idle&^mask
			}
		}
		return count, io.EOF
	}
	return count, nil
}

func (d *Reader) openDSF(size int64) error {
	const header = 92
	var h [header]byte
	if size < header {
		return ErrContainer
	}
	if _, err := d.file.ReadAt(h[:], 0); err != nil {
		return err
	}
	u32, u64 := binary.LittleEndian.Uint32, binary.LittleEndian.Uint64
	if u64(h[4:]) != 28 || u64(h[12:]) != uint64(size) ||
		string(h[28:32]) != "fmt " || u64(h[32:]) != 52 || string(h[80:84]) != "data" {
		return ErrContainer
	}
	order := u32(h[60:])
	if u32(h[40:]) != 1 || u32(h[44:]) != 0 || u32(h[48:]) != 2 || u32(h[52:]) != 2 ||
		(order != 1 && order != 8) || u32(h[72:]) != 4096 {
		return ErrUnsupported
	}
	block := int64(u32(h[72:]))
	chunk := u64(h[84:])
	if chunk < 12 || chunk-12 > uint64(size-header) {
		return ErrContainer
	}
	payload := int64(chunk - 12)
	tag := u64(h[20:])
	if tag != 0 && (tag < uint64(header+payload) || tag >= uint64(size)) {
		return ErrContainer
	}
	bits := u64(h[64:])
	if bits == 0 || bits > uint64(1<<63-8) {
		return ErrContainer
	}
	n := (int64(bits) + 7) / 8
	padded := (n + block - 1) / block * block
	if padded != payload/2 || payload%2 != 0 {
		return ErrContainer
	}
	d.info = Info{Container: "dsf", BitRate: int(u32(h[56:])), Bits: int64(bits)}
	d.data, d.bytes, d.group = header, n, int(block)
	d.planar, d.lsb = true, order == 1
	return nil
}

func (d *Reader) openDFF(ctx context.Context, size int64) error {
	kind, begin, end, _, err := d.chunk(0, size)
	if err != nil || kind != "FRM8" || end-begin < 4 {
		return ErrContainer
	}
	var four [4]byte
	if _, err := d.file.ReadAt(four[:], begin); err != nil || string(four[:]) != "DSD " {
		return ErrContainer
	}
	d.info.Container, d.group = "dff", 4096
	var version, props bool
	for off := begin + 4; off < end; {
		if err := ctx.Err(); err != nil {
			return err
		}
		id, start, stop, next, err := d.chunk(off, end)
		if err != nil {
			return err
		}
		switch id {
		case "FVER":
			if version || stop-start != 4 {
				return ErrContainer
			}
			if _, err := d.file.ReadAt(four[:], start); err != nil || four[0] != 1 {
				return ErrUnsupported
			}
			version = true
		case "PROP":
			if props {
				return ErrContainer
			}
			if err := d.soundProperties(ctx, start, stop); err != nil {
				return err
			}
			props = true
		case "DST ":
			return ErrDST
		case "DSD ":
			n := stop - start
			if d.data != 0 || n%2 != 0 || n <= 0 || n/2 > (1<<63-1)/8 {
				return ErrContainer
			}
			d.data, d.bytes = start, n/2
		}
		off = next
	}
	if !version || !props || d.data == 0 {
		return ErrContainer
	}
	d.info.Bits = d.bytes * 8
	return nil
}

func (d *Reader) soundProperties(ctx context.Context, begin, end int64) error {
	var b [10]byte
	if end-begin < 4 {
		return ErrContainer
	}
	if _, err := d.file.ReadAt(b[:4], begin); err != nil || string(b[:4]) != "SND " {
		return ErrUnsupported
	}
	var rate, channels, compression bool
	for off := begin + 4; off < end; {
		if err := ctx.Err(); err != nil {
			return err
		}
		id, start, stop, next, err := d.chunk(off, end)
		if err != nil {
			return err
		}
		switch id {
		case "FS  ":
			if rate || stop-start != 4 {
				return ErrContainer
			}
			if _, err := d.file.ReadAt(b[:4], start); err != nil {
				return err
			}
			d.info.BitRate, rate = int(binary.BigEndian.Uint32(b[:4])), true
		case "CHNL":
			if channels || stop-start != 10 {
				return ErrUnsupported
			}
			if _, err := d.file.ReadAt(b[:], start); err != nil {
				return err
			}
			if binary.BigEndian.Uint16(b[:2]) != 2 || (string(b[2:]) != "SLFTSRGT" && string(b[2:]) != "MLFTMRGT") {
				return ErrUnsupported
			}
			channels = true
		case "CMPR":
			if compression || stop-start < 5 {
				return ErrContainer
			}
			if _, err := d.file.ReadAt(b[:5], start); err != nil {
				return err
			}
			if int64(b[4]) > stop-start-5 {
				return ErrContainer
			}
			switch string(b[:4]) {
			case "DSD ":
				compression = true
			case "DST ":
				return ErrDST
			default:
				return ErrUnsupported
			}
		}
		off = next
	}
	if !rate || !channels || !compression {
		return ErrContainer
	}
	return nil
}

func (d *Reader) chunk(off, limit int64) (id string, start, end, next int64, err error) {
	if limit-off < 12 {
		return "", 0, 0, 0, ErrContainer
	}
	var h [12]byte
	if _, err := d.file.ReadAt(h[:], off); err != nil {
		return "", 0, 0, 0, err
	}
	start = off + 12
	n := binary.BigEndian.Uint64(h[4:])
	if n > uint64(limit-start) || n+(n&1) > uint64(limit-start) {
		return "", 0, 0, 0, ErrContainer
	}
	end = start + int64(n)
	return string(h[:4]), start, end, end + int64(n&1), nil
}
