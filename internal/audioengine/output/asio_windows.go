//go:build windows && amd64

package output

import (
	"encoding/binary"
	"errors"
	"math"
	"runtime"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"github.com/ebitengine/purego"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
	"mygo-music/internal/audioengine/buffer"
	"mygo-music/internal/audioengine/pcm"
)

const (
	asioInit = 3 + iota
	asioName
	asioVersion
	asioGetError
	asioStart
	asioStop
	asioChannels
	asioLatencies
	asioBufferSize
	asioCanRate
	asioGetRate
	asioSetRate
	asioGetClocks
	asioSetClock
	asioSamplePosition
	asioGetChannelInfo
	asioCreateBuffers
	asioDispose
	asioControlPanel
	asioFuture
	asioOutputReady
)

const (
	asioSelectorSupported = 1
	asioEngineVersion     = 2
	asioResetRequest      = 3
	asioResyncRequest     = 5
	asioLatenciesChanged  = 6
)

var (
	currentASIO  atomic.Pointer[asioDevice]
	desktopHWND  = windows.NewLazySystemDLL("user32.dll").NewProc("GetDesktopWindow")
	errASIOClock = errors.New("ASIO sample clock is unavailable")
)

// ASIO callbacks have no user-data pointer. One stable callback table and
// one live driver lease allow replacement without allocating new thunks.
var asioCallbacks = [...]uintptr{
	syscall.NewCallback(func(index, _ uintptr) uintptr {
		if d := currentASIO.Load(); d != nil {
			d.switchBuffer(int(int32(index)))
		}
		return 0
	}),
	// The callback notification identifies a change. The control thread
	// pauses and releases this epoch before a new rate is negotiated.
	syscall.NewCallback(func() uintptr {
		if d := currentASIO.Load(); d != nil {
			d.interrupted.Store(true)
		}
		return 0
	}),
	syscall.NewCallback(func(selector, value, _, _ uintptr) uintptr {
		switch selector {
		case asioSelectorSupported:
			if value == asioEngineVersion || value == asioResetRequest || value == asioResyncRequest || value == asioLatenciesChanged {
				return 1
			}
		case asioEngineVersion:
			return 2
		case asioResetRequest, asioResyncRequest, asioLatenciesChanged:
			if d := currentASIO.Load(); d != nil {
				d.interrupted.Store(true)
			}
			return 1
		}
		return 0
	}),
	0, // legacy buffer switch obtains its sample position from IASIO
}

type asioBufferInfo struct {
	input, channel int32
	buffers        [2]uintptr
}

type asioChannelInfo struct {
	channel, input, active, group, kind int32
	name                                [32]byte
}

type asioDevice struct {
	driver          uintptr
	wakeHandle      windows.Handle
	powerEpoch      uint64
	com, allocated  bool
	started         bool
	bufInfo         *[2]asioBufferInfo
	pin             runtime.Pinner
	buf             [2][2][]byte
	scratch         []byte
	ring            *buffer.Ring
	stream          *Stream
	info            Opened
	outputReady     bool
	outputLatency   uint64
	active, busy    atomic.Bool
	interrupted     atomic.Bool
	callbackFailure atomic.Bool
	presented       atomic.Uint64
	capturedClock   atomic.Bool
	final           atomic.Uint64 // final frame + 1, or zero until EOF
	// The following fields are used by only one callback at a time.
	callbackIndex   int
	lastPacket      time.Time
	origin, finalAt uint64
	clockSet        bool
	finalKnown      bool
}

func newASIO() native { return &asioDevice{} }

func ListASIO() ([]ASIODriver, error) {
	root, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\ASIO`, registry.ENUMERATE_SUB_KEYS|registry.WOW64_64KEY)
	if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer root.Close()
	names, err := root.ReadSubKeyNames(-1)
	if err != nil {
		return nil, err
	}
	var drivers []ASIODriver
	for _, name := range names {
		k, err := registry.OpenKey(root, name, registry.QUERY_VALUE|registry.WOW64_64KEY)
		if err != nil {
			continue
		}
		id, _, idErr := k.GetStringValue("CLSID")
		label, _, _ := k.GetStringValue("Description")
		k.Close()
		guid, parseErr := windows.GUIDFromString(id)
		if idErr != nil || parseErr != nil {
			continue
		}
		if label == "" {
			label = name
		}
		drivers = append(drivers, ASIODriver{ID: guid.String(), Name: label})
	}
	return drivers, nil
}

func (a *asioDevice) open(c Config) (Opened, error) {
	if !currentASIO.CompareAndSwap(nil, a) {
		return Opened{}, ErrASIOBusy
	}
	if err := windows.CoInitializeEx(0, windows.COINIT_APARTMENTTHREADED); err != nil {
		return Opened{}, err
	}
	a.com = true
	id, err := windows.GUIDFromString(c.ASIO)
	if err != nil {
		return Opened{}, err
	}
	drivers, err := ListASIO()
	if err != nil {
		return Opened{}, err
	}
	linked := false
	for _, d := range drivers {
		linked = linked || d.ID == id.String()
	}
	if !linked {
		return Opened{}, errors.New("ASIO driver is unavailable")
	}
	hr, _, _ := createCOM.Call(uintptr(unsafe.Pointer(&id)), 0, 1, uintptr(unsafe.Pointer(&id)), uintptr(unsafe.Pointer(&a.driver)))
	if err := check("ASIO activation", hr); err != nil {
		return Opened{}, err
	}
	if a.driver == 0 {
		return Opened{}, ErrASIOFormat
	}
	hwnd, _, _ := desktopHWND.Call()
	if int32(call(a.driver, asioInit, hwnd)) != 1 {
		return Opened{}, errors.New("ASIO driver initialization failed")
	}
	var input, channels, low, high, best, gran int32
	if err := asioCheck("channels", call(a.driver, asioChannels, uintptr(unsafe.Pointer(&input)), uintptr(unsafe.Pointer(&channels)))); err != nil {
		return Opened{}, err
	}
	if channels < 2 {
		return Opened{}, ErrASIOFormat
	}
	if err := a.configureRate(c.Format.Rate); err != nil {
		return Opened{}, err
	}
	if err := asioCheck("buffer size", call(a.driver, asioBufferSize,
		uintptr(unsafe.Pointer(&low)), uintptr(unsafe.Pointer(&high)), uintptr(unsafe.Pointer(&best)), uintptr(unsafe.Pointer(&gran)))); err != nil {
		return Opened{}, err
	}
	// A fixed negotiated packet has an upper bound before allocation.
	if low < 1 || best < low || best > high || best > 32768 {
		return Opened{}, ErrASIOFormat
	}
	f, err := a.channelFormat(c.Format)
	if err != nil {
		return Opened{}, err
	}
	a.bufInfo = new([2]asioBufferInfo)
	a.bufInfo[1].channel = 1
	a.pin.Pin(a.bufInfo)
	a.outputReady = int32(call(a.driver, asioOutputReady)) == 0
	if err := asioCheck("create buffers", call(a.driver, asioCreateBuffers,
		uintptr(unsafe.Pointer(a.bufInfo)), 2, uintptr(best), uintptr(unsafe.Pointer(&asioCallbacks[0])))); err != nil {
		return Opened{}, err
	}
	a.allocated = true
	for ch := range 2 {
		for i := range 2 {
			b := a.bufInfo[ch].buffers[i]
			if b == 0 {
				return Opened{}, ErrASIOFormat
			}
			a.buf[ch][i] = unsafe.Slice((*byte)(pointer(b)), int(best)*f.ContainerBits/8)
			clear(a.buf[ch][i])
		}
	}
	var latency int32
	if err := asioCheck("latencies", call(a.driver, asioLatencies, uintptr(unsafe.Pointer(&input)), uintptr(unsafe.Pointer(&latency)))); err != nil {
		return Opened{}, err
	}
	if latency < 0 || latency > int32(c.Format.Rate) {
		return Opened{}, ErrASIOFormat
	}
	a.outputLatency = uint64(latency)
	a.scratch = make([]byte, int(best)*c.Format.FrameBytes())
	a.wakeHandle, err = windows.CreateEvent(nil, 0, 0, nil)
	if err != nil {
		return Opened{}, err
	}
	a.powerEpoch, err = watchPower()
	if err != nil {
		return Opened{}, err
	}
	rate := time.Duration(c.Format.Rate)
	return Opened{ASIO: true, EndpointID: c.ASIO, Format: c.Format, DriverFormat: f, BufferFrames: int(best),
		Period: time.Duration(best) * time.Second / rate, Latency: time.Duration(latency) * time.Second / rate,
		ClockFrequency: uint64(c.Format.Rate)}, nil
}

func (a *asioDevice) configureRate(rate int) error {
	var got float64
	if err := asioCheck("sample rate", call(a.driver, asioGetRate, uintptr(unsafe.Pointer(&got)))); err == nil && got == float64(rate) {
		return nil
	}
	var canRate, setRate func(uintptr, float64) int32
	purego.RegisterFunc(&canRate, method(a.driver, asioCanRate))
	purego.RegisterFunc(&setRate, method(a.driver, asioSetRate))
	if canRate(a.driver, float64(rate)) != 0 || setRate(a.driver, float64(rate)) != 0 {
		return ErrASIOFormat
	}
	if int32(call(a.driver, asioGetRate, uintptr(unsafe.Pointer(&got)))) != 0 || got != float64(rate) {
		return ErrASIOFormat
	}
	return nil
}

func (a *asioDevice) channelFormat(source pcm.Format) (pcm.Format, error) {
	var typ, first int32
	for ch := range 2 {
		i := asioChannelInfo{channel: int32(ch)}
		if err := asioCheck("channel format", call(a.driver, asioGetChannelInfo, uintptr(unsafe.Pointer(&i)))); err != nil {
			return pcm.Format{}, err
		}
		typ = i.kind
		if ch == 0 {
			first = typ
		}
	}
	if typ != first {
		return pcm.Format{}, ErrASIOFormat
	}
	f := source
	switch typ {
	case 16, 17, 18: // full-width little-endian integer
		f.ValidBits, f.ContainerBits = int(typ-14)*8, int(typ-14)*8
		if f.ValidBits < source.ValidBits {
			return pcm.Format{}, ErrASIOFormat
		}
	case 19: // float32 may exactly represent up to 24 effective bits
		if source.ValidBits > 24 {
			return pcm.Format{}, ErrASIOFormat
		}
		f.Kind, f.ContainerBits, f.ValidBits = pcm.Float32, 32, 0
	case 20:
		f.Kind, f.ContainerBits, f.ValidBits = pcm.Float64, 64, 0
	default:
		return pcm.Format{}, &asioError{operation: "unsupported channel sample type", code: typ}
	}
	return f, nil
}

func asioCheck(op string, result uintptr) error {
	if code := int32(result); code != 0 {
		return &asioError{op, code}
	}
	return nil
}

func (a *asioDevice) attach(s *Stream, r *buffer.Ring, info Opened) {
	a.stream, a.ring, a.info = s, r, info
}

func (a *asioDevice) switchBuffer(index int) {
	if !a.active.Load() {
		return
	}
	if !a.busy.CompareAndSwap(false, true) {
		a.interrupted.Store(true)
		return
	}
	defer a.busy.Store(false)
	if !a.active.Load() || index < 0 || index > 1 {
		return
	}
	a.callbackIndex = index
	now := time.Now()
	if !a.lastPacket.IsZero() {
		s := &a.stream.stats
		s.packetGap.Store(max(s.packetGap.Load(), uint64(now.Sub(a.lastPacket))))
	}
	a.lastPacket = now
	if a.advanceClock() {
		a.capturedClock.Store(true)
	}
	if err := a.stream.fill(a.ring, a.info, &a.finalAt, &a.finalKnown); err != nil {
		a.callbackFailure.Store(true)
	}
	if a.finalKnown {
		a.final.Store(a.finalAt + 1)
	}
	s := &a.stream.stats
	s.service.Store(max(s.service.Load(), uint64(time.Since(now))))
}

func (a *asioDevice) advanceClock() bool {
	var ticks, stamp [2]uint32
	if int32(call(a.driver, asioSamplePosition, uintptr(unsafe.Pointer(&ticks[0])), uintptr(unsafe.Pointer(&stamp[0])))) != 0 {
		return false
	}
	position := uint64(ticks[0])<<32 | uint64(ticks[1])
	if !a.clockSet {
		a.origin, a.clockSet = position, true
		return true
	}
	if position < a.origin {
		a.interrupted.Store(true)
		return false
	}
	elapsed := position - a.origin
	frame := elapsed - min(elapsed, a.outputLatency)
	a.presented.Store(frame)
	return true
}

func (a *asioDevice) buffer() ([]byte, error) { return a.scratch, nil }

func (a *asioDevice) release() error {
	dst := [2][]byte{a.buf[0][a.callbackIndex], a.buf[1][a.callbackIndex]}
	convertASIO(a.scratch, dst, a.info.Format, a.info.DriverFormat)
	if a.outputReady && int32(call(a.driver, asioOutputReady)) != 0 {
		return errASIOClock
	}
	return nil
}

func convertASIO(src []byte, dst [2][]byte, input, output pcm.Format) {
	inBytes, outBytes := input.ContainerBits/8, output.ContainerBits/8
	for i := range len(src) / (2 * inBytes) {
		for ch := range 2 {
			word := src[(2*i+ch)*inBytes:][:inBytes]
			var u uint32
			for k, b := range word {
				u |= uint32(b) << (8 * k)
			}
			v := u << (32 - 8*inBytes)
			b := dst[ch][i*outBytes:][:outBytes]
			switch output.Kind {
			case pcm.Float32:
				binary.LittleEndian.PutUint32(b, math.Float32bits(float32(int32(v))*(1.0/(1<<31))))
			case pcm.Float64:
				binary.LittleEndian.PutUint64(b, math.Float64bits(float64(int32(v))*(1.0/(1<<31))))
			default:
				v >>= 32 - 8*outBytes
				for k := range b {
					b[k] = byte(v >> (8 * k))
				}
			}
		}
	}
}

func (a *asioDevice) endFrame() (uint64, bool) {
	end := a.final.Load()
	return end - min(end, 1), end != 0
}

func (a *asioDevice) start() error {
	if a.started {
		return nil
	}
	a.active.Store(true)
	err := asioCheck("start", call(a.driver, asioStart))
	if err != nil {
		a.active.Store(false)
	}
	a.started = err == nil
	return err
}

func (a *asioDevice) stop() error {
	if !a.started {
		return nil
	}
	a.active.Store(false)
	err := asioCheck("stop", call(a.driver, asioStop))
	for a.busy.Load() {
		runtime.Gosched()
	}
	a.started = false
	return err
}

func (a *asioDevice) reset() error { return ErrASIOReset }

func (a *asioDevice) position() (uint64, bool, error) {
	return a.presented.Load(), a.capturedClock.Load(), nil
}

func (a *asioDevice) wait(t time.Duration) (bool, error) {
	_, err := windows.WaitForSingleObject(a.wakeHandle, uint32(t/time.Millisecond))
	if a.interrupted.Load() {
		return false, ErrASIOReset
	}
	if a.powerEpoch != powerWatch.epoch.Load() {
		return false, ErrResumeRequired
	}
	if a.callbackFailure.Load() {
		return false, errASIOClock
	}
	return false, err
}

func (a *asioDevice) wake() error { return windows.SetEvent(a.wakeHandle) }
func (a *asioDevice) closeWake() error {
	if a.wakeHandle == 0 {
		return nil
	}
	return windows.CloseHandle(a.wakeHandle)
}

func (a *asioDevice) close() {
	_ = a.stop()
	if a.allocated {
		_ = asioCheck("dispose", call(a.driver, asioDispose))
	}
	releaseCOM(a.driver)
	a.pin.Unpin()
	currentASIO.CompareAndSwap(a, nil)
	if a.com {
		windows.CoUninitialize()
	}
}
