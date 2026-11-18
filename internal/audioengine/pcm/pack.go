package pcm

import "errors"

// Packer is prepared off the render thread. Pack and Unpack do not allocate.
type Packer struct {
	format       Format
	width, shift int
	low, high    int64
}

func NewPacker(f Format) (*Packer, error) {
	if err := f.Validate(); err != nil {
		return nil, err
	}
	if f.Kind != Integer {
		return nil, errors.New("pcm: integer packer requires integer PCM")
	}
	limit := int64(1) << (f.ValidBits - 1)
	return &Packer{f, f.ContainerBits / 8, f.ContainerBits - f.ValidBits, -limit, limit - 1}, nil
}

// Pack validates the entire input before modifying dst; it never clips or
// dithers. dst must have exactly the corresponding frame capacity.
func (p *Packer) Pack(dst []byte, src []int32) error {
	if len(src)%p.format.Channels != 0 || len(dst) != len(src)*p.width {
		return ErrBuffer
	}
	for _, v := range src {
		if int64(v) < p.low || int64(v) > p.high {
			return ErrRange
		}
	}
	for i, v := range src {
		u := uint32(v) << p.shift
		for j := range p.width {
			dst[i*p.width+j] = byte(u >> (8 * j))
		}
	}
	return nil
}

// Unpack also rejects nonzero padding bits: a successful comparison proves
// both the effective samples and their lossless wire alignment.
func (p *Packer) Unpack(dst []int32, src []byte) error {
	if len(dst)%p.format.Channels != 0 || len(src) != len(dst)*p.width {
		return ErrBuffer
	}
	mask := uint32(1)<<p.shift - 1
	for i := range dst {
		var u uint32
		for j := range p.width {
			u |= uint32(src[i*p.width+j]) << (8 * j)
		}
		if u&mask != 0 {
			return ErrRange
		}
		// Sign extend the container first, then shift to right-aligned values.
		dst[i] = int32(u<<(32-p.format.ContainerBits)) >> (32 - p.format.ValidBits)
	}
	return nil
}
