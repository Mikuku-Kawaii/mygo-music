package output

import (
	"errors"
	"fmt"
)

var ErrDefaultChanged = errors.New("output: default device changed; reopen required")
var ErrResumeRequired = errors.New("output: device observation interrupted; reopen required")
var ErrClockStopped = errors.New("output: device clock stopped advancing")

type DeviceError struct {
	Operation string
	Code      uint32
}

func (e *DeviceError) Error() string {
	return fmt.Sprintf("wasapi: %s HRESULT 0x%08x", e.Operation, e.Code)
}

func FailureCode(err error) uint32 {
	var d *DeviceError
	if errors.As(err, &d) {
		return d.Code
	}
	return 0
}

func FailureKind(err error) string {
	if errors.Is(err, ErrASIOFormat) || errors.Is(err, ErrDoPFormat) {
		return "format-unsupported"
	}
	if errors.Is(err, ErrASIOBusy) {
		return "device-busy"
	}
	if errors.Is(err, ErrASIOReset) {
		return "resume-required"
	}
	var asio *asioError
	if errors.As(err, &asio) {
		return "device-error"
	}
	if errors.Is(err, ErrDefaultChanged) {
		return "default-changed"
	}
	if errors.Is(err, ErrResumeRequired) {
		return "resume-required"
	}
	if errors.Is(err, ErrClockStopped) {
		return "clock-stopped"
	}
	var d *DeviceError
	if errors.As(err, &d) {
		switch d.Code {
		case 0x88890004, 0x88890026, 0x80070490:
			return "device-lost"
		case 0x88890010:
			return "service-stopped"
		case 0x8889000a:
			return "device-busy"
		case 0x88890008:
			return "format-unsupported"
		}
		return "device-error"
	}
	return "source-error"
}
