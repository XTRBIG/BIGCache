// Package config loads the BIGCache JSON configuration file.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/xtrbig/bigcache/internal/units"
)

// Defaults.
const (
	DefaultBlockSize       = "64K"
	DefaultListen          = "127.0.0.1:10809"
	DefaultControlListen   = "127.0.0.1:10810"
	DefaultFlushInterval   = "10s"
	DefaultMaxDirtyPercent = 50
	MinBlockSize           = 4 << 10
	MaxBlockSize           = 16 << 20
)

// Config is the on-disk configuration.
type Config struct {
	// CacheDevice is the SSD block device or file that holds the cache.
	CacheDevice string `json:"cache_device"`
	// CacheSize is only used by "init" when CacheDevice is a regular file
	// that does not exist yet.
	CacheSize string `json:"cache_size,omitempty"`
	// BlockSize is the cache block size (power of two, 4K..16M).
	BlockSize string `json:"block_size,omitempty"`
	// Listen is the NBD server address.
	Listen string `json:"listen,omitempty"`
	// ControlListen is the HTTP control/statistics API address.
	ControlListen string `json:"control_listen,omitempty"`
	// FlushInterval is how long dirty blocks may stay on the SSD before the
	// background flusher writes them to the HDD (deferred write latency).
	FlushInterval string `json:"flush_interval,omitempty"`
	// MaxDirtyPercent is the share of cache blocks that may be dirty before
	// the flusher starts writing them back regardless of their age.
	MaxDirtyPercent int `json:"max_dirty_percent,omitempty"`
	// DurableWrites fsyncs the SSD after every write-back write so that an
	// acknowledged write survives a power loss. Slower; off by default because
	// an NBD FLUSH (issued by journaling filesystems at barriers) already
	// makes everything durable.
	DurableWrites bool `json:"durable_writes"`
	// VerifyReads checks the CRC of every block served from the SSD.
	VerifyReads bool `json:"verify_reads"`
	// FlushOnExit writes all dirty blocks to the HDDs on shutdown (default
	// true). When false dirty blocks stay on the SSD and are written back
	// after the next start.
	FlushOnExit *bool `json:"flush_on_exit,omitempty"`
	// Volumes are the HDD devices to cache.
	Volumes []Volume `json:"volumes"`
}

// Volume describes one cached HDD device.
type Volume struct {
	// Name is the stable identifier of the volume and the NBD export name.
	Name string `json:"name"`
	// Device is the HDD block device or file.
	Device string `json:"device"`
	// WritePolicy is "writeback" (default), "writethrough" or "none".
	WritePolicy string `json:"write_policy,omitempty"`
	// ReadCache populates the cache on read misses (default true).
	ReadCache *bool `json:"read_cache,omitempty"`
	// ReadOnly exports the volume read-only and never writes to the HDD.
	ReadOnly bool `json:"read_only,omitempty"`
}

// Load reads, validates and applies defaults to a configuration file.
func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, nil
}

// Validate checks the configuration and fills in defaults.
func (c *Config) Validate() error {
	if c.CacheDevice == "" {
		return fmt.Errorf("cache_device is required")
	}
	if c.BlockSize == "" {
		c.BlockSize = DefaultBlockSize
	}
	bs, err := units.ParseSize(c.BlockSize)
	if err != nil {
		return fmt.Errorf("block_size: %w", err)
	}
	if bs < MinBlockSize || bs > MaxBlockSize || bs&(bs-1) != 0 {
		return fmt.Errorf("block_size must be a power of two between 4K and 16M, got %s", c.BlockSize)
	}
	if c.CacheSize != "" {
		if _, err := units.ParseSize(c.CacheSize); err != nil {
			return fmt.Errorf("cache_size: %w", err)
		}
	}
	if c.Listen == "" {
		c.Listen = DefaultListen
	}
	if c.ControlListen == "" {
		c.ControlListen = DefaultControlListen
	}
	if c.FlushInterval == "" {
		c.FlushInterval = DefaultFlushInterval
	}
	if d, err := time.ParseDuration(c.FlushInterval); err != nil || d < 0 {
		return fmt.Errorf("flush_interval: invalid duration %q", c.FlushInterval)
	}
	if c.MaxDirtyPercent == 0 {
		c.MaxDirtyPercent = DefaultMaxDirtyPercent
	}
	if c.MaxDirtyPercent < 1 || c.MaxDirtyPercent > 100 {
		return fmt.Errorf("max_dirty_percent must be between 1 and 100")
	}
	if c.FlushOnExit == nil {
		t := true
		c.FlushOnExit = &t
	}
	if len(c.Volumes) == 0 {
		return fmt.Errorf("at least one volume is required")
	}
	seen := map[string]bool{}
	for i := range c.Volumes {
		v := &c.Volumes[i]
		if v.Name == "" {
			return fmt.Errorf("volumes[%d]: name is required", i)
		}
		if strings.ContainsAny(v.Name, "/\\ \t\n") {
			return fmt.Errorf("volumes[%d]: name %q must not contain whitespace or slashes", i, v.Name)
		}
		if seen[v.Name] {
			return fmt.Errorf("volumes[%d]: duplicate name %q", i, v.Name)
		}
		seen[v.Name] = true
		if v.Device == "" {
			return fmt.Errorf("volume %q: device is required", v.Name)
		}
		if v.Device == c.CacheDevice {
			return fmt.Errorf("volume %q: device must differ from cache_device", v.Name)
		}
		if v.WritePolicy == "" {
			v.WritePolicy = "writeback"
		}
		switch v.WritePolicy {
		case "writeback", "writethrough", "none":
		default:
			return fmt.Errorf("volume %q: write_policy must be writeback, writethrough or none", v.Name)
		}
		if v.ReadCache == nil {
			t := true
			v.ReadCache = &t
		}
	}
	return nil
}

// BlockSizeBytes returns the validated block size.
func (c *Config) BlockSizeBytes() uint32 {
	bs, _ := units.ParseSize(c.BlockSize)
	return uint32(bs)
}

// CacheSizeBytes returns cache_size in bytes (0 when unset).
func (c *Config) CacheSizeBytes() int64 {
	if c.CacheSize == "" {
		return 0
	}
	n, _ := units.ParseSize(c.CacheSize)
	return n
}

// FlushIntervalDuration returns the validated flush interval.
func (c *Config) FlushIntervalDuration() time.Duration {
	d, _ := time.ParseDuration(c.FlushInterval)
	return d
}

// Volume returns the volume with the given name, or nil.
func (c *Config) Volume(name string) *Volume {
	for i := range c.Volumes {
		if c.Volumes[i].Name == name {
			return &c.Volumes[i]
		}
	}
	return nil
}
