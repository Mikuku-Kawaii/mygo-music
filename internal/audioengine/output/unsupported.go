//go:build !windows || !amd64

package output

import (
	"context"
	"mygo-music/internal/audioengine/pcm"
	"time"
)

func Probe(context.Context, []pcm.Format) ([]Device, error) { return nil, ErrUnsupported }
func CheckEndpoint(context.Context, string, bool) error     { return ErrUnsupported }
func ListASIO() ([]ASIODriver, error)                       { return nil, ErrUnsupported }

type unsupported struct{}

func newNative() native                              { return unsupported{} }
func newASIO() native                                { return unsupported{} }
func (unsupported) open(Config) (Opened, error)      { return Opened{}, ErrUnsupported }
func (unsupported) wake() error                      { return ErrUnsupported }
func (unsupported) closeWake() error                 { return nil }
func (unsupported) wait(time.Duration) (bool, error) { return false, ErrUnsupported }
func (unsupported) buffer() ([]byte, error)          { return nil, ErrUnsupported }
func (unsupported) release() error                   { return ErrUnsupported }
func (unsupported) start() error                     { return ErrUnsupported }
func (unsupported) stop() error                      { return ErrUnsupported }
func (unsupported) reset() error                     { return ErrUnsupported }
func (unsupported) position() (uint64, bool, error)  { return 0, false, ErrUnsupported }
func (unsupported) close()                           {}
