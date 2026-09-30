package cache

import (
	"errors"
	"fmt"
	"sort"
	"time"
)

// flusherLoop writes dirty blocks back in the background: blocks older than
// FlushInterval on every tick, and the oldest blocks immediately when the
// dirty share exceeds MaxDirtyPercent.
func (c *Cache) flusherLoop() {
	defer c.wg.Done()
	tick := time.Second
	if c.opts.FlushInterval > 0 && c.opts.FlushInterval < tick {
		tick = c.opts.FlushInterval
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		select {
		case <-c.stopCh:
			return
		case <-c.flushWake:
			c.flushOverflow()
		case <-t.C:
			c.flushOverflow()
			if c.opts.FlushInterval > 0 {
				c.recordFlush(c.flushPass(0, c.opts.FlushInterval, 0))
			}
		}
	}
}

// flushOverflow brings the dirty share back under the watermark.
func (c *Cache) flushOverflow() {
	c.mu.Lock()
	dirty := len(c.dirtySet)
	limit := int(c.layout.SlotCount) * c.opts.MaxDirtyPercent / 100
	c.mu.Unlock()
	if dirty <= limit {
		return
	}
	// Flush down to half the watermark so that we do not run on every write.
	target := limit / 2
	c.recordFlush(c.flushPass(0, 0, dirty-target))
}

func (c *Cache) recordFlush(n int, err error) {
	c.mu.Lock()
	c.flushPasses++
	if err != nil {
		c.lastFlushErr = err.Error()
	} else {
		c.lastFlushErr = ""
	}
	c.mu.Unlock()
	if err != nil {
		c.log.Printf("cache: write-back: %v", err)
	}
}

// Flush writes every dirty block of every volume back to the HDDs.
func (c *Cache) Flush() (int, error) {
	return c.flushPass(0, 0, 0)
}

type flushItem struct {
	idx  uint32
	key  Key
	gen  uint64
	done bool
}

// flushPass writes dirty blocks back to their HDDs. vol limits the pass to
// one volume (0 = all), minAge skips blocks that became dirty more recently,
// limit caps the number of blocks (0 = all; the oldest are chosen first).
// Adjacent blocks are coalesced into single HDD writes. Blocks are only
// marked clean after the HDD has been synced, and only if no write touched
// them meanwhile.
func (c *Cache) flushPass(vol uint32, minAge time.Duration, limit int) (int, error) {
	c.flushMu.Lock()
	defer c.flushMu.Unlock()

	c.mu.Lock()
	cutoff := time.Now().Add(-minAge).UnixNano()
	items := make([]flushItem, 0, len(c.dirtySet))
	for idx := range c.dirtySet {
		s := &c.slots[idx]
		if vol != 0 && s.key.Vol != vol {
			continue
		}
		if minAge > 0 && s.dirtyAt > cutoff {
			continue
		}
		items = append(items, flushItem{idx: idx, key: s.key})
	}
	if limit > 0 && len(items) > limit {
		sort.Slice(items, func(i, j int) bool {
			return c.slots[items[i].idx].dirtyAt < c.slots[items[j].idx].dirtyAt
		})
		items = items[:limit]
	}
	for i := range items {
		c.slots[items[i].idx].refs++
		c.pinned++
	}
	c.mu.Unlock()
	if len(items) == 0 {
		return 0, nil
	}
	defer func() {
		c.mu.Lock()
		for i := range items {
			c.releaseLocked(items[i].idx)
		}
		c.mu.Unlock()
	}()

	sort.Slice(items, func(i, j int) bool {
		if items[i].key.Vol != items[j].key.Vol {
			return items[i].key.Vol < items[j].key.Vol
		}
		return items[i].key.Block < items[j].key.Block
	})

	var errs []error
	touched := map[uint32]*Volume{}
	maxRun := c.opts.MaxRunBytes / int(c.layout.BlockSize)
	if maxRun < 1 {
		maxRun = 1
	}
	for i := 0; i < len(items); {
		v := c.volume(items[i].key.Vol)
		if v == nil {
			i++
			continue
		}
		j := i + 1
		for j < len(items) && j-i < maxRun && items[j].key.Vol == items[i].key.Vol &&
			items[j].key.Block == items[j-1].key.Block+1 {
			j++
		}
		run := items[i:j]
		if err := c.flushRun(v, run); err != nil {
			if errors.Is(err, errRunChanged) && len(run) > 1 {
				for k := range run {
					if err := c.flushRun(v, run[k:k+1]); err != nil && !errors.Is(err, errRunChanged) {
						errs = append(errs, err)
					}
				}
			} else if !errors.Is(err, errRunChanged) {
				errs = append(errs, err)
			}
		}
		touched[v.id] = v
		i = j
	}

	synced := map[uint32]bool{}
	for id, v := range touched {
		if err := v.backend.Sync(); err != nil {
			errs = append(errs, fmt.Errorf("volume %s: sync HDD: %w", v.name, err))
			continue
		}
		synced[id] = true
	}

	flushed := 0
	for i := range items {
		it := &items[i]
		if !it.done || !synced[it.key.Vol] {
			continue
		}
		if c.markClean(it) {
			flushed++
		}
	}
	if len(errs) > 0 {
		c.stats.Errors.Add(int64(len(errs)))
		return flushed, errors.Join(errs...)
	}
	return flushed, nil
}

var errRunChanged = errors.New("run changed")

// flushRun writes a run of consecutive dirty blocks to the HDD with one
// write. It fails with errRunChanged when a slot is no longer dirty or
// bound to the expected key, so the caller can retry block by block.
func (c *Cache) flushRun(v *Volume, run []flushItem) error {
	bs := int(c.layout.BlockSize)
	for k := range run {
		c.slots[run[k].idx].mu.Lock()
	}
	defer func() {
		for k := len(run) - 1; k >= 0; k-- {
			c.slots[run[k].idx].mu.Unlock()
		}
	}()
	for k := range run {
		s := &c.slots[run[k].idx]
		if s.state != stateValid || !s.dirty || s.key != run[k].key {
			return errRunChanged
		}
	}
	var buf []byte
	if len(run) == 1 {
		buf = c.getBuf()
		defer c.putBuf(buf)
	} else {
		buf = make([]byte, len(run)*bs)
	}
	for k := range run {
		if err := v.ssdRead(buf[k*bs:(k+1)*bs], c.layout.dataOffset(run[k].idx)); err != nil {
			return fmt.Errorf("write-back of block %+v: %w", run[k].key, err)
		}
	}
	last := run[len(run)-1]
	total := (len(run)-1)*bs + v.blockLen(last.key.Block)
	if err := v.hddWrite(buf[:total], int64(run[0].key.Block)*int64(bs)); err != nil {
		return fmt.Errorf("write-back of %d block(s) from %+v: %w", len(run), run[0].key, err)
	}
	v.stats.FlushRuns.Add(1)
	c.stats.FlushRuns.Add(1)
	for k := range run {
		run[k].gen = c.slots[run[k].idx].writeGen
		run[k].done = true
	}
	return nil
}

// markClean clears the dirty flag of a flushed block unless it was written
// to again after the flush read it.
func (c *Cache) markClean(it *flushItem) bool {
	s := &c.slots[it.idx]
	s.mu.Lock()
	defer s.mu.Unlock()
	c.mu.Lock()
	if s.state != stateValid || s.key != it.key || !s.dirty || s.writeGen != it.gen {
		c.mu.Unlock()
		return false
	}
	s.dirty = false
	delete(c.dirtySet, it.idx)
	v := c.volumes[it.key.Vol]
	m := metaEntry{Vol: it.key.Vol, Flags: metaValid, Block: it.key.Block, CRC: s.crc, Stamp: s.stamp}
	c.mu.Unlock()
	if v != nil {
		v.dirty.Add(-1)
		v.stats.Flushed.Add(1)
	}
	c.stats.Flushed.Add(1)
	if err := c.writeMeta(it.idx, m); err != nil {
		c.log.Printf("cache: %v (block %+v will be written back again after a restart)", err, it.key)
	}
	return true
}

// flushSlot synchronously writes one dirty block back (eviction path). The
// caller holds a pin on the slot.
func (c *Cache) flushSlot(idx uint32) error {
	s := &c.slots[idx]
	s.mu.Lock()
	if s.state != stateValid || !s.dirty {
		s.mu.Unlock()
		return nil
	}
	key := s.key
	s.mu.Unlock()
	v := c.volume(key.Vol)
	if v == nil {
		return fmt.Errorf("dirty block %+v belongs to a detached volume", key)
	}
	run := []flushItem{{idx: idx, key: key}}
	if err := c.flushRun(v, run); err != nil {
		if errors.Is(err, errRunChanged) {
			return nil
		}
		return err
	}
	if err := v.backend.Sync(); err != nil {
		return fmt.Errorf("volume %s: sync HDD: %w", v.name, err)
	}
	c.markClean(&run[0])
	return nil
}
