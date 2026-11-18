package downloads

import (
	"errors"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
)

func publishFile(root *os.Root, from, to string) error {
	err := root.Link(from, to)
	if err == nil || errors.Is(err, os.ErrExist) {
		return err
	}
	if !errors.Is(err, windows.ERROR_INVALID_FUNCTION) && !errors.Is(err, windows.ERROR_NOT_SUPPORTED) {
		return err
	}
	// FAT/exFAT cannot hard-link. Windows MoveFile also refuses to replace an
	// existing destination, and both names live on the same volume.
	source, err := windows.UTF16PtrFromString(filepath.Join(root.Name(), from))
	if err != nil {
		return err
	}
	dest, err := windows.UTF16PtrFromString(filepath.Join(root.Name(), to))
	if err != nil {
		return err
	}
	if err = windows.MoveFile(source, dest); errors.Is(err, windows.ERROR_FILE_EXISTS) || errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		return os.ErrExist
	}
	return err
}
