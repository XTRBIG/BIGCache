package cache

import (
	"fmt"
	"hash/crc32"
	"time"

	"github.com/xtrbig/bigcache/internal/backend"
)

// WritePolicy selects how writes are cached.
type WritePolicy int

const (
	// PolicyWriteBack acknowledges writes once they are on the SSD and
	// writes them to the HDD later ("deferred write").
	PolicyWriteBack WritePolicy = iota
	// PolicyWriteThrough writes to the HDD and keeps a copy on the SSD.
	PolicyWriteThrough
	// PolicyNone writes to the HDD only; an existing cached copy is updated
	// so that reads stay coherent, but writes never allocate cache space.
	PolicyNone
)

// ParsePolicy converts the configuration string.
func ParsePolicy(s string) (WritePolicy, error) {
	switch s {
	case "", "writeback", "write-back", "wb":
		return PolicyWriteBack, nil
	case "writethrough", "write-through", "wt":
		return PolicyWriteThrough, nil
	case "none", "readonly", "read-only":
		return PolicyNone, nil
	}
	return 0, fmt.Errorf("unknown write policy %q", s)
}

func (p WritePolicy) String() string {
	switch p {
	case PolicyWriteBack:
		return "writeback"
	case PolicyWriteThrough:
		return "writethrough"
	default:
		return "none"
	}
}

// VolumeConfig describes a volume to attach.
type VolumeConfig struct {
	Name      string
	Device    string // informational
	Backend   backend.Backend
	Policy    WritePolicy
	ReadCache bool
	ReadOnly  bool
}

// Volume is a cached view of one HDD device. It implements io.ReaderAt and
// io.WriterAt with strict bounds: requests must lie inside the volume.
type Volume struct {
	c         *Cache
	id        uint32
	name      string
	device    string
	backend   backend.Backend
	size      int64
	policy    WritePolicy
	readCache bool
	readOnly  bool
	stats     counters
	cached    atomicInt64
	dirty     atomicInt64
}

func (v *Volume) Name() string             { return v.name }
func (v *Volume) ID() uint32               { return v.id }
func (v *Volume) Size() int64              { return v.size }
func (v *Volume) ReadOnly() bool           { return v.readOnly }
func (v *Volume) Policy() WritePolicy      { return v.policy }
func (v *Volume) Backend() backend.Backend { return v.backend }

// Stats returns the per-volume statistics.
func (v *Volume) Stats() VolumeStats {
	return VolumeStats{
		Name:         v.name,
		ID:           v.id,
		Device:       v.device,
		Size:         v.size,
		WritePolicy:  v.policy.String(),
		ReadCache:    v.readCache,
		ReadOnly:     v.readOnly,
		CachedBlocks: v.cached.Load(),
		DirtyBlocks:  v.dirty.Load(),
		Counters:     v.stats.snapshot(),
	}
}

func (v *Volume) blockLen(blk uint64) int {
	bs := int64(v.c.layout.BlockSize)
	rem := v.size - int64(blk)*bs
	if rem >= bs {
		return int(bs)
	}
	return int(rem)
}

func (v *Volume) checkRange(p []byte, off int64) error {
	if off < 0 || off > v.size || int64(len(p)) > v.size-off {
		return fmt.Errorf("%w: [%d,%d) of %d", ErrOutOfRange, off, off+int64(len(p)), v.size)
	}
	return nil
}

// ReadAt reads len(p) bytes at off through the cache.
func (v *Volume) ReadAt(p []byte, off int64) (int, error) {
	if err := v.checkRange(p, off); err != nil {
		return 0, err
	}
	bs := int64(v.c.layout.BlockSize)
	done := 0
	for done < len(p) {
		cur := off + int64(done)
		blk := uint64(cur / bs)
		inBlk := int(cur % bs)
		n := int(bs) - inBlk
		if n > len(p)-done {
			n = len(p) - done
		}
		if err := v.readBlock(blk, inBlk, p[done:done+n]); err != nil {
			return done, err
		}
		done += n
	}
	v.stats.BytesRead.Add(int64(done))
	v.c.stats.BytesRead.Add(int64(done))
	return done, nil
}

// WriteAt writes p at off according to the volume's write policy.
func (v *Volume) WriteAt(p []byte, off int64) (int, error) {
	if v.readOnly {
		return 0, ErrReadOnly
	}
	if err := v.checkRange(p, off); err != nil {
		return 0, err
	}
	bs := int64(v.c.layout.BlockSize)
	done := 0
	for done < len(p) {
		cur := off + int64(done)
		blk := uint64(cur / bs)
		inBlk := int(cur % bs)
		n := int(bs) - inBlk
		if n > len(p)-done {
			n = len(p) - done
		}
		if err := v.writeBlock(blk, inBlk, p[done:done+n]); err != nil {
			return done, err
		}
		done += n
	}
	v.stats.BytesWritten.Add(int64(done))
	v.c.stats.BytesWritten.Add(int64(done))
	return done, nil
}

// Sync makes all completed writes durable: for write-back volumes the
// dirty data only has to reach the SSD; for the other policies the HDD.
func (v *Volume) Sync() error {
	if v.readOnly {
		return nil
	}
	if v.policy == PolicyWriteBack {
		if err := v.c.dev.Sync(); err != nil {
			return err
		}
	}
	return v.backend.Sync()
}

// Flush writes every dirty block of the volume back to the HDD.
func (v *Volume) Flush() (int, error) {
	return v.c.flushPass(v.id, 0, 0)
}

func (v *Volume) hddRead(p []byte, off int64) error {
	_, err := v.backend.ReadAt(p, off)
	if err != nil {
		v.stats.Errors.Add(1)
		v.c.stats.Errors.Add(1)
		return fmt.Errorf("volume %s: read HDD at %d: %w", v.name, off, err)
	}
	v.stats.HDDBytesRead.Add(int64(len(p)))
	v.c.stats.HDDBytesRead.Add(int64(len(p)))
	return nil
}

func (v *Volume) hddWrite(p []byte, off int64) error {
	_, err := v.backend.WriteAt(p, off)
	if err != nil {
		v.stats.Errors.Add(1)
		v.c.stats.Errors.Add(1)
		return fmt.Errorf("volume %s: write HDD at %d: %w", v.name, off, err)
	}
	v.stats.HDDBytesWrite.Add(int64(len(p)))
	v.c.stats.HDDBytesWrite.Add(int64(len(p)))
	return nil
}

func (v *Volume) ssdRead(p []byte, off int64) error {
	if _, err := v.c.dev.ReadAt(p, off); err != nil {
		v.c.stats.Errors.Add(1)
		return fmt.Errorf("read cache device at %d: %w", off, err)
	}
	v.c.stats.SSDBytesRead.Add(int64(len(p)))
	return nil
}

func (v *Volume) ssdWrite(p []byte, off int64) error {
	if _, err := v.c.dev.WriteAt(p, off); err != nil {
		v.c.stats.Errors.Add(1)
		return fmt.Errorf("write cache device at %d: %w", off, err)
	}
	v.c.stats.SSDBytesWrite.Add(int64(len(p)))
	return nil
}

// lockBound pins the slot for key (allocating when alloc is set), locks it
// and verifies that it is still bound to key. It returns nilIdx when the
// caller should bypass the cache.
func (v *Volume) lockBound(key Key, alloc bool) uint32 {
	c := v.c
	for {
		idx := c.acquire(key, alloc)
		if idx == nilIdx {
			return nilIdx
		}
		s := &c.slots[idx]
		s.mu.Lock()
		if s.state != stateFree && s.key == key {
			return idx
		}
		// The slot was unbound while we waited for it; start over.
		s.mu.Unlock()
		c.release(idx)
	}
}

func (v *Volume) readBlock(blk uint64, inBlk int, p []byte) error {
	c := v.c
	bs := c.layout.BlockSize
	key := Key{Vol: v.id, Block: blk}
	off := int64(blk)*int64(bs) + int64(inBlk)
	v.stats.Reads.Add(1)
	c.stats.Reads.Add(1)

	idx := v.lockBound(key, v.readCache)
	if idx == nilIdx {
		v.stats.ReadMisses.Add(1)
		c.stats.ReadMisses.Add(1)
		return v.hddRead(p, off)
	}
	s := &c.slots[idx]
	defer c.release(idx)
	defer s.mu.Unlock()
	dataOff := c.layout.dataOffset(idx)

	if s.state == stateValid {
		v.stats.ReadHits.Add(1)
		c.stats.ReadHits.Add(1)
		var err error
		if c.opts.VerifyReads {
			buf := c.getBuf()
			defer c.putBuf(buf)
			if err = v.ssdRead(buf, dataOff); err == nil {
				if crc32.ChecksumIEEE(buf) != s.crc {
					err = ErrChecksum
				} else {
					copy(p, buf[inBlk:])
				}
			}
		} else {
			err = v.ssdRead(p, dataOff+int64(inBlk))
		}
		if err == nil {
			return nil
		}
		if s.dirty {
			c.log.Printf("cache: %v (dirty block %+v cannot be recovered from the HDD)", err, key)
			return err
		}
		// The HDD still holds the data: drop the bad copy and read it there.
		c.log.Printf("cache: %v; falling back to HDD for %+v", err, key)
		c.unbind(idx)
		_ = c.writeMeta(idx, metaEntry{})
		return v.hddRead(p, off)
	}

	// Miss: we own the loading slot. Fill it from the HDD.
	v.stats.ReadMisses.Add(1)
	c.stats.ReadMisses.Add(1)
	buf := c.getBuf()
	defer c.putBuf(buf)
	blen := v.blockLen(blk)
	if err := v.hddRead(buf[:blen], int64(blk)*int64(bs)); err != nil {
		c.unbind(idx)
		return err
	}
	clear(buf[blen:])
	copy(p, buf[inBlk:])
	if err := v.populate(idx, s, key, buf, false); err != nil {
		// Serve the read anyway; the block simply is not cached.
		c.log.Printf("cache: %v; block %+v not cached", err, key)
		c.unbind(idx)
	}
	return nil
}

// populate stores a full block in a loading slot. Caller holds slot.mu.
func (v *Volume) populate(idx uint32, s *slot, key Key, buf []byte, dirty bool) error {
	c := v.c
	if err := v.ssdWrite(buf, c.layout.dataOffset(idx)); err != nil {
		return err
	}
	crc := crc32.ChecksumIEEE(buf)
	c.mu.Lock()
	stamp := s.stamp
	c.mu.Unlock()
	m := metaEntry{Vol: key.Vol, Flags: metaValid, Block: key.Block, CRC: crc, Stamp: stamp}
	if dirty {
		m.Flags |= metaDirty
	}
	if err := c.writeMeta(idx, m); err != nil {
		return err
	}
	c.mu.Lock()
	s.state = stateValid
	s.crc = crc
	v.cached.Add(1)
	if dirty {
		c.markDirtyLocked(idx, s, v)
	}
	c.mu.Unlock()
	return nil
}

// markDirtyLocked flags a slot dirty. Caller holds c.mu.
func (c *Cache) markDirtyLocked(idx uint32, s *slot, v *Volume) {
	if !s.dirty {
		s.dirty = true
		s.dirtyAt = time.Now().UnixNano()
		c.dirtySet[idx] = struct{}{}
		v.dirty.Add(1)
	}
	if len(c.dirtySet)*100 > int(c.layout.SlotCount)*c.opts.MaxDirtyPercent {
		select {
		case c.flushWake <- struct{}{}:
		default:
		}
	}
}

func (v *Volume) writeBlock(blk uint64, inBlk int, p []byte) error {
	c := v.c
	bs := c.layout.BlockSize
	key := Key{Vol: v.id, Block: blk}
	off := int64(blk)*int64(bs) + int64(inBlk)
	blkOff := int64(blk) * int64(bs)
	v.stats.Writes.Add(1)
	c.stats.Writes.Add(1)

	idx := v.lockBound(key, v.policy != PolicyNone)
	if idx == nilIdx {
		v.stats.WriteBypass.Add(1)
		c.stats.WriteBypass.Add(1)
		return v.hddWrite(p, off)
	}
	s := &c.slots[idx]
	defer c.release(idx)
	defer s.mu.Unlock()
	dataOff := c.layout.dataOffset(idx)
	blen := v.blockLen(blk)
	full := inBlk == 0 && len(p) == blen
	loading := s.state == stateLoading
	if loading {
		v.stats.WriteMisses.Add(1)
		c.stats.WriteMisses.Add(1)
	} else {
		v.stats.WriteHits.Add(1)
		c.stats.WriteHits.Add(1)
	}

	// Build the full block image when we need it: always when the slot is
	// being filled, and for partial writes to a valid slot (to recompute
	// the CRC; only the changed range is written to the SSD in that case).
	var buf []byte
	if loading || !full {
		buf = c.getBuf()
		defer c.putBuf(buf)
		if loading {
			if full {
				copy(buf, p)
			} else if err := v.hddRead(buf[:blen], blkOff); err != nil {
				c.unbind(idx)
				return err
			}
		} else if err := v.ssdRead(buf, dataOff); err != nil {
			if s.dirty {
				return err
			}
			// Clean copy unreadable: drop it and write straight to the HDD.
			c.unbind(idx)
			_ = c.writeMeta(idx, metaEntry{})
			v.stats.WriteBypass.Add(1)
			return v.hddWrite(p, off)
		}
		clear(buf[blen:])
		copy(buf[inBlk:], p)
	}

	// Write-through and no-write-cache policies write to the HDD first: the
	// HDD stays the source of truth and the SSD copy is only a mirror.
	if v.policy != PolicyWriteBack {
		if err := v.hddWrite(p, off); err != nil {
			if loading {
				c.unbind(idx)
			}
			return err
		}
	}

	if loading {
		if err := v.populate(idx, s, key, buf, v.policy == PolicyWriteBack); err != nil {
			c.log.Printf("cache: %v; block %+v not cached", err, key)
			c.unbind(idx)
			if v.policy == PolicyWriteBack {
				// The write was not persisted anywhere yet.
				return v.hddWrite(p, off)
			}
			return nil
		}
		if v.policy == PolicyWriteBack && c.opts.DurableWrites {
			return c.dev.Sync()
		}
		return nil
	}

	// Update a valid slot.
	var crc uint32
	var werr error
	if full {
		crc = blockCRC(p, bs)
		werr = v.ssdWrite(p, dataOff)
	} else {
		crc = crc32.ChecksumIEEE(buf)
		werr = v.ssdWrite(p, dataOff+int64(inBlk))
	}
	if werr != nil {
		// The SSD copy is now undefined. Push the merged image to the HDD so
		// no data is lost, then forget the slot.
		c.log.Printf("cache: %v; writing block %+v through to the HDD", werr, key)
		img := p
		if !full {
			img = buf[:blen]
		}
		wasDirty := s.dirty
		c.unbind(idx)
		_ = c.writeMeta(idx, metaEntry{})
		if v.policy == PolicyWriteBack || wasDirty {
			return v.hddWrite(img, blkOff)
		}
		return nil
	}
	c.mu.Lock()
	stamp := s.stamp
	c.mu.Unlock()
	m := metaEntry{Vol: key.Vol, Flags: metaValid, Block: key.Block, CRC: crc, Stamp: stamp}
	dirty := v.policy == PolicyWriteBack || s.dirty
	if dirty {
		m.Flags |= metaDirty
	}
	if err := c.writeMeta(idx, m); err != nil {
		c.log.Printf("cache: %v; writing block %+v through to the HDD", err, key)
		img := p
		if !full {
			img = buf[:blen]
		}
		c.unbind(idx)
		return v.hddWrite(img, blkOff)
	}
	c.mu.Lock()
	s.crc = crc
	s.writeGen++
	if dirty {
		c.markDirtyLocked(idx, s, v)
	}
	c.mu.Unlock()
	if v.policy == PolicyWriteBack && c.opts.DurableWrites {
		return c.dev.Sync()
	}
	return nil
}
