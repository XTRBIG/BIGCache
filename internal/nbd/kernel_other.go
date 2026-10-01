//go:build !linux && !windows

package nbd

import (
	"errors"
	"time"
)

// ErrUnsupported is returned on platforms without an NBD kernel driver.
var ErrUnsupported = errors.New("attaching NBD devices requires Linux (nbd) or Windows (WNBD)")

// Attach is only available on Linux.
func Attach(device, addr, export string, timeout time.Duration) error { return ErrUnsupported }

// Detach is only available on Linux.
func Detach(device string) error { return ErrUnsupported }

// Attached is only available on Linux.
func Attached(device string) bool { return false }
