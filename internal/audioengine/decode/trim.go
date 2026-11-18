package decode

import (
	"encoding/binary"
	"io"
	"math"
)

// mp3Trim recognizes MPEG-1 Layer III Xing/Info with encoder padding.
// go-mp3 v0.3.4 decodes the metadata frame and retains the 529-frame synthesis
// delay. The decoded frame count must agree with the Xing count before trim.
func mp3Trim(r io.ReaderAt, raw int64) (skip, length int64, ok bool) {
	be := binary.BigEndian
	var tag [10]byte
	if _, err := r.ReadAt(tag[:], 0); err != nil {
		return 0, 0, false
	}
	var off int64
	if string(tag[:3]) == "ID3" {
		for _, c := range tag[6:] {
			if c >= 128 {
				return 0, 0, false
			}
			off = (off << 7) | int64(c)
		}
		off += 10
		if tag[3] == 4 && tag[5]&0x10 != 0 {
			off += 10
		}
	}
	var head [4]byte
	if _, err := r.ReadAt(head[:], off); err != nil {
		return 0, 0, false
	}
	h := be.Uint32(head[:])
	if h&0xfffe0000 != 0xfffa0000 {
		return 0, 0, false
	}
	bitrates := [...]int{0, 32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, 0}
	rates := [...]int{44100, 48000, 32000, 0}
	kbps, hz := bitrates[(h>>12)&15], rates[(h>>10)&3]
	if kbps == 0 || hz == 0 {
		return 0, 0, false
	}
	size := 144000*kbps/hz + int((h>>9)&1)
	at := 36
	if (h>>6)&3 == 3 {
		at = 21
	}
	if h&(1<<16) == 0 {
		at += 2
	}
	if at+8 > size {
		return 0, 0, false
	}
	var b [1444]byte
	if _, err := r.ReadAt(b[:size], off); err != nil {
		return 0, 0, false
	}
	switch string(b[at : at+4]) {
	case "Xing", "Info":
	default:
		return 0, 0, false
	}
	flags := be.Uint32(b[at+4:])
	i := at + 8
	if flags&1 == 0 || flags&^15 != 0 {
		return 0, 0, false
	}
	if i+4 > size {
		return 0, 0, false
	}
	count := int64(be.Uint32(b[i:]))
	i += 4
	if flags&2 != 0 {
		i += 4
	}
	if flags&4 != 0 {
		i += 100
	}
	if flags&8 != 0 {
		i += 4
	}
	if i+24 > size || count == 0 || (count+1)*1152 != raw {
		return 0, 0, false
	}
	switch string(b[i : i+4]) {
	case "LAME", "Lavf", "Lavc":
	default:
		return 0, 0, false
	}
	delay := int64(b[i+21])<<4 | int64(b[i+22]>>4)
	padding := int64(b[i+22]&15)<<8 | int64(b[i+23])
	const synthesisDelay = 529
	skip, length = 1152+delay+synthesisDelay, count*1152-delay-padding
	return skip, length, delay > 0 && padding >= synthesisDelay && length > 0 && skip+length <= raw
}

type boxRange struct{ start, end int64 }

func mp4Boxes(r io.ReaderAt, bounds boxRange, visit func(string, boxRange) bool) bool {
	var h [16]byte
	for off := bounds.start; bounds.end-off >= 8; {
		if _, err := r.ReadAt(h[:8], off); err != nil {
			return false
		}
		header, size := int64(8), uint64(binary.BigEndian.Uint32(h[:]))
		switch size {
		case 0:
			size = uint64(bounds.end - off)
		case 1:
			if bounds.end-off < 16 {
				return false
			}
			if _, err := r.ReadAt(h[8:16], off+8); err != nil {
				return false
			}
			header, size = 16, binary.BigEndian.Uint64(h[8:])
		}
		if size < uint64(header) || size > uint64(bounds.end-off) {
			return false
		}
		next := off + int64(size)
		if visit(string(h[4:8]), boxRange{off + header, next}) {
			return true
		}
		off = next
	}
	return false
}

func findBox(r io.ReaderAt, parent boxRange, name string) (box boxRange, found bool) {
	found = mp4Boxes(r, parent, func(kind string, candidate boxRange) bool {
		if kind != name {
			return false
		}
		box = candidate
		return true
	})
	return box, found
}

// mp4Trim checks the first audio track's single rate-1 edit. MyGo already
// drops its start, so only shorten the post-priming frame count here. An
// exact sample-table length wins within a single tick of the movie clock.
func mp4Trim(r io.ReaderAt, size, frames int64, rate int, lossless bool) (int64, bool) {
	if frames <= 0 {
		return frames, false
	}
	moov, ok := findBox(r, boxRange{0, size}, "moov")
	if !ok {
		return frames, false
	}
	var sound boxRange
	found := mp4Boxes(r, moov, func(kind string, b boxRange) bool {
		if kind != "trak" {
			return false
		}
		mdia, _ := findBox(r, b, "mdia")
		hdlr, _ := findBox(r, mdia, "hdlr")
		var h [4]byte
		if hdlr.end-hdlr.start < 12 {
			return false
		}
		if _, err := r.ReadAt(h[:], hdlr.start+8); err != nil || string(h[:]) != "soun" {
			return false
		}
		sound = b
		return true
	})
	if !found {
		return frames, false
	}
	edts, ok := findBox(r, sound, "edts")
	if !ok {
		return frames, lossless
	}
	elst, ok := findBox(r, edts, "elst")
	if !ok || elst.end-elst.start < 20 {
		return frames, false
	}
	mvhd, ok := findBox(r, moov, "mvhd")
	if !ok || mvhd.end-mvhd.start < 24 {
		return frames, false
	}
	var movie [24]byte
	if _, err := r.ReadAt(movie[:], mvhd.start); err != nil {
		return frames, false
	}
	clockAt := 12
	switch movie[0] {
	case 0:
	case 1:
		clockAt = 20
	default:
		return frames, false
	}
	clock := int64(binary.BigEndian.Uint32(movie[clockAt:]))
	if clock == 0 || rate <= 0 {
		return frames, false
	}
	var edit [28]byte
	n := min(int64(len(edit)), elst.end-elst.start)
	if _, err := r.ReadAt(edit[:n], elst.start); err != nil || binary.BigEndian.Uint32(edit[4:]) != 1 {
		return frames, false
	}
	var movieLength uint64
	var start int64
	var speed uint32
	switch edit[0] {
	case 0:
		movieLength, start, speed = uint64(binary.BigEndian.Uint32(edit[8:])), int64(int32(binary.BigEndian.Uint32(edit[12:]))), binary.BigEndian.Uint32(edit[16:])
	case 1:
		if n < 28 {
			return frames, false
		}
		movieLength, start, speed = binary.BigEndian.Uint64(edit[8:]), int64(binary.BigEndian.Uint64(edit[16:])), binary.BigEndian.Uint32(edit[24:])
	default:
		return frames, false
	}
	if start < 0 || speed != 0x10000 || movieLength == 0 || movieLength > uint64(math.MaxInt64/int64(rate)) {
		return frames, false
	}
	length := int64(movieLength) * int64(rate) / clock
	tick := (int64(rate) + clock - 1) / clock
	if length <= 0 || length-frames > tick {
		return frames, false
	}
	if frames-length <= tick {
		return frames, true
	}
	// A coarse movie tick cannot identify an exact earlier source frame.
	if (int64(movieLength)*int64(rate))%clock != 0 || clock < int64(rate) {
		return frames, false
	}
	return length, true
}
