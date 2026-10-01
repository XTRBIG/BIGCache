//go:build !windows

package main

func defaultConfigPath() string { return "/etc/bigcache/config.json" }
func defaultLogPath() string    { return "/var/log/bigcache.log" }

// attachDevice returns the device argument for "bigcache attach"; on Linux
// the /dev/nbdN device is mandatory.
func attachDevice(export, arg string) (string, bool) { return arg, arg != "" }

const attachUsage = "usage: bigcache attach NAME /dev/nbdN [--server ADDR]"
