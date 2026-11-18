package dsd

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"math/cmplx"
	"os"
	"path/filepath"
	"testing"

	"mygo-music/internal/audioengine/fixture"
)

func TestDSFPlanarBitOrderAndDeclaredTail(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "lsb.dsf")
	if err := fixture.WriteDSF(path, 2822400, 4097*8+3, func(i int) (byte, byte) {
		return 0x80 | byte(i%8), 0x20 | byte(i%16)
	}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	r, err := Open(ctx, bytes.NewReader(data), int64(len(data)))
	if err != nil || r.Info() != (Info{"dsf", 2822400, 32779}) {
		t.Fatal(r, err)
	}
	var out [8]byte
	n, err := r.ReadBytes(ctx, out[:], 4095)
	want := []byte{0x87, 0x2f, 0x80, 0x20, 0x89, 0x29}
	if n != 3 || err != io.EOF || !bytes.Equal(out[:6], want) {
		t.Fatalf("tail %x, pairs %d, error %v", out, n, err)
	}
	if _, err := Open(ctx, bytes.NewReader(data[:len(data)-1]), int64(len(data)-1)); !errors.Is(err, ErrContainer) {
		t.Fatal("truncated DSF", err)
	}
}

func TestDFFInterleavingAndDSTRejection(t *testing.T) {
	chunk := func(id string, payload []byte) []byte {
		b := make([]byte, 12+len(payload)+len(payload)%2)
		copy(b, id)
		binary.BigEndian.PutUint64(b[4:], uint64(len(payload)))
		copy(b[12:], payload)
		return b
	}
	ctx := context.Background()
	var rate [4]byte
	binary.BigEndian.PutUint32(rate[:], 5644800)
	props := append([]byte("SND "), chunk("FS  ", rate[:])...)
	props = append(props, chunk("CHNL", []byte("\x00\x02SLFTSRGT"))...)
	props = append(props, chunk("CMPR", []byte("DSD \x03raw"))...)
	form := append([]byte("DSD "), chunk("FVER", []byte{1, 5, 0, 0})...)
	form = append(form, chunk("PROP", props)...)
	payload := []byte{0x12, 0x56, 0x34, 0x78, 0x9a, 0xbc}
	image := chunk("FRM8", append(form, chunk("DSD ", payload)...))
	r, err := Open(ctx, bytes.NewReader(image), int64(len(image)))
	if err != nil || r.Info() != (Info{"dff", 5644800, 24}) {
		t.Fatal(r, err)
	}
	var out [6]byte
	if n, err := r.ReadBytes(ctx, out[:], 0); n != 3 || err != io.EOF || !bytes.Equal(out[:], payload) {
		t.Fatalf("data %x, pairs %d, error %v", out, n, err)
	}
	image = chunk("FRM8", append(form, chunk("DST ", []byte{1})...))
	if _, err := Open(ctx, bytes.NewReader(image), int64(len(image))); !errors.Is(err, ErrDST) {
		t.Fatal("DST was not rejected", err)
	}
}

func TestDecimatorFrequencyAndSeek(t *testing.T) {
	for _, rate := range []int{2822400, 5644800} {
		path := filepath.Join(t.TempDir(), "idle.dsf")
		const bits = 19017
		if err := fixture.WriteDSF(path, rate, bits, func(i int) (byte, byte) {
			return Idle ^ byte(i%2), Idle
		}); err != nil {
			t.Fatal(err)
		}
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { f.Close() })
		st, err := f.Stat()
		if err != nil {
			t.Fatal(err)
		}
		r, err := Open(t.Context(), f, st.Size())
		if err != nil {
			t.Fatal(err)
		}
		m := NewDecimator(r)
		for _, hz := range []float64{0, 20000, 44100, 52000} {
			var sum complex128
			for group, w := range m.weights {
				for bit := range 8 {
					weight := (w[0x80>>bit] - w[0]) / 2
					sum += complex(weight, 0) * cmplx.Rect(1, 2*math.Pi*hz*float64(8*group+bit)/float64(rate))
				}
			}
			db := 20 * math.Log10(cmplx.Abs(sum))
			t.Logf("DSD %d, %.0f Hz %.2f dB", rate, hz, db)
			if hz <= 20000 && math.Abs(db) > .1 || hz >= 44100 && db > -75 {
				t.Fatalf("DSD %d response at %g Hz is %g dB", rate, hz, db)
			}
		}
		var prefix [130]float64
		if n, err := m.Read(t.Context(), prefix[:], 0); n != 65 || err != nil {
			t.Fatal(n, err)
		}
		var seek [2]float64
		if n, err := m.Read(t.Context(), seek[:], 64); n != 1 || err != nil || seek != [2]float64{prefix[128], prefix[129]} {
			t.Fatal(n, err, seek)
		}
		want := int64((bits + rate/PCMRate - 1) / (rate / PCMRate))
		if n, err := m.Read(t.Context(), seek[:], want-1); m.Frames() != want || n != 1 || err != io.EOF {
			t.Fatal(m.Frames(), n, err)
		}
	}
}
