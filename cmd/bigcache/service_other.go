//go:build !windows

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// runningAsService reports whether we were started by a service manager
// that needs special handling (only Windows).
func runningAsService() bool { return false }

func runAsService(run func(stop <-chan struct{}) error) error { return run(nil) }

func cmdService(args []string) error {
	return errors.New("the 'service' command manages the Windows service; on Linux use the systemd unit in examples/")
}

const onlineHint = "  (unmount any remaining /dev/nbdN devices and re-mount the HDDs directly)"

func systemctl(args ...string) (string, error) {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return "", nil
	}
	out, err := exec.Command("systemctl", args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// serviceStop stops the systemd units when they exist. The attach units
// are stopped first so that the block devices disappear before the daemon
// flushes and exits.
func serviceStop() (bool, error) {
	out, _ := systemctl("list-units", "--plain", "--no-legend", "bigcache-attach@*.service")
	for _, line := range strings.Split(out, "\n") {
		if f := strings.Fields(line); len(f) > 0 && strings.HasPrefix(f[0], "bigcache-attach@") {
			if _, err := systemctl("stop", f[0]); err != nil {
				return false, fmt.Errorf("systemctl stop %s: %w", f[0], err)
			}
			fmt.Printf("stopped %s\n", f[0])
		}
	}
	out, _ = systemctl("is-active", "bigcache.service")
	if out != "active" && out != "activating" {
		return false, nil
	}
	if _, err := systemctl("stop", "bigcache.service"); err != nil {
		return false, fmt.Errorf("systemctl stop bigcache.service: %w", err)
	}
	return true, nil
}

// serviceUninstall disables the units; the files are removed by 'make
// uninstall' or the package manager.
func serviceUninstall() (bool, error) {
	if _, err := os.Stat("/etc/systemd/system/bigcache.service"); err != nil {
		return false, nil
	}
	_, _ = systemctl("disable", "--now", "bigcache.service")
	_, _ = systemctl("disable", "bigcache-attach@.service")
	return true, nil
}
