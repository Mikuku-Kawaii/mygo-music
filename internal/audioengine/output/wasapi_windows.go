//go:build windows && amd64

package output

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
	"mygo-music/internal/audioengine/pcm"
)

// COM method indexes and ABI follow audioclient.h/mmdeviceapi.h. This backend
// is independent of MyGo's shared-mode mixer; no AUTOCONVERTPCM flag is used.
var (
	iidStreamVolume = windows.GUID{Data1: 0x93014887, Data2: 0x242d, Data3: 0x4068, Data4: [8]byte{0x8a, 0x15, 0xcf, 0x5e, 0x93, 0xb9, 0x0f, 0xe3}}
	clsidEnumerator = windows.GUID{Data1: 0xbcde0395, Data2: 0xe52f, Data3: 0x467c, Data4: [8]byte{0x8e, 0x3d, 0xc4, 0x57, 0x92, 0x91, 0x69, 0x2e}}
	iidEnumerator   = windows.GUID{Data1: 0xa95664d2, Data2: 0x9614, Data3: 0x4f35, Data4: [8]byte{0xa7, 0x46, 0xde, 0x8d, 0xb6, 0x36, 0x17, 0xe6}}
	iidClient       = windows.GUID{Data1: 0x1cb9ad4c, Data2: 0xdbfa, Data3: 0x4c32, Data4: [8]byte{0xb1, 0x78, 0xc2, 0xf5, 0x68, 0xa7, 0x03, 0xb2}}
	iidRender       = windows.GUID{Data1: 0xf294acfc, Data2: 0x3146, Data3: 0x4483, Data4: [8]byte{0xa7, 0xbf, 0xad, 0xdc, 0xa7, 0xc2, 0x60, 0xe2}}
	iidClock        = windows.GUID{Data1: 0xcd63314f, Data2: 0x3fba, Data3: 0x4a1b, Data4: [8]byte{0x81, 0x2c, 0xef, 0x96, 0x35, 0x87, 0x28, 0xe7}}
	subPCM          = windows.GUID{Data1: 1, Data3: 0x10, Data4: [8]byte{0x80, 0, 0, 0xaa, 0, 0x38, 0x9b, 0x71}}
	ole             = windows.NewLazySystemDLL("ole32.dll")
	createCOM       = ole.NewProc("CoCreateInstance")
	clearVariant    = ole.NewProc("PropVariantClear")
	avrt            = windows.NewLazySystemDLL("avrt.dll")
	setMMCSS        = avrt.NewProc("AvSetMmThreadCharacteristicsW")
	revertMMCSS     = avrt.NewProc("AvRevertMmThreadCharacteristics")
)

type waveFormat struct {
	Tag, Channels            uint16
	Rate, ByteRate           uint32
	Align, Bits, Size, Valid uint16
	Mask                     uint32
	Sub                      windows.GUID
}

func wireFormat(f pcm.Format) waveFormat {
	return waveFormat{0xfffe, uint16(f.Channels), uint32(f.Rate), uint32(f.Rate * f.FrameBytes()), uint16(f.FrameBytes()), uint16(f.ContainerBits), 22, uint16(f.ValidBits), f.ChannelMask, subPCM}
}

//go:uintptrescapes
func call(obj uintptr, index int, args ...uintptr) uintptr {
	vt := *(*uintptr)(pointer(obj))
	fn := *(*uintptr)(unsafe.Add(pointer(vt), index*8))
	var a [12]uintptr
	a[0] = obj
	n := copy(a[1:], args)
	hr, _, _ := syscall.SyscallN(fn, a[:n+1]...)
	return hr
}

// Native COM pointers are not Go heap pointers. This conversion also makes
// that distinction explicit to checkptr; Go pointers are never stored by COM.
func pointer(v uintptr) unsafe.Pointer { return *(*unsafe.Pointer)(unsafe.Pointer(&v)) }
func failed(hr uintptr) bool           { return int32(uint32(hr)) < 0 }
func check(op string, hr uintptr) error {
	if failed(hr) {
		return &DeviceError{Operation: op, Code: uint32(hr)}
	}
	return nil
}
func releaseCOM(p uintptr) {
	if p != 0 {
		call(p, 2)
	}
}
func enumerator() (uintptr, error) {
	var p uintptr
	hr, _, _ := createCOM.Call(uintptr(unsafe.Pointer(&clsidEnumerator)), 0, 23, uintptr(unsafe.Pointer(&iidEnumerator)), uintptr(unsafe.Pointer(&p)))
	return p, check("CoCreateInstance", hr)
}
func endpoint(e uintptr, id string) (uintptr, error) {
	var d uintptr
	var hr uintptr
	if id == "" {
		hr = call(e, 4, 0, 0, uintptr(unsafe.Pointer(&d)))
	} else {
		u, err := windows.UTF16PtrFromString(id)
		if err != nil {
			return 0, err
		}
		hr = call(e, 5, uintptr(unsafe.Pointer(u)), uintptr(unsafe.Pointer(&d)))
	}
	return d, check("GetDevice", hr)
}
func deviceID(d uintptr) (string, error) {
	var p *uint16
	if err := check("GetId", call(d, 5, uintptr(unsafe.Pointer(&p)))); err != nil {
		return "", err
	}
	defer windows.CoTaskMemFree(unsafe.Pointer(p))
	return windows.UTF16PtrToString(p), nil
}
func activate(d uintptr) (uintptr, error) {
	var c uintptr
	err := check("Activate", call(d, 3, uintptr(unsafe.Pointer(&iidClient)), 23, 0, uintptr(unsafe.Pointer(&c))))
	return c, err
}
func deviceName(d uintptr) string {
	var store uintptr
	if failed(call(d, 4, 0, uintptr(unsafe.Pointer(&store)))) {
		return ""
	}
	defer releaseCOM(store)
	key := struct {
		ID  windows.GUID
		PID uint32
	}{windows.GUID{Data1: 0xa45c254e, Data2: 0xdf1c, Data3: 0x4efd, Data4: [8]byte{0x80, 0x20, 0x67, 0xd1, 0x46, 0xa8, 0x50, 0xe0}}, 14}
	v := struct {
		VT       uint16
		Reserved [3]uint16
		Value    uintptr
		Extra    uintptr
	}{}
	if failed(call(store, 5, uintptr(unsafe.Pointer(&key)), uintptr(unsafe.Pointer(&v)))) {
		return ""
	}
	defer clearVariant.Call(uintptr(unsafe.Pointer(&v)))
	if v.VT != 31 {
		return ""
	}
	return windows.UTF16PtrToString((*uint16)(pointer(v.Value)))
}

// Probe lists only active render endpoints. Results are support queries for
// the exact supplied formats, NOT proof that an exclusive stream can open.
func Probe(ctx context.Context, formats []pcm.Format) ([]Device, error) {
	for _, f := range formats {
		if err := f.Validate(); err != nil {
			return nil, err
		}
		if f.Kind != pcm.Integer {
			return nil, ErrUnsupported
		}
	}
	type result struct {
		ds  []Device
		err error
	}
	done := make(chan result, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		if err := windows.CoInitializeEx(0, 0); err != nil {
			done <- result{err: err}
			return
		}
		defer windows.CoUninitialize()
		e, err := enumerator()
		if err != nil {
			done <- result{err: err}
			return
		}
		defer releaseCOM(e)
		defaultID := ""
		if d, err := endpoint(e, ""); err == nil {
			defaultID, _ = deviceID(d)
			releaseCOM(d)
		}
		var collection uintptr
		if err = check("EnumAudioEndpoints", call(e, 3, 0, 1, uintptr(unsafe.Pointer(&collection)))); err != nil {
			done <- result{err: err}
			return
		}
		defer releaseCOM(collection)
		var count uint32
		if err = check("GetCount", call(collection, 3, uintptr(unsafe.Pointer(&count)))); err != nil {
			done <- result{err: err}
			return
		}
		var devices []Device
		for i := uint32(0); i < count; i++ {
			if err = ctx.Err(); err != nil {
				done <- result{err: err}
				return
			}
			var d uintptr
			if err = check("Item", call(collection, 4, uintptr(i), uintptr(unsafe.Pointer(&d)))); err != nil {
				done <- result{err: err}
				return
			}
			item := probeDevice(d, defaultID, formats)
			releaseCOM(d)
			devices = append(devices, item)
		}
		done <- result{ds: devices}
	}()
	r := <-done
	return r.ds, r.err
}
func probeDevice(d uintptr, defaultID string, formats []pcm.Format) Device {
	x := Device{Name: deviceName(d)}
	var err error
	x.ID, err = deviceID(d)
	if err != nil {
		x.Error = err.Error()
		return x
	}
	x.Default = x.ID == defaultID
	c, err := activate(d)
	if err != nil {
		x.Error = err.Error()
		return x
	}
	defer releaseCOM(c)
	if err = check("GetDevicePeriod", call(c, 9, uintptr(unsafe.Pointer(&x.DefaultPeriod100ns)), uintptr(unsafe.Pointer(&x.MinimumPeriod100ns)))); err != nil {
		x.Error = err.Error()
	}
	for _, f := range formats {
		w := wireFormat(f)
		hr := call(c, 7, 1, uintptr(unsafe.Pointer(&w)), 0)
		x.Formats = append(x.Formats, Support{f, hr == 0, uint32(hr)})
	}
	return x
}

type wasapi struct {
	powerEpoch                            uint64
	shared                                bool
	volumeClient                          uintptr
	gains                                 [2]float32
	packet, padding                       uint32
	initialized, pollShared               bool
	pollAt                                time.Time
	client, render, clock                 uintptr
	getBuffer, releaseBuffer, getPosition uintptr
	control, event                        windows.Handle
	handles                               [2]windows.Handle
	mmcss                                 uintptr
	task                                  uint32
	frames                                uint32
	frameBytes                            int
	data                                  unsafe.Pointer // out parameters reused: no allocation per packet
	pos, qpc                              uint64
}

func newNative() native { return &wasapi{} }
func (d *wasapi) open(c Config) (Opened, error) {
	info := Opened{Format: c.Format, Shared: c.Shared, StableBuffer: c.StableBuffer}
	var powerErr error
	d.powerEpoch, powerErr = watchPower()
	if powerErr != nil {
		return info, powerErr
	}
	d.shared = c.Shared
	if err := windows.CoInitializeEx(0, 0); err != nil {
		return info, err
	}
	d.initialized = true
	e, err := enumerator()
	if err != nil {
		return info, err
	}
	defer releaseCOM(e)
	dev, err := endpoint(e, c.EndpointID)
	if err != nil {
		return info, err
	}
	defer releaseCOM(dev)
	info.EndpointID, err = deviceID(dev)
	if err != nil {
		return info, err
	}
	d.client, err = activate(dev)
	if err != nil {
		return info, err
	}
	f := wireFormat(c.Format)
	if !c.Shared {
		if hr := call(d.client, 7, 1, uintptr(unsafe.Pointer(&f)), 0); hr != 0 {
			return info, &DeviceError{Operation: "exclusive format unsupported", Code: uint32(hr)}
		}
	}
	var def, minPeriod int64
	if err = check("GetDevicePeriod", call(d.client, 9, uintptr(unsafe.Pointer(&def)), uintptr(unsafe.Pointer(&minPeriod)))); err != nil {
		return info, err
	}
	period := int64(c.Period / 100)
	if period == 0 {
		period = def
		if c.StableBuffer && !c.Shared {
			period = max(period, int64(60*time.Millisecond/100))
		}
	}
	period = max(period, minPeriod)
	var hr uintptr
	if c.Shared {
		var mix *waveFormat
		if err = check("GetMixFormat", call(d.client, 8, uintptr(unsafe.Pointer(&mix)))); err != nil {
			return info, err
		}
		info.MixFormat, err = readMixFormat(mix)
		windows.CoTaskMemFree(unsafe.Pointer(mix))
		if err != nil {
			return info, err
		}
		// Explicit system conversion is confined to shared mode. It is shown
		// in the signal path, never described as strict native output.
		// A shared event-driven client uses the engine's minimum buffer.
		// Polling padding lets the app keep a longer shared buffer queued.
		var flags uintptr = 0x88040000 // auto conversion, SRC quality, event callback
		var duration uintptr
		d.pollShared = c.StableBuffer
		if d.pollShared {
			flags &^= 0x40000
			duration = uintptr(250 * time.Millisecond / 100)
		}
		hr = call(d.client, 3, 0, flags, duration, 0, uintptr(unsafe.Pointer(&f)), 0)
		period = def
	} else {
		hr = call(d.client, 3, 1, 0x40000, uintptr(period), uintptr(period), uintptr(unsafe.Pointer(&f)), 0)
	}
	if !c.Shared && uint32(hr) == 0x88890019 { // AUDCLNT_E_BUFFER_SIZE_NOT_ALIGNED
		if err = check("GetBufferSize(alignment)", call(d.client, 4, uintptr(unsafe.Pointer(&d.frames)))); err != nil {
			return info, err
		}
		period = (int64(d.frames)*10000000 + int64(c.Format.Rate)/2) / int64(c.Format.Rate)
		releaseCOM(d.client)
		d.client = 0
		d.client, err = activate(dev)
		if err != nil {
			return info, err
		}
		hr = call(d.client, 3, 1, 0x40000, uintptr(period), uintptr(period), uintptr(unsafe.Pointer(&f)), 0)
	}
	if err = check("Initialize(event)", hr); err != nil {
		return info, err
	}
	if err = check("GetBufferSize", call(d.client, 4, uintptr(unsafe.Pointer(&d.frames)))); err != nil {
		return info, err
	}
	if d.frames == 0 {
		return info, errors.New("wasapi: empty device buffer")
	}
	d.frameBytes = c.Format.FrameBytes()
	info.BufferFrames = int(d.frames)
	info.Period = time.Duration(period) * 100
	var latency int64
	if err = check("GetStreamLatency", call(d.client, 5, uintptr(unsafe.Pointer(&latency)))); err != nil {
		return info, err
	}
	info.Latency = time.Duration(latency) * 100
	if err = check("GetService(render)", call(d.client, 14, uintptr(unsafe.Pointer(&iidRender)), uintptr(unsafe.Pointer(&d.render)))); err != nil {
		return info, err
	}
	if err = check("GetService(clock)", call(d.client, 14, uintptr(unsafe.Pointer(&iidClock)), uintptr(unsafe.Pointer(&d.clock)))); err != nil {
		return info, err
	}
	if c.Shared {
		if err = check("GetService(volume)", call(d.client, 14, uintptr(unsafe.Pointer(&iidStreamVolume)), uintptr(unsafe.Pointer(&d.volumeClient)))); err != nil {
			return info, err
		}
		if err = d.volume(0); err != nil {
			return info, err
		} // silent until controller applies its gain
	}
	d.getBuffer = method(d.render, 3)
	d.releaseBuffer = method(d.render, 4)
	d.getPosition = method(d.clock, 4)
	if err = check("GetFrequency", call(d.clock, 3, uintptr(unsafe.Pointer(&info.ClockFrequency)))); err != nil {
		return info, err
	}
	d.control, err = windows.CreateEvent(nil, 0, 0, nil)
	if err != nil {
		return info, err
	}
	if !d.pollShared {
		d.event, err = windows.CreateEvent(nil, 0, 0, nil)
		if err != nil {
			return info, err
		}
		d.handles = [2]windows.Handle{d.control, d.event}
		if err = check("SetEventHandle", call(d.client, 13, uintptr(d.event))); err != nil {
			return info, err
		}
	}
	task, _ := windows.UTF16PtrFromString("Pro Audio")
	d.mmcss, _, err = setMMCSS.Call(uintptr(unsafe.Pointer(task)), uintptr(unsafe.Pointer(&d.task)))
	if d.mmcss == 0 {
		return info, fmt.Errorf("wasapi: MMCSS: %w", err)
	}
	info.MMCSS = true
	return info, nil
}
func (d *wasapi) wake() error { return windows.SetEvent(d.control) }
func (d *wasapi) closeWake() error {
	if d.control == 0 {
		return nil
	}
	err := windows.CloseHandle(d.control)
	d.control = 0
	return err
}
func (d *wasapi) wait(timeout time.Duration) (bool, error) {
	if d.powerChanged() {
		return false, ErrResumeRequired
	}
	if d.pollShared {
		// A stream of gain or playback commands must not postpone audio service.
		if d.pollAt.IsZero() {
			d.pollAt = time.Now().Add(timeout)
		}
		var err error
		if delay := time.Until(d.pollAt); delay > 0 {
			ms := (min(delay, timeout) + time.Millisecond - 1) / time.Millisecond
			_, err = windows.WaitForSingleObject(d.control, uint32(ms))
		}
		if d.powerChanged() {
			return false, ErrResumeRequired
		}
		now := time.Now()
		due := !now.Before(d.pollAt)
		if due {
			d.pollAt = now.Add(timeout)
		}
		return due, err
	}
	n, err := windows.WaitForMultipleObjects(d.handles[:], false, uint32(timeout/time.Millisecond))
	if d.powerChanged() {
		return false, ErrResumeRequired
	}
	return n == windows.WAIT_OBJECT_0+1, err
}
func (d *wasapi) buffer() ([]byte, error) {
	d.packet = d.frames
	if d.shared {
		if err := check("GetCurrentPadding", call(d.client, 6, uintptr(unsafe.Pointer(&d.padding)))); err != nil {
			return nil, err
		}
		if d.padding > d.frames {
			return nil, errors.New("wasapi: invalid padding")
		}
		d.packet -= d.padding
		if d.packet == 0 {
			return nil, nil
		}
	}
	// Direct SyscallN preserves pointer liveness without forcing a variadic
	// wrapper's argument slice onto the heap on every device packet.
	hr, _, _ := syscall.SyscallN(d.getBuffer, d.render, uintptr(d.packet), uintptr(unsafe.Pointer(&d.data)))
	if err := check("GetBuffer", hr); err != nil {
		return nil, err
	}
	return unsafe.Slice((*byte)(d.data), int(d.packet)*d.frameBytes), nil
}
func (d *wasapi) release() error {
	hr, _, _ := syscall.SyscallN(d.releaseBuffer, d.render, uintptr(d.packet), 0)
	err := check("ReleaseBuffer", hr)
	d.data = nil
	return err
}
func (d *wasapi) start() error {
	if d.powerChanged() {
		return ErrResumeRequired
	}
	return check("Start", call(d.client, 10))
}
func (d *wasapi) stop() error  { return check("Stop", call(d.client, 11)) }
func (d *wasapi) reset() error { return check("Reset", call(d.client, 12)) }
func (d *wasapi) position() (uint64, bool, error) {
	hr, _, _ := syscall.SyscallN(d.getPosition, d.clock, uintptr(unsafe.Pointer(&d.pos)), uintptr(unsafe.Pointer(&d.qpc)))
	return d.pos, hr == 0, check("GetPosition", hr)
}

func method(obj uintptr, index int) uintptr {
	vt := *(*uintptr)(pointer(obj))
	return *(*uintptr)(unsafe.Add(pointer(vt), index*8))
}
func (d *wasapi) close() {
	if d.client != 0 {
		_ = d.stop()
	}
	releaseCOM(d.clock)
	releaseCOM(d.volumeClient)
	releaseCOM(d.render)
	releaseCOM(d.client)
	if d.event != 0 {
		windows.CloseHandle(d.event)
	}
	if d.mmcss != 0 {
		revertMMCSS.Call(d.mmcss)
	}
	if d.initialized {
		windows.CoUninitialize()
	}
}

func (d *wasapi) volume(gain float64) error {
	d.gains = [2]float32{float32(gain), float32(gain)}
	return check("SetAllVolumes", call(d.volumeClient, 6, 2, uintptr(unsafe.Pointer(&d.gains[0]))))
}

func readMixFormat(w *waveFormat) (pcm.Format, error) {
	if w == nil {
		return pcm.Format{}, errors.New("wasapi: missing mix format")
	}
	f := pcm.Format{Kind: pcm.Integer, Rate: int(w.Rate), Channels: int(w.Channels), ContainerBits: int(w.Bits), ValidBits: int(w.Bits)}
	tag := uint32(w.Tag)
	if w.Tag == 0xfffe {
		if w.Size < 22 {
			return pcm.Format{}, errors.New("wasapi: truncated extensible mix format")
		}
		f.ChannelMask = w.Mask
		f.ValidBits = int(w.Valid)
		tag = w.Sub.Data1
	}
	switch tag {
	case 1:
	case 3:
		f.Kind, f.ValidBits = pcm.Float32, 0
		if w.Bits == 64 {
			f.Kind = pcm.Float64
		}
	default:
		return pcm.Format{}, errors.New("wasapi: unsupported mix encoding")
	}
	if f.ChannelMask == 0 && f.Channels == 1 {
		f.ChannelMask = 4
	}
	if f.ChannelMask == 0 && f.Channels == 2 {
		f.ChannelMask = 3
	}
	return f, f.Validate()
}
