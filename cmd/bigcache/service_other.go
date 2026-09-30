//go:build !windows

package main

import "errors"

// runningAsService reports whether we were started by a service manager
// that needs special handling (only Windows).
func runningAsService() bool { return false }

func runAsService(run func(stop <-chan struct{}) error) error { return run(nil) }

func cmdService(args []string) error {
	return errors.New("the 'service' command manages the Windows service; on Linux use the systemd unit in examples/")
}
