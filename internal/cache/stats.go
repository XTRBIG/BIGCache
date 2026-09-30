package cache

import "sync/atomic"

// counters holds hot-path counters updated with atomics.
type counters struct {
	Reads         atomic.Int64 // read requests (blocks)
	ReadHits      atomic.Int64 // blocks served from the SSD
	ReadMisses    atomic.Int64 // blocks read from the HDD
	Writes        atomic.Int64 // write requests (blocks)
	WriteHits     atomic.Int64 // writes to a block already in cache
	WriteMisses   atomic.Int64 // writes that allocated a new cache block
	WriteBypass   atomic.Int64 // writes that went straight to the HDD
	BytesRead     atomic.Int64 // bytes returned to clients
	BytesWritten  atomic.Int64 // bytes accepted from clients
	HDDBytesRead  atomic.Int64 // bytes read from the HDD
	HDDBytesWrite atomic.Int64 // bytes written to the HDD
	SSDBytesRead  atomic.Int64
	SSDBytesWrite atomic.Int64
	Flushed       atomic.Int64 // dirty blocks written back
	FlushRuns     atomic.Int64 // coalesced write-back operations
	Evictions     atomic.Int64
	Errors        atomic.Int64
}

// Snapshot is a point-in-time copy of counters, suitable for JSON.
type Snapshot struct {
	Reads         int64   `json:"reads"`
	ReadHits      int64   `json:"read_hits"`
	ReadMisses    int64   `json:"read_misses"`
	ReadHitRatio  float64 `json:"read_hit_ratio"`
	Writes        int64   `json:"writes"`
	WriteHits     int64   `json:"write_hits"`
	WriteMisses   int64   `json:"write_misses"`
	WriteBypass   int64   `json:"write_bypass"`
	BytesRead     int64   `json:"bytes_read"`
	BytesWritten  int64   `json:"bytes_written"`
	HDDBytesRead  int64   `json:"hdd_bytes_read"`
	HDDBytesWrite int64   `json:"hdd_bytes_written"`
	SSDBytesRead  int64   `json:"ssd_bytes_read"`
	SSDBytesWrite int64   `json:"ssd_bytes_written"`
	Flushed       int64   `json:"flushed_blocks"`
	FlushRuns     int64   `json:"flush_runs"`
	Evictions     int64   `json:"evictions"`
	Errors        int64   `json:"errors"`
}

func (c *counters) snapshot() Snapshot {
	s := Snapshot{
		Reads:         c.Reads.Load(),
		ReadHits:      c.ReadHits.Load(),
		ReadMisses:    c.ReadMisses.Load(),
		Writes:        c.Writes.Load(),
		WriteHits:     c.WriteHits.Load(),
		WriteMisses:   c.WriteMisses.Load(),
		WriteBypass:   c.WriteBypass.Load(),
		BytesRead:     c.BytesRead.Load(),
		BytesWritten:  c.BytesWritten.Load(),
		HDDBytesRead:  c.HDDBytesRead.Load(),
		HDDBytesWrite: c.HDDBytesWrite.Load(),
		SSDBytesRead:  c.SSDBytesRead.Load(),
		SSDBytesWrite: c.SSDBytesWrite.Load(),
		Flushed:       c.Flushed.Load(),
		FlushRuns:     c.FlushRuns.Load(),
		Evictions:     c.Evictions.Load(),
		Errors:        c.Errors.Load(),
	}
	if s.Reads > 0 {
		s.ReadHitRatio = float64(s.ReadHits) / float64(s.Reads)
	}
	return s
}

// VolumeStats is the per-volume view exposed by the API.
type VolumeStats struct {
	Name         string   `json:"name"`
	ID           uint32   `json:"id"`
	Device       string   `json:"device"`
	Size         int64    `json:"size"`
	WritePolicy  string   `json:"write_policy"`
	ReadCache    bool     `json:"read_cache"`
	ReadOnly     bool     `json:"read_only"`
	CachedBlocks int64    `json:"cached_blocks"`
	DirtyBlocks  int64    `json:"dirty_blocks"`
	Counters     Snapshot `json:"counters"`
}

// CacheStats is the global view exposed by the API.
type CacheStats struct {
	BlockSize    uint32        `json:"block_size"`
	Slots        uint64        `json:"slots"`
	UsedSlots    int64         `json:"used_slots"`
	DirtySlots   int64         `json:"dirty_slots"`
	CacheSize    int64         `json:"cache_size"`
	UsedBytes    int64         `json:"used_bytes"`
	DirtyBytes   int64         `json:"dirty_bytes"`
	Counters     Snapshot      `json:"counters"`
	Volumes      []VolumeStats `json:"volumes"`
	UptimeSecs   float64       `json:"uptime_seconds"`
	FlushPasses  int64         `json:"flush_passes"`
	LastFlushErr string        `json:"last_flush_error,omitempty"`
}
