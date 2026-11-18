package output

import (
	"errors"
	"fmt"
)

type ASIODriver struct{ ID, Name string }

var (
	ErrASIOFormat = errors.New("ASIO driver does not support the requested PCM rate or precision")
	ErrASIOReset  = errors.New("ASIO driver requested a reset or changed format")
	ErrASIOBusy   = errors.New("an ASIO stream already owns the driver callbacks")
)

type asioError struct {
	operation string
	code      int32
}

func (e *asioError) Error() string { return fmt.Sprintf("ASIO %s failed (%d)", e.operation, e.code) }
