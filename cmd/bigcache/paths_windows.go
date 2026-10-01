//go:build windows

package main

import (
	"os"
	"path/filepath"
)

func programData() string {
	if d := os.Getenv("ProgramData"); d != "" {
		return d
	}
	return `C:\ProgramData`
}

func defaultConfigPath() string { return filepath.Join(programData(), "BIGCache", "config.json") }
func defaultLogPath() string    { return filepath.Join(programData(), "BIGCache", "bigcache.log") }

// attachDevice returns the device argument for "bigcache attach": on
// Windows it is the WNBD instance name and defaults to the export name.
func attachDevice(export, arg string) (string, bool) {
	if arg == "" {
		return export, true
	}
	return arg, true
}

const attachUsage = "usage: bigcache attach NAME [INSTANCE] [--server ADDR]   (maps the export as a disk via WNBD)"
