//go:build windows && amd64

package output

import (
	"golang.org/x/sys/windows"
	"sync"
	"sync/atomic"
	"unsafe"
)

// One process-lifetime registration, independent of UI windows. The callback
// only advances an atomic epoch; each device thread stops its own COM stream.
// Keeping the callback and recipient alive also avoids reuse after unregister.
var powerWatch struct {
	once      sync.Once
	epoch     atomic.Uint64
	recipient struct{ callback, context uintptr }
	handle    uintptr
	err       error
}

func notePowerEvent(kind uintptr) {
	switch kind {
	case 4, 6, 7, 18: // suspend, critical resume, resume suspend, automatic resume
		powerWatch.epoch.Add(1)
	}
}
func watchPower() (uint64, error) {
	powerWatch.once.Do(func() {
		powerWatch.recipient.callback = windows.NewCallback(func(_, kind, _ uintptr) uintptr { notePowerEvent(kind); return 0 })
		register := windows.NewLazySystemDLL("powrprof.dll").NewProc("PowerRegisterSuspendResumeNotification")
		if err := register.Find(); err != nil {
			powerWatch.err = err
			return
		}
		code, _, _ := register.Call(2, uintptr(unsafe.Pointer(&powerWatch.recipient)), uintptr(unsafe.Pointer(&powerWatch.handle)))
		if code != 0 {
			powerWatch.err = windows.Errno(code)
		}
	})
	return powerWatch.epoch.Load(), powerWatch.err
}
func (d *wasapi) powerChanged() bool { return d.powerEpoch != powerWatch.epoch.Load() }
