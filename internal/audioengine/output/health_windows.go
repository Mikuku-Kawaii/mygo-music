//go:build windows && amd64

package output

import (
	"context"
	"golang.org/x/sys/windows"
	"runtime"
	"unsafe"
)

// CheckEndpoint is control-plane work; it never runs on the output thread.
// Fixed endpoints cannot migrate. A default change requires an explicit reopen.
func CheckEndpoint(ctx context.Context, id string, followDefault bool) error {
	done := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()
		if err := windows.CoInitializeEx(0, 0); err != nil {
			done <- err
			return
		}
		defer windows.CoUninitialize()
		e, err := enumerator()
		if err != nil {
			done <- err
			return
		}
		defer releaseCOM(e)
		d, err := endpoint(e, id)
		if err != nil {
			done <- err
			return
		}
		defer releaseCOM(d)
		var state uint32
		if err = check("GetState", call(d, 6, uintptr(unsafe.Pointer(&state)))); err != nil {
			done <- err
			return
		}
		if state != 1 {
			done <- &DeviceError{Operation: "endpoint inactive", Code: 0x88890004}
			return
		}
		if followDefault {
			current, err := endpoint(e, "")
			if err != nil {
				done <- err
				return
			}
			defer releaseCOM(current)
			currentID, err := deviceID(current)
			if err != nil {
				done <- err
				return
			}
			if currentID != id {
				done <- ErrDefaultChanged
				return
			}
		}
		done <- nil
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}
