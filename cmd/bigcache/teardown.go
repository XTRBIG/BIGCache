package main

import (
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/xtrbig/bigcache/internal/backend"
	"github.com/xtrbig/bigcache/internal/cache"
	"github.com/xtrbig/bigcache/internal/config"
	"github.com/xtrbig/bigcache/internal/nbd"
	"github.com/xtrbig/bigcache/internal/units"
)

// cmdTeardown undoes everything 'serve'/'service install'/'attach' set up,
// in the only order that is safe for the data:
//
//  1. detach the cached disks (so nothing writes through the cache anymore)
//  2. stop the service, which writes all dirty blocks back to the HDDs
//  3. verify on the cache device that no dirty blocks remain
//  4. deregister the service
//  5. optionally delete the cache file (or wipe the cache partition),
//     the configuration and the log
//
// It is what the Windows uninstaller and 'make uninstall' run. It refuses
// to continue while unwritten data exists unless --force is given.
func cmdTeardown(args []string) error {
	fs := flag.NewFlagSet("teardown", flag.ExitOnError)
	cfgPath := configFlag(fs)
	purgeCache := fs.Bool("purge-cache", false, "delete the cache file, or wipe the cache partition, after a clean flush")
	purgeConfig := fs.Bool("purge-config", false, "delete the configuration file, the log file and their directory")
	force := fs.Bool("force", false, "continue even if dirty blocks cannot be written back (DATA LOSS)")
	fs.Parse(args)

	cfg, err := config.Load(*cfgPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	hadConfig := cfg != nil
	if !hadConfig {
		fmt.Printf("no configuration at %s; only removing the service\n", *cfgPath)
	}

	// 1. Detach disks that are mapped through the cache.
	if hadConfig {
		for _, v := range cfg.Volumes {
			if nbd.Attached(v.Name) {
				fmt.Printf("detaching %s\n", v.Name)
				if err := nbd.Detach(v.Name); err != nil {
					if !*force {
						return fmt.Errorf("detach %s (unmount it first): %w", v.Name, err)
					}
					fmt.Printf("warning: %v\n", err)
				}
			}
		}
	}

	// 2. Stop the daemon: as a service when installed, otherwise refuse
	// while a foreground 'bigcache serve' is still running.
	stopped, err := serviceStop()
	if err != nil && !*force {
		return fmt.Errorf("stop service: %w", err)
	}
	if stopped {
		fmt.Println("service stopped (dirty blocks written back)")
	}
	if hadConfig && serveRunning(cfg) {
		if !*force {
			return fmt.Errorf("bigcache serve is still running on %s; stop it first", cfg.ControlListen)
		}
		fmt.Println("warning: a bigcache serve process is still running")
	}

	// 3. Verify the cache is clean before anything is removed.
	if hadConfig {
		if err := checkClean(cfg, *force); err != nil {
			return err
		}
	}

	// 4. Deregister the service.
	removed, err := serviceUninstall()
	if err != nil && !*force {
		return fmt.Errorf("remove service: %w", err)
	}
	if removed {
		fmt.Println("service removed")
	}

	// 5. Optional purges.
	if *purgeCache && hadConfig {
		if err := purgeCacheDevice(cfg, *force); err != nil {
			return err
		}
	}
	if *purgeConfig {
		for _, p := range []string{*cfgPath, defaultLogPath()} {
			if err := os.Remove(p); err == nil {
				fmt.Printf("removed %s\n", p)
			}
		}
		_ = os.Remove(filepath.Dir(*cfgPath)) // only succeeds when empty
	}
	if hadConfig {
		fmt.Println("the HDDs can now be used directly again:")
		for _, v := range cfg.Volumes {
			fmt.Printf("  %-16s %s\n", v.Name, v.Device)
		}
		fmt.Println(onlineHint)
	}
	fmt.Println("teardown complete")
	return nil
}

func serveRunning(cfg *config.Config) bool {
	c := &http.Client{Timeout: 500 * time.Millisecond}
	resp, err := c.Get("http://" + cfg.ControlListen + "/healthz")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// checkClean inspects the cache device and fails when dirty blocks remain.
func checkClean(cfg *config.Config, force bool) error {
	dev, err := backend.OpenFile(cfg.CacheDevice, true)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if force {
			fmt.Printf("warning: cannot open cache device: %v\n", err)
			return nil
		}
		return fmt.Errorf("open cache device: %w", err)
	}
	defer dev.Close()
	info, err := cache.Inspect(dev)
	if err != nil {
		if errors.Is(err, cache.ErrNotFormatted) || force {
			return nil
		}
		return err
	}
	if info.DirtySlots > 0 {
		msg := fmt.Sprintf("%d dirty blocks (%s) on %s have not been written to the HDDs", info.DirtySlots,
			units.FormatSize(int64(info.DirtySlots)*int64(info.BlockSize)), cfg.CacheDevice)
		if !force {
			return fmt.Errorf("%s; start bigcache, stop it cleanly and retry, or use --force to discard them", msg)
		}
		fmt.Printf("WARNING: %s and are being discarded\n", msg)
	}
	if !info.Clean && !force {
		return fmt.Errorf("%s was not shut down cleanly; start bigcache, stop it cleanly and retry, or use --force", cfg.CacheDevice)
	}
	return nil
}

func purgeCacheDevice(cfg *config.Config, force bool) error {
	st, err := os.Stat(cfg.CacheDevice)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if st.Mode().IsRegular() {
		if err := os.Remove(cfg.CacheDevice); err != nil {
			return err
		}
		fmt.Printf("deleted cache file %s (%s)\n", cfg.CacheDevice, units.FormatSize(st.Size()))
		return nil
	}
	dev, err := backend.OpenFile(cfg.CacheDevice, false)
	if err != nil {
		return err
	}
	defer dev.Close()
	if err := cache.Wipe(dev, force); err != nil {
		return err
	}
	fmt.Printf("wiped cache signature on %s\n", cfg.CacheDevice)
	return nil
}
