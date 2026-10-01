// Package cache implements a persistent block cache that uses one fast
// device (an SSD) to cache reads and writes for several slow devices (HDDs),
// in the spirit of PrimoCache's level-2 cache.
//
// The cache device is divided into fixed-size slots. An in-memory index maps
// (volume, block) keys to slots and an LRU list orders them for eviction.
// Every slot has a persisted metadata entry, so the cache survives restarts:
// clean blocks are still hits after a clean shutdown and dirty (write-back)
// blocks are recovered and written to the HDD even after a crash.
package cache

import (
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"log"
	"sort"
	"sync"
	"time"

	"github.com/xtrbig/bigcache/internal/backend"
)

const (
	nilIdx     = ^uint32(0)
	maxLRUScan = 64 // how far from the LRU tail we look for a clean victim before flushing one
)

// Errors.
var (
	ErrClosed       = errors.New("cache is closed")
	ErrReadOnly     = errors.New("volume is read-only")
	ErrOutOfRange   = errors.New("request outside the volume")
	ErrOrphanDirty  = errors.New("cache holds dirty blocks for volumes that are not configured")
	ErrChecksum     = errors.New("cached block failed checksum verification")
	ErrVolumeExists = errors.New("volume already attached")
)

// Options tune the cache engine.
type Options struct {
	// FlushInterval is how long a dirty block may age before the background
	// flusher writes it back. Zero disables the age-based flush (dirty blocks
	// are still flushed when MaxDirtyPercent is exceeded, on eviction, on
	// Flush and on Close).
	FlushInterval time.Duration
	// MaxDirtyPercent is the share of slots that may be dirty before the
	// flusher writes the oldest ones back regardless of age.
	MaxDirtyPercent int
	// DurableWrites fsyncs the cache device after each write-back write.
	DurableWrites bool
	// VerifyReads validates the CRC of every block served from the cache.
	VerifyReads bool
	// KeepDirtyOnClose skips the write-back of dirty blocks on Close; they
	// stay on the cache device and are written back after the next Start.
	KeepDirtyOnClose bool
	// DiscardOrphanDirty drops dirty blocks that belong to volumes that were
	// not attached, instead of refusing to start.
	DiscardOrphanDirty bool
	// MaxRunBytes caps the size of coalesced write-back writes.
	MaxRunBytes int
	// Logger receives warnings; nil uses the standard logger.
	Logger *log.Logger
}

func (o *Options) defaults() {
	if o.MaxDirtyPercent <= 0 || o.MaxDirtyPercent > 100 {
		o.MaxDirtyPercent = 50
	}
	if o.MaxRunBytes <= 0 {
		o.MaxRunBytes = 4 << 20
	}
	if o.Logger == nil {
		o.Logger = log.Default()
	}
}

// Key identifies a block of a volume.
type Key struct {
	Vol   uint32
	Block uint64
}

type slotState uint8

const (
	stateFree    slotState = iota // not bound to a key
	stateLoading                  // bound; data not yet on the SSD
	stateValid                    // bound; data on the SSD
)

// slot is the in-memory state of one cache slot.
//
// Locking: key, state, dirty, crc, refs, stamp and the LRU links are written
// only while holding Cache.mu. A goroutine that has pinned a slot (refs > 0)
// and holds slot.mu may read key/state/dirty without Cache.mu, because the
// slot cannot be rebound while pinned and unbinding is only done under
// slot.mu. slot.mu serialises all data I/O on the slot.
type slot struct {
	mu       sync.Mutex
	key      Key
	prev     uint32
	next     uint32
	refs     int32
	state    slotState
	dirty    bool
	inLRU    bool
	crc      uint32
	stamp    uint64 // access counter, persisted for LRU order across restarts
	writeGen uint64 // incremented on every write; used by the flusher
	dirtyAt  int64  // unix nanoseconds when the slot became dirty
}

// Cache is the shared SSD cache.
type Cache struct {
	dev    backend.Backend
	layout Layout
	opts   Options
	log    *log.Logger

	mu        sync.Mutex
	cond      *sync.Cond // broadcast when a pin is released
	slots     []slot
	index     map[Key]uint32
	lruHead   uint32 // most recently used
	lruTail   uint32 // least recently used
	free      []uint32
	dirtySet  map[uint32]struct{}
	tick      uint64
	pinned    int64
	volumes   map[uint32]*Volume
	volByName map[string]*Volume
	table     *volumeTable
	started   bool
	closed    bool
	sbCreated int64

	flushMu      sync.Mutex
	flushWake    chan struct{}
	stopCh       chan struct{}
	wg           sync.WaitGroup
	flushPasses  int64
	lastFlushErr string

	metaLocks [256]sync.Mutex // striped by metadata sector

	bufPool sync.Pool
	stats   counters
	opened  time.Time
	dropped RecoveryReport
}

// RecoveryReport summarises what Open found on the cache device.
type RecoveryReport struct {
	CleanShutdown bool
	Restored      uint64 // valid entries restored into the index
	RestoredDirty uint64 // of which dirty
	DroppedClean  uint64 // clean entries dropped after an unclean shutdown
	Corrupt       uint64 // dirty entries whose data failed the CRC check
	Duplicates    uint64
}

// Open loads the cache from a formatted device. Volumes must then be
// attached with AttachVolume and the engine started with Start.
func Open(dev backend.Backend, opts Options) (*Cache, error) {
	opts.defaults()
	sb, err := readSuperblock(dev)
	if err != nil {
		return nil, err
	}
	if sb.DeviceSize > dev.Size() {
		return nil, fmt.Errorf("cache device shrank: formatted for %d bytes, now %d", sb.DeviceSize, dev.Size())
	}
	table, err := readTable(dev, sb.Layout)
	if err != nil {
		return nil, err
	}
	c := &Cache{
		dev:       dev,
		layout:    sb.Layout,
		opts:      opts,
		log:       opts.Logger,
		slots:     make([]slot, sb.SlotCount),
		index:     make(map[Key]uint32),
		lruHead:   nilIdx,
		lruTail:   nilIdx,
		dirtySet:  make(map[uint32]struct{}),
		volumes:   make(map[uint32]*Volume),
		volByName: make(map[string]*Volume),
		table:     table,
		flushWake: make(chan struct{}, 1),
		stopCh:    make(chan struct{}),
		opened:    time.Now(),
		sbCreated: sb.Created,
	}
	c.cond = sync.NewCond(&c.mu)
	bs := int(c.layout.BlockSize)
	c.bufPool.New = func() any { b := make([]byte, bs); return &b }

	if err := c.load(sb.Flags&sbFlagClean != 0); err != nil {
		return nil, err
	}
	// Mark the device as in use; an unclean shutdown is detected by this flag
	// still being clear at the next Open.
	sb.Flags &^= sbFlagClean
	if err := writeSuperblock(dev, sb); err != nil {
		return nil, err
	}
	return c, nil
}

// load rebuilds the in-memory index from the metadata region.
func (c *Cache) load(clean bool) error {
	c.dropped = RecoveryReport{CleanShutdown: clean}
	type kept struct {
		idx   uint32
		stamp uint64
	}
	var keep []kept
	now := time.Now().UnixNano()
	err := forEachMeta(c.dev, c.layout, func(idx uint32, m metaEntry) {
		s := &c.slots[idx]
		s.prev, s.next = nilIdx, nilIdx
		if m.Flags&metaValid == 0 {
			c.free = append(c.free, idx)
			return
		}
		dirty := m.Flags&metaDirty != 0
		if !clean {
			if !dirty {
				// The metadata write for a clean block may have raced a crash;
				// it is cheap to re-read from the HDD so simply drop it.
				c.dropped.DroppedClean++
				c.free = append(c.free, idx)
				return
			}
			if !c.verifySlot(idx, m.CRC) {
				c.log.Printf("cache: dropping dirty block vol=%d block=%d in slot %d: checksum mismatch after unclean shutdown", m.Vol, m.Block, idx)
				c.dropped.Corrupt++
				c.free = append(c.free, idx)
				return
			}
		}
		key := Key{Vol: m.Vol, Block: m.Block}
		if old, ok := c.index[key]; ok {
			c.dropped.Duplicates++
			os := &c.slots[old]
			if os.stamp >= m.Stamp && !(dirty && !os.dirty) {
				c.free = append(c.free, idx)
				return
			}
			// Replace the older duplicate.
			if os.dirty {
				delete(c.dirtySet, old)
				c.dropped.RestoredDirty--
			}
			os.state, os.dirty = stateFree, false
			c.free = append(c.free, old)
			for i := range keep {
				if keep[i].idx == old {
					keep = append(keep[:i], keep[i+1:]...)
					break
				}
			}
			c.dropped.Restored--
		}
		s.key = key
		s.state = stateValid
		s.dirty = dirty
		s.crc = m.CRC
		s.stamp = m.Stamp
		if dirty {
			s.dirtyAt = now
			c.dirtySet[idx] = struct{}{}
			c.dropped.RestoredDirty++
		}
		c.index[key] = idx
		c.dropped.Restored++
		keep = append(keep, kept{idx, m.Stamp})
	})
	if err != nil {
		return err
	}
	sort.Slice(keep, func(i, j int) bool { return keep[i].stamp < keep[j].stamp })
	for _, k := range keep {
		c.lruPushFront(k.idx)
		if k.stamp > c.tick {
			c.tick = k.stamp
		}
	}
	// Reverse the free list so that low slot numbers are used first; this
	// keeps a young cache's data region compact and sequential.
	for i, j := 0, len(c.free)-1; i < j; i, j = i+1, j-1 {
		c.free[i], c.free[j] = c.free[j], c.free[i]
	}
	if !clean || c.dropped.Duplicates > 0 {
		c.log.Printf("cache: recovery: clean=%v restored=%d (dirty %d) dropped_clean=%d corrupt=%d duplicates=%d",
			clean, c.dropped.Restored, c.dropped.RestoredDirty, c.dropped.DroppedClean, c.dropped.Corrupt, c.dropped.Duplicates)
	}
	return nil
}

func (c *Cache) verifySlot(idx uint32, crc uint32) bool {
	buf := c.getBuf()
	defer c.putBuf(buf)
	if _, err := c.dev.ReadAt(buf, c.layout.dataOffset(idx)); err != nil {
		return false
	}
	return crc32.ChecksumIEEE(buf) == crc
}

// Recovery reports what Open found on the device.
func (c *Cache) Recovery() RecoveryReport { return c.dropped }

// Layout returns the device layout.
func (c *Cache) Layout() Layout { return c.layout }

// BlockSize returns the cache block size in bytes.
func (c *Cache) BlockSize() uint32 { return c.layout.BlockSize }

// Start validates that every dirty block belongs to an attached volume and
// starts the background flusher.
func (c *Cache) Start() error {
	c.mu.Lock()
	if c.started {
		c.mu.Unlock()
		return nil
	}
	// Drop cached entries for volumes that are not attached; dirty ones are
	// an error unless the caller asked to discard them.
	orphanDirty := map[uint32]uint64{}
	var orphans []uint32
	for idx := range c.dirtySet {
		s := &c.slots[idx]
		if _, ok := c.volumes[s.key.Vol]; !ok {
			orphanDirty[s.key.Vol]++
		}
	}
	if len(orphanDirty) > 0 && !c.opts.DiscardOrphanDirty {
		c.mu.Unlock()
		names := ""
		for id, n := range orphanDirty {
			name := fmt.Sprintf("id %d", id)
			if row := c.table.byID(id); row != nil {
				name = row.Name
			}
			names += fmt.Sprintf(" %s(%d blocks)", name, n)
		}
		return fmt.Errorf("%w:%s; re-add the volume(s) or start with discard-orphan-dirty", ErrOrphanDirty, names)
	}
	for idx := c.lruHead; idx != nilIdx; {
		s := &c.slots[idx]
		next := s.next
		if _, ok := c.volumes[s.key.Vol]; !ok {
			orphans = append(orphans, idx)
		}
		idx = next
	}
	for _, idx := range orphans {
		s := &c.slots[idx]
		if s.dirty {
			c.log.Printf("cache: discarding dirty orphan block vol=%d block=%d", s.key.Vol, s.key.Block)
			delete(c.dirtySet, idx)
			s.dirty = false
		}
		c.evictLocked(idx)
		c.free = append(c.free, idx)
	}
	for _, v := range c.volumes {
		v.cached.Store(0)
		v.dirty.Store(0)
	}
	for idx := c.lruHead; idx != nilIdx; idx = c.slots[idx].next {
		s := &c.slots[idx]
		if v := c.volumes[s.key.Vol]; v != nil {
			v.cached.Add(1)
			if s.dirty {
				v.dirty.Add(1)
			}
		}
	}
	c.started = true
	c.mu.Unlock()
	for _, idx := range orphans {
		_ = c.updateMeta(idx, nil)
	}
	c.wg.Add(1)
	go c.flusherLoop()
	return nil
}

// Close stops the flusher, optionally writes dirty blocks back, persists the
// LRU order and marks the device as cleanly shut down.
func (c *Cache) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	started := c.started
	c.mu.Unlock()

	if started {
		close(c.stopCh)
		c.wg.Wait()
	}
	var firstErr error
	if !c.opts.KeepDirtyOnClose {
		if _, err := c.flushPass(0, 0, 0); err != nil {
			firstErr = err
			c.log.Printf("cache: flush on close: %v (dirty blocks stay on the cache device)", err)
		}
	}
	// Wait for in-flight requests to drain.
	c.mu.Lock()
	for c.pinned > 0 {
		c.cond.Wait()
	}
	c.mu.Unlock()

	if err := c.persistAllMeta(); err != nil && firstErr == nil {
		firstErr = err
	}
	if err := c.dev.Sync(); err != nil && firstErr == nil {
		firstErr = err
	}
	for _, v := range c.volumes {
		if err := v.backend.Sync(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if firstErr == nil {
		sb := &superblock{Layout: c.layout, Flags: sbFlagClean, Created: c.sbCreated}
		firstErr = writeSuperblock(c.dev, sb)
	}
	return firstErr
}

// persistAllMeta rewrites the whole metadata region from memory (used at
// close so that access stamps, and therefore LRU order, survive restarts).
func (c *Cache) persistAllMeta() error {
	const chunk = 32768
	buf := make([]byte, chunk*metaEntrySize)
	for start := uint64(0); start < c.layout.SlotCount; start += chunk {
		n := uint64(chunk)
		if start+n > c.layout.SlotCount {
			n = c.layout.SlotCount - start
		}
		c.mu.Lock()
		for i := uint64(0); i < n; i++ {
			c.slots[start+i].meta().encode(buf[i*metaEntrySize:])
		}
		c.mu.Unlock()
		size := alignUp(int64(n)*metaEntrySize, metaSectorSize)
		clear(buf[n*metaEntrySize : size])
		if _, err := c.dev.WriteAt(buf[:size], c.layout.metaOffset(uint32(start))); err != nil {
			return fmt.Errorf("persist metadata: %w", err)
		}
	}
	return nil
}

func (c *Cache) getBuf() []byte {
	return *(c.bufPool.Get().(*[]byte))
}

func (c *Cache) putBuf(b []byte) {
	if cap(b) == int(c.layout.BlockSize) {
		b = b[:cap(b)]
		c.bufPool.Put(&b)
	}
}

// ---- LRU list (intrusive, indices into c.slots) ----

func (c *Cache) lruPushFront(idx uint32) {
	s := &c.slots[idx]
	s.prev, s.next = nilIdx, c.lruHead
	if c.lruHead != nilIdx {
		c.slots[c.lruHead].prev = idx
	}
	c.lruHead = idx
	if c.lruTail == nilIdx {
		c.lruTail = idx
	}
	s.inLRU = true
}

func (c *Cache) lruRemove(idx uint32) {
	s := &c.slots[idx]
	if !s.inLRU {
		return
	}
	if s.prev != nilIdx {
		c.slots[s.prev].next = s.next
	} else {
		c.lruHead = s.next
	}
	if s.next != nilIdx {
		c.slots[s.next].prev = s.prev
	} else {
		c.lruTail = s.prev
	}
	s.prev, s.next = nilIdx, nilIdx
	s.inLRU = false
}

func (c *Cache) lruMoveFront(idx uint32) {
	if c.lruHead == idx {
		return
	}
	c.lruRemove(idx)
	c.lruPushFront(idx)
}

// ---- slot acquisition ----

// acquire returns the slot bound to key with a pin held. When the key is
// absent and alloc is true, a slot is allocated (evicting if necessary) and
// bound in stateLoading. It returns nilIdx when the key is absent and alloc
// is false, or when no slot could be made available.
func (c *Cache) acquire(key Key, alloc bool) uint32 {
	c.mu.Lock()
	defer c.mu.Unlock()
	for {
		if c.closed {
			return nilIdx
		}
		if idx, ok := c.index[key]; ok {
			s := &c.slots[idx]
			s.refs++
			c.pinned++
			c.tick++
			s.stamp = c.tick
			c.lruMoveFront(idx)
			return idx
		}
		if !alloc {
			return nilIdx
		}
		idx, retry, err := c.allocLocked()
		if err != nil {
			return nilIdx
		}
		if retry {
			continue
		}
		s := &c.slots[idx]
		s.key = key
		s.state = stateLoading
		s.dirty = false
		s.refs = 1
		s.writeGen++
		c.pinned++
		c.tick++
		s.stamp = c.tick
		c.index[key] = idx
		c.lruPushFront(idx)
		return idx
	}
}

// allocLocked finds a free slot. It is called with c.mu held. When it has to
// flush a dirty victim or wait for pins it temporarily releases c.mu and
// returns retry=true so that the caller re-checks the index.
func (c *Cache) allocLocked() (idx uint32, retry bool, err error) {
	if n := len(c.free); n > 0 {
		idx = c.free[n-1]
		c.free = c.free[:n-1]
		return idx, false, nil
	}
	dirtyVictim := nilIdx
	scanned := 0
	for i := c.lruTail; i != nilIdx; i = c.slots[i].prev {
		s := &c.slots[i]
		if s.refs != 0 {
			continue
		}
		if !s.dirty {
			c.evictLocked(i)
			return i, false, nil
		}
		if dirtyVictim == nilIdx {
			dirtyVictim = i
		}
		scanned++
		if scanned >= maxLRUScan {
			break
		}
	}
	if dirtyVictim != nilIdx {
		s := &c.slots[dirtyVictim]
		s.refs++
		c.pinned++
		c.mu.Unlock()
		ferr := c.flushSlot(dirtyVictim)
		c.mu.Lock()
		c.releaseLocked(dirtyVictim)
		if ferr != nil {
			c.log.Printf("cache: cannot evict dirty block %+v: write-back failed: %v", s.key, ferr)
			return nilIdx, false, ferr
		}
		return nilIdx, true, nil
	}
	if c.closed {
		return nilIdx, false, ErrClosed
	}
	// Everything is pinned (tiny cache, many concurrent requests): wait.
	c.cond.Wait()
	return nilIdx, true, nil
}

// evictLocked unbinds an unpinned, clean slot. The caller takes ownership
// of the slot index.
func (c *Cache) evictLocked(idx uint32) {
	s := &c.slots[idx]
	delete(c.index, s.key)
	c.lruRemove(idx)
	if s.state == stateValid {
		if v := c.volumes[s.key.Vol]; v != nil {
			v.cached.Add(-1)
		}
		c.stats.Evictions.Add(1)
	}
	s.state = stateFree
	s.dirty = false
}

// unbind drops a pinned slot from the index (e.g. after an I/O error) and
// invalidates its on-disk metadata. The caller must hold slot.mu and its
// pin; the slot returns to the free list when the last pin is released.
func (c *Cache) unbind(idx uint32) {
	_ = c.updateMeta(idx, func() bool {
		s := &c.slots[idx]
		if s.state == stateFree {
			return false
		}
		delete(c.index, s.key)
		c.lruRemove(idx)
		if s.dirty {
			delete(c.dirtySet, idx)
			if v := c.volumes[s.key.Vol]; v != nil {
				v.dirty.Add(-1)
			}
		}
		if s.state == stateValid {
			if v := c.volumes[s.key.Vol]; v != nil {
				v.cached.Add(-1)
			}
		}
		s.state = stateFree
		s.dirty = false
		return true
	})
}

func (c *Cache) release(idx uint32) {
	c.mu.Lock()
	c.releaseLocked(idx)
	c.mu.Unlock()
}

func (c *Cache) releaseLocked(idx uint32) {
	s := &c.slots[idx]
	s.refs--
	c.pinned--
	if s.refs == 0 {
		if s.state == stateFree && !s.inLRU {
			c.free = append(c.free, idx)
		}
		c.cond.Broadcast()
	}
}

// meta encodes the persisted view of a slot from its in-memory state.
// Caller holds c.mu.
func (s *slot) meta() metaEntry {
	if s.state != stateValid {
		return metaEntry{}
	}
	m := metaEntry{Vol: s.key.Vol, Flags: metaValid, Block: s.key.Block, CRC: s.crc, Stamp: s.stamp}
	if s.dirty {
		m.Flags |= metaDirty
	}
	return m
}

// updateMeta applies mutate to the in-memory slot state (under c.mu) and
// then persists the whole metadata sector that contains slot idx, encoded
// from memory. Writing full sectors keeps every cache-device write
// sector-aligned, which raw disks on Windows require, and the per-sector
// lock makes concurrent updates of neighbouring slots safe. When mutate
// returns false nothing is written. Callers hold slot.mu.
func (c *Cache) updateMeta(idx uint32, mutate func() bool) error {
	sector := idx / metaEntriesPerSector
	lk := &c.metaLocks[sector%uint32(len(c.metaLocks))]
	lk.Lock()
	defer lk.Unlock()

	var buf [metaSectorSize]byte
	c.mu.Lock()
	if mutate != nil && !mutate() {
		c.mu.Unlock()
		return nil
	}
	first := uint64(sector) * metaEntriesPerSector
	last := first + metaEntriesPerSector
	if last > c.layout.SlotCount {
		last = c.layout.SlotCount
	}
	for i := first; i < last; i++ {
		c.slots[i].meta().encode(buf[(i-first)*metaEntrySize:])
	}
	c.mu.Unlock()

	if _, err := c.dev.WriteAt(buf[:], c.layout.MetaOff+int64(sector)*metaSectorSize); err != nil {
		c.stats.Errors.Add(1)
		return fmt.Errorf("write slot metadata: %w", err)
	}
	return nil
}

// blockCRC computes the CRC of a full slot: data followed by zero padding.
func blockCRC(data []byte, blockSize uint32) uint32 {
	crc := crc32.ChecksumIEEE(data)
	if pad := int(blockSize) - len(data); pad > 0 {
		crc = crc32.Update(crc, crc32.IEEETable, zeroPad(pad))
	}
	return crc
}

var zeroBuf = make([]byte, 1<<20)

func zeroPad(n int) []byte {
	if n <= len(zeroBuf) {
		return zeroBuf[:n]
	}
	return make([]byte, n)
}

// ---- volume registry ----

// AttachVolume registers a backend under a stable name. The name is mapped
// to a persistent numeric ID stored in the cache's volume table so cached
// blocks are recognised across restarts.
func (c *Cache) AttachVolume(cfg VolumeConfig) (*Volume, error) {
	if cfg.Name == "" || cfg.Backend == nil {
		return nil, fmt.Errorf("volume name and backend are required")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, ErrClosed
	}
	if _, ok := c.volByName[cfg.Name]; ok {
		return nil, ErrVolumeExists
	}
	row := c.table.find(cfg.Name)
	changed := false
	if row == nil {
		c.table.Volumes = append(c.table.Volumes, volumeTableRow{ID: c.table.NextID, Name: cfg.Name})
		c.table.NextID++
		row = &c.table.Volumes[len(c.table.Volumes)-1]
		changed = true
	}
	size := cfg.Backend.Size()
	if row.Size != size || row.Device != cfg.Device {
		if row.Size != 0 && row.Size != size {
			c.log.Printf("cache: volume %q size changed from %d to %d bytes", cfg.Name, row.Size, size)
		}
		row.Size, row.Device = size, cfg.Device
		changed = true
	}
	if changed {
		if err := writeTable(c.dev, c.layout, c.table); err != nil {
			return nil, err
		}
	}
	v := &Volume{
		c:         c,
		id:        row.ID,
		name:      cfg.Name,
		device:    cfg.Device,
		backend:   cfg.Backend,
		size:      size,
		policy:    cfg.Policy,
		readCache: cfg.ReadCache,
		readOnly:  cfg.ReadOnly,
	}
	if v.readOnly {
		v.policy = PolicyNone
	}
	c.volumes[v.id] = v
	c.volByName[v.name] = v
	if c.started {
		for idx := c.lruHead; idx != nilIdx; idx = c.slots[idx].next {
			s := &c.slots[idx]
			if s.key.Vol == v.id && s.state == stateValid {
				v.cached.Add(1)
				if s.dirty {
					v.dirty.Add(1)
				}
			}
		}
	}
	return v, nil
}

// Volume returns an attached volume by name.
func (c *Cache) Volume(name string) *Volume {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.volByName[name]
}

// Volumes returns the attached volumes sorted by name.
func (c *Cache) Volumes() []*Volume {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]*Volume, 0, len(c.volumes))
	for _, v := range c.volByName {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

func (c *Cache) volume(id uint32) *Volume {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.volumes[id]
}

// Stats returns a snapshot of all counters.
func (c *Cache) Stats() CacheStats {
	c.mu.Lock()
	used := int64(len(c.index))
	dirty := int64(len(c.dirtySet))
	passes := c.flushPasses
	lastErr := c.lastFlushErr
	c.mu.Unlock()
	bs := int64(c.layout.BlockSize)
	st := CacheStats{
		BlockSize:    c.layout.BlockSize,
		Slots:        c.layout.SlotCount,
		UsedSlots:    used,
		DirtySlots:   dirty,
		CacheSize:    int64(c.layout.SlotCount) * bs,
		UsedBytes:    used * bs,
		DirtyBytes:   dirty * bs,
		Counters:     c.stats.snapshot(),
		UptimeSecs:   time.Since(c.opened).Seconds(),
		FlushPasses:  passes,
		LastFlushErr: lastErr,
	}
	for _, v := range c.Volumes() {
		st.Volumes = append(st.Volumes, v.Stats())
	}
	return st
}

// DropClean removes all clean cached blocks of a volume (or of every volume
// when name is empty). Dirty blocks are kept.
func (c *Cache) DropClean(name string) (int, error) {
	var vol *Volume
	if name != "" {
		vol = c.Volume(name)
		if vol == nil {
			return 0, fmt.Errorf("unknown volume %q", name)
		}
	}
	var victims []uint32
	c.mu.Lock()
	for idx := c.lruHead; idx != nilIdx; idx = c.slots[idx].next {
		s := &c.slots[idx]
		if s.refs != 0 || s.dirty || s.state != stateValid {
			continue
		}
		if vol != nil && s.key.Vol != vol.id {
			continue
		}
		victims = append(victims, idx)
	}
	for _, idx := range victims {
		c.evictLocked(idx)
		c.slots[idx].refs++ // keep it out of the free list until metadata is cleared
		c.pinned++
	}
	c.mu.Unlock()
	for _, idx := range victims {
		s := &c.slots[idx]
		s.mu.Lock()
		_ = c.updateMeta(idx, nil)
		s.mu.Unlock()
		c.release(idx)
	}
	return len(victims), nil
}

// ensure io is used (ReadAt contracts).
var _ io.ReaderAt = (*Volume)(nil)
