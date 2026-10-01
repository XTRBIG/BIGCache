//go:build windows

package nbd

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// On Windows the kernel side is provided by WNBD, the open source NBD
// driver from the Ceph project (https://github.com/cloudbase/wnbd). Its
// command line tool wnbd-client maps an NBD export as a local disk, which
// then appears in Disk Management like any other drive.

// ErrWNBDMissing is returned when wnbd-client.exe cannot be found.
var ErrWNBDMissing = errors.New("wnbd-client.exe not found: install the WNBD driver from https://github.com/cloudbase/wnbd/releases (or Ceph for Windows) and make sure wnbd-client is on the PATH")

func wnbdClient() (string, error) {
	if p, err := exec.LookPath("wnbd-client"); err == nil {
		return p, nil
	}
	candidates := []string{
		filepath.Join(os.Getenv("ProgramFiles"), "Ceph", "bin", "wnbd-client.exe"),
		filepath.Join(os.Getenv("ProgramFiles"), "WNBD", "wnbd-client.exe"),
		filepath.Join(os.Getenv("ProgramFiles(x86)"), "WNBD", "wnbd-client.exe"),
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c, nil
		}
	}
	return "", ErrWNBDMissing
}

func runWNBD(args ...string) (string, error) {
	exe, err := wnbdClient()
	if err != nil {
		return "", err
	}
	out, err := exec.Command(exe, args...).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("wnbd-client %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// Attach maps the export as a Windows disk. device is the WNBD instance
// name (any identifier; the export name is a good choice). Unlike the
// Linux version this returns as soon as the disk is mapped; the WNBD
// driver keeps the connection open until Detach.
func Attach(device, addr, export string, timeout time.Duration) error {
	if strings.HasPrefix(addr, "unix:") {
		return errors.New("WNBD needs a TCP listen address, not a unix socket")
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("invalid server address %q: %w", addr, err)
	}
	if host == "" {
		host = "127.0.0.1"
	}
	if device == "" {
		device = export
	}
	args := []string{"map", device, "--hostname", host, "--port", port, "--export-name", export}
	if timeout > 0 {
		args = append(args, "--nbd-timeout", fmt.Sprint(int(timeout/time.Second)))
	}
	_, err = runWNBD(args...)
	return err
}

// Detach unmaps a WNBD instance.
func Detach(device string) error {
	_, err := runWNBD("unmap", device)
	return err
}

// Attached reports whether a WNBD instance with this name exists.
func Attached(device string) bool {
	out, err := runWNBD("list")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 && fields[0] == device {
			return true
		}
	}
	return false
}
