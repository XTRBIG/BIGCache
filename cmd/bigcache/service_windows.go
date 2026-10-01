//go:build windows

package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"
	"golang.org/x/sys/windows/svc/mgr"
)

const serviceName = "BIGCache"

func runningAsService() bool {
	is, err := svc.IsWindowsService()
	return err == nil && is
}

// handler adapts the serve loop to the Windows service control manager.
type handler struct {
	run func(stop <-chan struct{}) error
	err error
}

func (h *handler) Execute(args []string, req <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	status <- svc.Status{State: svc.StartPending}
	stop := make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- h.run(stop) }()
	status <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	for {
		select {
		case err := <-done:
			h.err = err
			status <- svc.Status{State: svc.StopPending}
			if err != nil {
				return true, 1
			}
			return false, 0
		case r := <-req:
			switch r.Cmd {
			case svc.Interrogate:
				status <- r.CurrentStatus
			case svc.Stop, svc.Shutdown:
				// Writing dirty blocks back can take a while; keep the
				// SCM informed so it does not kill us.
				status <- svc.Status{State: svc.StopPending, WaitHint: 600_000}
				close(stop)
				h.err = <-done
				status <- svc.Status{State: svc.Stopped}
				if h.err != nil {
					return true, 1
				}
				return false, 0
			}
		}
	}
}

// runAsService runs the serve loop under the service control manager,
// logging to the file next to the configuration.
func runAsService(run func(stop <-chan struct{}) error) error {
	elog, _ := eventlog.Open(serviceName)
	if elog != nil {
		defer elog.Close()
	}
	h := &handler{run: run}
	if err := svc.Run(serviceName, h); err != nil {
		if elog != nil {
			elog.Error(1, fmt.Sprintf("service failed: %v", err))
		}
		return err
	}
	if h.err != nil && elog != nil {
		elog.Error(1, fmt.Sprintf("bigcache serve exited with error: %v", h.err))
	}
	return h.err
}

// cmdService implements "bigcache service install|uninstall|start|stop|status".
func cmdService(args []string) error {
	fs := flag.NewFlagSet("service", flag.ExitOnError)
	cfgPath := fs.String("c", defaultConfigPath(), "configuration file the service will use")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: bigcache service install|uninstall|start|stop|status [-c CONFIG]")
		fs.PrintDefaults()
	}
	fs.Parse(args)
	if fs.NArg() != 1 {
		fs.Usage()
		os.Exit(2)
	}
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect to the service manager (run as Administrator): %w", err)
	}
	defer m.Disconnect()

	switch fs.Arg(0) {
	case "install":
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		abs, _ := filepath.Abs(*cfgPath)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			return err
		}
		if _, err := os.Stat(abs); errors.Is(err, os.ErrNotExist) {
			if err := os.WriteFile(abs, []byte(exampleWindowsConfig), 0o600); err != nil {
				return err
			}
			fmt.Printf("wrote an example configuration to %s; edit it before starting the service\n", abs)
		}
		if s, err := m.OpenService(serviceName); err == nil {
			s.Close()
			return fmt.Errorf("service %s is already installed", serviceName)
		}
		s, err := m.CreateService(serviceName, exe, mgr.Config{
			DisplayName:      "BIGCache SSD cache",
			Description:      "Uses an SSD as a persistent block cache for HDD volumes and serves them over NBD.",
			StartType:        mgr.StartAutomatic,
			DelayedAutoStart: true,
		}, "serve", "-c", abs)
		if err != nil {
			return fmt.Errorf("create service: %w", err)
		}
		defer s.Close()
		// Restart on failure after 10 seconds.
		_ = s.SetRecoveryActions([]mgr.RecoveryAction{
			{Type: mgr.ServiceRestart, Delay: 10 * time.Second},
			{Type: mgr.ServiceRestart, Delay: 10 * time.Second},
			{Type: mgr.ServiceRestart, Delay: 60 * time.Second},
		}, 86400)
		if err := eventlog.InstallAsEventCreate(serviceName, eventlog.Error|eventlog.Warning|eventlog.Info); err != nil {
			fmt.Fprintf(os.Stderr, "warning: event log source not registered: %v\n", err)
		}
		fmt.Printf("service %s installed (automatic start), config %s, log %s\n", serviceName, abs, defaultLogPath())
		fmt.Println("start it with: bigcache service start")
		return nil

	case "uninstall":
		m.Disconnect()
		removed, err := serviceUninstall()
		if err != nil {
			return err
		}
		if !removed {
			return fmt.Errorf("service %s is not installed", serviceName)
		}
		fmt.Printf("service %s removed\n", serviceName)
		return nil

	case "start":
		s, err := m.OpenService(serviceName)
		if err != nil {
			return fmt.Errorf("service %s is not installed", serviceName)
		}
		defer s.Close()
		if err := s.Start(); err != nil {
			return fmt.Errorf("start: %w (see %s)", err, defaultLogPath())
		}
		fmt.Printf("service %s started\n", serviceName)
		return nil

	case "stop":
		s, err := m.OpenService(serviceName)
		if err != nil {
			return fmt.Errorf("service %s is not installed", serviceName)
		}
		defer s.Close()
		return stopService(s)

	case "status":
		s, err := m.OpenService(serviceName)
		if err != nil {
			fmt.Println("not installed")
			return nil
		}
		defer s.Close()
		st, err := s.Query()
		if err != nil {
			return err
		}
		fmt.Println(stateName(st.State))
		return nil
	}
	fs.Usage()
	os.Exit(2)
	return nil
}

func stopService(s *mgr.Service) error {
	st, err := s.Control(svc.Stop)
	if err != nil {
		return fmt.Errorf("stop: %w", err)
	}
	deadline := time.Now().Add(10 * time.Minute)
	for st.State != svc.Stopped {
		if time.Now().After(deadline) {
			return errors.New("timeout waiting for the service to stop")
		}
		time.Sleep(500 * time.Millisecond)
		if st, err = s.Query(); err != nil {
			return err
		}
	}
	fmt.Printf("service %s stopped\n", serviceName)
	return nil
}

func stateName(s svc.State) string {
	switch s {
	case svc.Stopped:
		return "stopped"
	case svc.StartPending:
		return "starting"
	case svc.StopPending:
		return "stopping"
	case svc.Running:
		return "running"
	case svc.ContinuePending, svc.PausePending, svc.Paused:
		return "paused"
	}
	return fmt.Sprintf("state %d", s)
}

const exampleWindowsConfig = `{
  "cache_device": "D:\\bigcache.cache",
  "cache_size": "64G",
  "block_size": "64K",
  "listen": "127.0.0.1:10809",
  "control_listen": "127.0.0.1:10810",
  "flush_interval": "10s",
  "max_dirty_percent": 50,
  "flush_on_exit": true,
  "volumes": [
    {"name": "media", "device": "\\\\.\\PhysicalDrive1", "write_policy": "writeback"}
  ]
}
`

const onlineHint = "  (in Disk Management, right-click each disk and choose Online)"

// serviceStop stops the service if it is installed and running. It
// returns false when there was nothing to stop.
func serviceStop() (bool, error) {
	if !serviceInstalled() {
		return false, nil
	}
	m, err := mgr.Connect()
	if err != nil {
		return false, fmt.Errorf("connect to the service manager (run as Administrator): %w", err)
	}
	defer m.Disconnect()
	s, err := m.OpenService(serviceName)
	if err != nil {
		return false, nil
	}
	defer s.Close()
	st, err := s.Query()
	if err != nil {
		return false, err
	}
	if st.State == svc.Stopped {
		return false, nil
	}
	return true, stopService(s)
}

// serviceUninstall stops and deletes the service and its event log
// source. It returns false when the service was not installed.
func serviceUninstall() (bool, error) {
	if !serviceInstalled() {
		return false, nil
	}
	m, err := mgr.Connect()
	if err != nil {
		return false, fmt.Errorf("connect to the service manager (run as Administrator): %w", err)
	}
	defer m.Disconnect()
	s, err := m.OpenService(serviceName)
	if err != nil {
		return false, nil
	}
	defer s.Close()
	if st, err := s.Query(); err == nil && st.State != svc.Stopped {
		if err := stopService(s); err != nil {
			return false, err
		}
	}
	if err := s.Delete(); err != nil {
		return false, err
	}
	_ = eventlog.Remove(serviceName)
	return true, nil
}

// serviceInstalled checks for the service with a low-privilege handle, so
// that teardown on a portable installation does not need Administrator.
func serviceInstalled() bool {
	scm, err := windows.OpenSCManager(nil, nil, windows.SC_MANAGER_CONNECT)
	if err != nil {
		return true // cannot tell; let the privileged path report the real error
	}
	defer windows.CloseServiceHandle(scm)
	name, _ := windows.UTF16PtrFromString(serviceName)
	h, err := windows.OpenService(scm, name, windows.SERVICE_QUERY_STATUS)
	if err != nil {
		return !errors.Is(err, windows.ERROR_SERVICE_DOES_NOT_EXIST)
	}
	windows.CloseServiceHandle(h)
	return true
}
