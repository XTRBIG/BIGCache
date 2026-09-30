//go:build !linux

package nbd

import (
	"errors"
	"time"
)

// ErrUnsupported is returned on platforms without the Linux NBD driver.
var ErrUnsupported = errors.New("attaching NBD devices requires Linux")

// Attach is only available on Linux.
func Attach(device, addr, export string, timeout time.Duration) error { return ErrUnsupported }

// Detach is only available on Linux.
func Detach(device string) error { return ErrUnsupported }

// Attached is only available on Linux.
func Attached(device string) bool { return false }
