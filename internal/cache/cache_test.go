package cache

import (
	"bytes"
	"errors"
	"io"
	"log"
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/xtrbig/bigcache/internal/backend"
)

const testBS = 4096

type harness struct {
	t     *testing.T
	ssd   *backend.Mem
	hdds  map[string]*backend.Mem
	c     *Cache
	opts  Options
	vcfgs []VolumeConfig
}

func quietLogger() *log.Logger { return log.New(io.Discard, "", 0) }

func newHarness(t *testing.T, slots int, opts Options) *harness {
	t.Helper()
	l, err := ComputeLayout(int64(superblockSize+tableSize)+int64(slots)*(testBS+metaEntrySize)+testBS, testBS)
	if err != nil {
		t.Fatal(err)
	}
	ssd := backend.NewMem(l.DeviceSize)
	if _, err := Format(ssd, testBS); err != nil {
		t.Fatal(err)
	}
	if opts.Logger == nil {
		opts.Logger = quietLogger()
	}
	h := &harness{t: t, ssd: ssd, hdds: map[string]*backend.Mem{}, opts: opts}
	return h
}

func (h *harness) addVolume(name string, size int64, policy WritePolicy) {
	h.hdds[name] = backend.NewMem(size)
	h.vcfgs = append(h.vcfgs, VolumeConfig{Name: name, Backend: h.hdds[name], Policy: policy, ReadCache: true})
}

func (h *harness) open() *Cache {
	h.t.Helper()
	c, err := Open(h.ssd, h.opts)
	if err != nil {
		h.t.Fatal(err)
	}
	for _, vc := range h.vcfgs {
		vc.Backend = h.hdds[vc.Name]
		if _, err := c.AttachVolume(vc); err != nil {
			h.t.Fatal(err)
		}
	}
	if err := c.Start(); err != nil {
		h.t.Fatal(err)
	}
	h.c = c
	return c
}

func (h *harness) close() {
	h.t.Helper()
	if err := h.c.Close(); err != nil {
		h.t.Fatal(err)
	}
}

// crash stops the engine without the clean shutdown bookkeeping.
func (h *harness) crash() {
	c := h.c
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	close(c.stopCh)
	c.wg.Wait()
}

func randBytes(r *rand.Rand, n int) []byte {
	b := make([]byte, n)
	r.Read(b)
	return b
}

func TestFormatInspect(t *testing.T) {
	h := newHarness(t, 16, Options{})
	info, err := Inspect(h.ssd)
	if err != nil {
		t.Fatal(err)
	}
	if info.SlotCount < 16 || !info.Clean || info.ValidSlots != 0 {
		t.Fatalf("unexpected info %+v", info)
	}
	if _, err := Open(backend.NewMem(1<<20), Options{}); !errors.Is(err, ErrNotFormatted) {
		t.Fatalf("expected ErrNotFormatted, got %v", err)
	}
	if _, err := ComputeLayout(1000, testBS); err == nil {
		t.Fatal("expected error for tiny device")
	}
}

func TestLayoutFits(t *testing.T) {
	for _, size := range []int64{1 << 20, 3<<20 + 12345, 100 << 20} {
		for _, bs := range []uint32{4096, 65536} {
			l, err := ComputeLayout(size, bs)
			if err != nil {
				t.Fatal(err)
			}
			if l.DataOff+int64(l.SlotCount)*int64(bs) > size {
				t.Fatalf("layout overflows device: %+v", l)
			}
			if l.MetaOff+int64(l.SlotCount)*metaEntrySize > l.DataOff {
				t.Fatalf("metadata overlaps data: %+v", l)
			}
			if l.DataOff%int64(bs) != 0 {
				t.Fatalf("data offset not aligned: %+v", l)
			}
		}
	}
}

func fuzzVolume(t *testing.T, v *Volume, shadow []byte, r *rand.Rand, ops int) {
	t.Helper()
	size := int64(len(shadow))
	for i := 0; i < ops; i++ {
		n := 1 + r.Intn(3*testBS)
		off := r.Int63n(size)
		if off+int64(n) > size {
			n = int(size - off)
		}
		if r.Intn(2) == 0 {
			data := randBytes(r, n)
			if _, err := v.WriteAt(data, off); err != nil {
				t.Fatalf("write %d@%d: %v", n, off, err)
			}
			copy(shadow[off:], data)
		} else {
			got := make([]byte, n)
			if _, err := v.ReadAt(got, off); err != nil {
				t.Fatalf("read %d@%d: %v", n, off, err)
			}
			if !bytes.Equal(got, shadow[off:off+int64(n)]) {
				t.Fatalf("op %d: read %d@%d mismatch", i, n, off)
			}
		}
	}
}

func verifyWhole(t *testing.T, v *Volume, shadow []byte) {
	t.Helper()
	got := make([]byte, len(shadow))
	if _, err := v.ReadAt(got, 0); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, shadow) {
		t.Fatal("volume content differs from shadow")
	}
}

func TestReadWritePolicies(t *testing.T) {
	for _, policy := range []WritePolicy{PolicyWriteBack, PolicyWriteThrough, PolicyNone} {
		t.Run(policy.String(), func(t *testing.T) {
			r := rand.New(rand.NewSource(1))
			// Small cache (8 slots) so eviction is exercised; odd size so the
			// last block is partial.
			h := newHarness(t, 8, Options{FlushInterval: 50 * time.Millisecond})
			size := int64(40*testBS + 1234)
			h.addVolume("a", size, policy)
			h.addVolume("b", size/2, policy)
			c := h.open()
			shadowA := make([]byte, size)
			shadowB := make([]byte, size/2)
			fuzzVolume(t, c.Volume("a"), shadowA, r, 2000)
			fuzzVolume(t, c.Volume("b"), shadowB, r, 1000)
			verifyWhole(t, c.Volume("a"), shadowA)
			verifyWhole(t, c.Volume("b"), shadowB)
			if policy != PolicyWriteBack {
				// HDD must be up to date at all times.
				if !bytes.Equal(h.hdds["a"].Bytes(), shadowA) {
					t.Fatal("HDD out of date under non write-back policy")
				}
			}
			st := c.Stats()
			if st.Counters.Evictions == 0 {
				t.Fatal("expected evictions with a tiny cache")
			}
			if st.Counters.ReadHits == 0 {
				t.Fatal("expected some read hits")
			}
			h.close()
			if !bytes.Equal(h.hdds["a"].Bytes(), shadowA) || !bytes.Equal(h.hdds["b"].Bytes(), shadowB) {
				t.Fatal("HDD content differs from shadow after close")
			}
		})
	}
}

func TestPersistenceAcrossCleanRestart(t *testing.T) {
	h := newHarness(t, 64, Options{})
	size := int64(32 * testBS)
	h.addVolume("a", size, PolicyWriteBack)
	c := h.open()
	data := randBytes(rand.New(rand.NewSource(2)), int(size))
	if _, err := c.Volume("a").WriteAt(data, 0); err != nil {
		t.Fatal(err)
	}
	if c.Stats().DirtySlots != 32 {
		t.Fatalf("expected 32 dirty slots, got %d", c.Stats().DirtySlots)
	}
	h.close()
	if !bytes.Equal(h.hdds["a"].Bytes(), data) {
		t.Fatal("close did not flush")
	}
	info, _ := Inspect(h.ssd)
	if !info.Clean || info.DirtySlots != 0 || info.ValidSlots != 32 {
		t.Fatalf("unexpected on-disk state %+v", info)
	}

	// Reopen: the blocks must be served from the SSD without touching the HDD.
	h.hdds["a"].FailReads = true
	c = h.open()
	if rep := c.Recovery(); !rep.CleanShutdown || rep.Restored != 32 {
		t.Fatalf("recovery report %+v", rep)
	}
	got := make([]byte, size)
	if _, err := c.Volume("a").ReadAt(got, 0); err != nil {
		t.Fatalf("read after restart hit the HDD: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("data mismatch after restart")
	}
	st := c.Volume("a").Stats()
	if st.Counters.ReadHits != 32 || st.Counters.ReadMisses != 0 || st.CachedBlocks != 32 {
		t.Fatalf("expected all hits, got %+v", st)
	}
	h.hdds["a"].FailReads = false
	h.close()
}

func TestDirtyBlocksSurviveRestartWithoutFlushOnClose(t *testing.T) {
	h := newHarness(t, 64, Options{KeepDirtyOnClose: true})
	size := int64(16 * testBS)
	h.addVolume("a", size, PolicyWriteBack)
	c := h.open()
	data := randBytes(rand.New(rand.NewSource(3)), int(size))
	if _, err := c.Volume("a").WriteAt(data, 0); err != nil {
		t.Fatal(err)
	}
	h.close()
	if bytes.Equal(h.hdds["a"].Bytes(), data) {
		t.Fatal("HDD should not have been written yet")
	}
	info, _ := Inspect(h.ssd)
	if info.DirtySlots != 16 {
		t.Fatalf("expected 16 dirty on disk, got %d", info.DirtySlots)
	}
	c = h.open()
	if c.Stats().DirtySlots != 16 || c.Volume("a").Stats().DirtyBlocks != 16 {
		t.Fatalf("dirty blocks not restored: %+v", c.Stats())
	}
	n, err := c.Flush()
	if err != nil || n != 16 {
		t.Fatalf("flush: n=%d err=%v", n, err)
	}
	if !bytes.Equal(h.hdds["a"].Bytes(), data) {
		t.Fatal("HDD content wrong after flush")
	}
	if c.Stats().DirtySlots != 0 {
		t.Fatal("dirty slots remain after flush")
	}
	h.close()
}

func TestUncleanShutdownRecovery(t *testing.T) {
	h := newHarness(t, 64, Options{FlushInterval: time.Hour})
	size := int64(16 * testBS)
	h.addVolume("a", size, PolicyWriteBack)
	c := h.open()
	v := c.Volume("a")
	// Blocks 0..7 dirty, blocks 8..15 clean (read into cache).
	dirtyData := randBytes(rand.New(rand.NewSource(4)), 8*testBS)
	if _, err := v.WriteAt(dirtyData, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := v.ReadAt(make([]byte, 8*testBS), 8*testBS); err != nil {
		t.Fatal(err)
	}
	if c.Stats().UsedSlots != 16 {
		t.Fatalf("expected 16 used slots, got %d", c.Stats().UsedSlots)
	}
	// Corrupt the on-SSD data of dirty block 3 to simulate a torn write.
	idx := c.index[Key{Vol: v.id, Block: 3}]
	junk := []byte("torn")
	if _, err := h.ssd.WriteAt(junk, c.layout.dataOffset(idx)); err != nil {
		t.Fatal(err)
	}
	h.crash()

	c = h.open()
	rep := c.Recovery()
	if rep.CleanShutdown || rep.Restored != 7 || rep.RestoredDirty != 7 || rep.DroppedClean != 8 || rep.Corrupt != 1 {
		t.Fatalf("unexpected recovery report %+v", rep)
	}
	if _, err := c.Flush(); err != nil {
		t.Fatal(err)
	}
	hdd := h.hdds["a"].Bytes()
	for blk := 0; blk < 8; blk++ {
		want := dirtyData[blk*testBS : (blk+1)*testBS]
		got := hdd[blk*testBS : (blk+1)*testBS]
		if blk == 3 {
			if bytes.Equal(got, want) {
				t.Fatal("corrupt block should not have been written back")
			}
			continue
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("block %d not recovered", blk)
		}
	}
	h.close()
}

func TestOrphanDirtyRefusesStart(t *testing.T) {
	h := newHarness(t, 64, Options{KeepDirtyOnClose: true})
	h.addVolume("a", 8*testBS, PolicyWriteBack)
	h.addVolume("b", 8*testBS, PolicyWriteBack)
	c := h.open()
	if _, err := c.Volume("b").WriteAt(make([]byte, testBS), 0); err != nil {
		t.Fatal(err)
	}
	h.close()

	// Reopen without volume b.
	h.vcfgs = h.vcfgs[:1]
	c, err := Open(h.ssd, h.opts)
	if err != nil {
		t.Fatal(err)
	}
	vc := h.vcfgs[0]
	vc.Backend = h.hdds["a"]
	if _, err := c.AttachVolume(vc); err != nil {
		t.Fatal(err)
	}
	if err := c.Start(); !errors.Is(err, ErrOrphanDirty) {
		t.Fatalf("expected ErrOrphanDirty, got %v", err)
	}
	// With DiscardOrphanDirty the engine starts and the orphan is gone.
	h.opts.DiscardOrphanDirty = true
	c, err = Open(h.ssd, h.opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.AttachVolume(vc); err != nil {
		t.Fatal(err)
	}
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	if c.Stats().DirtySlots != 0 || c.Stats().UsedSlots != 0 {
		t.Fatalf("orphan not discarded: %+v", c.Stats())
	}
	h.c = c
	h.close()
	if info, _ := Inspect(h.ssd); info.ValidSlots != 0 {
		t.Fatalf("orphan metadata not cleared: %+v", info)
	}
}

func TestVolumeIDsAreStable(t *testing.T) {
	h := newHarness(t, 64, Options{})
	h.addVolume("x", 8*testBS, PolicyWriteBack)
	h.addVolume("y", 8*testBS, PolicyWriteBack)
	c := h.open()
	xid, yid := c.Volume("x").ID(), c.Volume("y").ID()
	h.close()
	// Attach in the opposite order: IDs must come from the table.
	h.vcfgs[0], h.vcfgs[1] = h.vcfgs[1], h.vcfgs[0]
	c = h.open()
	if c.Volume("x").ID() != xid || c.Volume("y").ID() != yid {
		t.Fatal("volume IDs changed across restart")
	}
	h.close()
}

func TestCoalescedWriteBack(t *testing.T) {
	h := newHarness(t, 256, Options{FlushInterval: time.Hour, MaxRunBytes: 16 * testBS})
	h.addVolume("a", 128*testBS, PolicyWriteBack)
	c := h.open()
	if _, err := c.Volume("a").WriteAt(make([]byte, 128*testBS), 0); err != nil {
		t.Fatal(err)
	}
	n, err := c.Flush()
	if err != nil || n != 128 {
		t.Fatalf("flush n=%d err=%v", n, err)
	}
	st := c.Stats().Counters
	if st.FlushRuns != 8 {
		t.Fatalf("expected 8 coalesced runs of 16 blocks, got %d", st.FlushRuns)
	}
	if h.hdds["a"].Syncs == 0 {
		t.Fatal("HDD was not synced after write-back")
	}
	h.close()
}

func TestFlushRespectsConcurrentWrites(t *testing.T) {
	// A block written while the flusher is between its HDD write and the
	// mark-clean phase must stay dirty.
	h := newHarness(t, 64, Options{FlushInterval: time.Hour})
	h.addVolume("a", 8*testBS, PolicyWriteBack)
	c := h.open()
	v := c.Volume("a")
	if _, err := v.WriteAt(bytes.Repeat([]byte{1}, testBS), 0); err != nil {
		t.Fatal(err)
	}
	idx := c.index[Key{Vol: v.id, Block: 0}]
	item := flushItem{idx: idx, key: Key{Vol: v.id, Block: 0}}
	c.mu.Lock()
	c.slots[idx].refs++
	c.pinned++
	c.mu.Unlock()
	if err := c.flushRun(v, []flushItem{item}); err != nil {
		t.Fatal(err)
	}
	item.done, item.gen = true, c.slots[idx].writeGen
	if _, err := v.WriteAt(bytes.Repeat([]byte{2}, 10), 100); err != nil {
		t.Fatal(err)
	}
	if c.markClean(&item) {
		t.Fatal("block was marked clean despite a concurrent write")
	}
	c.release(idx)
	if _, err := c.Flush(); err != nil {
		t.Fatal(err)
	}
	got := h.hdds["a"].Bytes()[:testBS]
	if got[100] != 2 || got[0] != 1 {
		t.Fatal("second write lost")
	}
	h.close()
}

func TestBackgroundFlusher(t *testing.T) {
	h := newHarness(t, 64, Options{FlushInterval: 20 * time.Millisecond})
	h.addVolume("a", 8*testBS, PolicyWriteBack)
	c := h.open()
	data := bytes.Repeat([]byte{7}, 4*testBS)
	if _, err := c.Volume("a").WriteAt(data, 0); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for c.Stats().DirtySlots != 0 {
		if time.Now().After(deadline) {
			t.Fatal("background flusher did not write back")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !bytes.Equal(h.hdds["a"].Bytes()[:len(data)], data) {
		t.Fatal("HDD content wrong")
	}
	h.close()
}

func TestDirtyWatermark(t *testing.T) {
	h := newHarness(t, 100, Options{FlushInterval: time.Hour, MaxDirtyPercent: 20})
	h.addVolume("a", 100*testBS, PolicyWriteBack)
	c := h.open()
	if _, err := c.Volume("a").WriteAt(make([]byte, 60*testBS), 0); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for c.Stats().DirtySlots > 20 {
		if time.Now().After(deadline) {
			t.Fatalf("dirty count %d stayed above the watermark", c.Stats().DirtySlots)
		}
		time.Sleep(5 * time.Millisecond)
	}
	h.close()
}

func TestEvictionFlushesDirtyVictim(t *testing.T) {
	h := newHarness(t, 4, Options{FlushInterval: time.Hour, MaxDirtyPercent: 100})
	h.addVolume("a", 16*testBS, PolicyWriteBack)
	c := h.open()
	v := c.Volume("a")
	for blk := 0; blk < 16; blk++ {
		if _, err := v.WriteAt(bytes.Repeat([]byte{byte(blk)}, testBS), int64(blk)*testBS); err != nil {
			t.Fatal(err)
		}
	}
	hdd := h.hdds["a"].Bytes()
	for blk := 0; blk < 12; blk++ {
		if hdd[blk*testBS] != byte(blk) {
			t.Fatalf("block %d was evicted without write-back", blk)
		}
	}
	if c.Stats().Counters.Evictions < 12 {
		t.Fatalf("expected evictions, got %d", c.Stats().Counters.Evictions)
	}
	h.close()
}

func TestReadOnlyAndRange(t *testing.T) {
	h := newHarness(t, 16, Options{})
	h.hdds["ro"] = backend.NewMem(4 * testBS)
	h.vcfgs = append(h.vcfgs, VolumeConfig{Name: "ro", Backend: h.hdds["ro"], ReadCache: true, ReadOnly: true})
	c := h.open()
	v := c.Volume("ro")
	if _, err := v.WriteAt([]byte{1}, 0); !errors.Is(err, ErrReadOnly) {
		t.Fatalf("expected ErrReadOnly, got %v", err)
	}
	if _, err := v.ReadAt(make([]byte, 10), 4*testBS-5); !errors.Is(err, ErrOutOfRange) {
		t.Fatalf("expected ErrOutOfRange, got %v", err)
	}
	if _, err := v.ReadAt(make([]byte, 0), 4*testBS); err != nil {
		t.Fatalf("zero-length read at end should succeed: %v", err)
	}
	h.close()
}

func TestHDDReadFailureDoesNotLeakSlots(t *testing.T) {
	h := newHarness(t, 4, Options{})
	h.addVolume("a", 8*testBS, PolicyWriteBack)
	c := h.open()
	v := c.Volume("a")
	h.hdds["a"].FailReads = true
	if _, err := v.ReadAt(make([]byte, testBS), 0); err == nil {
		t.Fatal("expected read error")
	}
	if _, err := v.WriteAt([]byte{1}, 100); err == nil {
		t.Fatal("expected partial-write miss to fail (needs HDD read)")
	}
	h.hdds["a"].FailReads = false
	c.mu.Lock()
	free, used, pinned := len(c.free), len(c.index), c.pinned
	c.mu.Unlock()
	if free != 4 || used != 0 || pinned != 0 {
		t.Fatalf("slot leak: free=%d used=%d pinned=%d", free, used, pinned)
	}
	if _, err := v.WriteAt([]byte{1}, 100); err != nil {
		t.Fatal(err)
	}
	h.close()
}

func TestSSDWriteFailureFallsBackToHDD(t *testing.T) {
	h := newHarness(t, 8, Options{})
	h.addVolume("a", 8*testBS, PolicyWriteBack)
	c := h.open()
	v := c.Volume("a")
	data := bytes.Repeat([]byte{9}, testBS)
	h.ssd.FailWrites = true
	if _, err := v.WriteAt(data, 0); err != nil {
		t.Fatalf("write should fall back to the HDD: %v", err)
	}
	h.ssd.FailWrites = false
	if !bytes.Equal(h.hdds["a"].Bytes()[:testBS], data) {
		t.Fatal("data not written through to HDD")
	}
	if c.Stats().UsedSlots != 0 {
		t.Fatal("failed slot should not be in the index")
	}
	h.close()
}

func TestVerifyReadsDetectsCorruption(t *testing.T) {
	h := newHarness(t, 8, Options{VerifyReads: true})
	h.addVolume("a", 8*testBS, PolicyWriteThrough)
	c := h.open()
	v := c.Volume("a")
	data := bytes.Repeat([]byte{5}, testBS)
	if _, err := v.WriteAt(data, 0); err != nil {
		t.Fatal(err)
	}
	idx := c.index[Key{Vol: v.id, Block: 0}]
	h.ssd.WriteAt([]byte("xx"), c.layout.dataOffset(idx))
	got := make([]byte, testBS)
	if _, err := v.ReadAt(got, 0); err != nil {
		t.Fatalf("clean block should be re-read from the HDD: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("wrong data")
	}
	h.close()
}

func TestDropClean(t *testing.T) {
	h := newHarness(t, 16, Options{FlushInterval: time.Hour})
	h.addVolume("a", 8*testBS, PolicyWriteBack)
	c := h.open()
	v := c.Volume("a")
	v.ReadAt(make([]byte, 4*testBS), 0)
	v.WriteAt(make([]byte, 2*testBS), 4*testBS)
	n, err := c.DropClean("a")
	if err != nil || n != 4 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if st := c.Stats(); st.UsedSlots != 2 || st.DirtySlots != 2 {
		t.Fatalf("unexpected %+v", st)
	}
	h.close()
}

func TestConcurrentAccess(t *testing.T) {
	h := newHarness(t, 32, Options{FlushInterval: 10 * time.Millisecond, MaxDirtyPercent: 25})
	const nvol = 3
	size := int64(64 * testBS)
	for i := 0; i < nvol; i++ {
		h.addVolume(string(rune('a'+i)), size, PolicyWriteBack)
	}
	c := h.open()
	// Each goroutine owns a disjoint region so results are deterministic;
	// readers of the whole volume just check for errors.
	var wg sync.WaitGroup
	const workers = 8
	region := size / workers
	shadows := make([][]byte, nvol)
	for i := range shadows {
		shadows[i] = make([]byte, size)
	}
	for vi := 0; vi < nvol; vi++ {
		v := c.Volume(string(rune('a' + vi)))
		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func(v *Volume, w int, shadow []byte) {
				defer wg.Done()
				r := rand.New(rand.NewSource(int64(w)*100 + int64(v.ID())))
				base := int64(w) * region
				for i := 0; i < 300; i++ {
					n := 1 + r.Intn(2*testBS)
					off := base + r.Int63n(region)
					if off+int64(n) > base+region {
						n = int(base + region - off)
					}
					if r.Intn(3) != 0 {
						data := randBytes(r, n)
						if _, err := v.WriteAt(data, off); err != nil {
							t.Error(err)
							return
						}
						copy(shadow[off:], data)
					} else {
						got := make([]byte, n)
						if _, err := v.ReadAt(got, off); err != nil {
							t.Error(err)
							return
						}
						if !bytes.Equal(got, shadow[off:off+int64(n)]) {
							t.Errorf("mismatch in region %d", w)
							return
						}
					}
				}
			}(v, w, shadows[vi])
		}
		wg.Add(1)
		go func(v *Volume) {
			defer wg.Done()
			buf := make([]byte, size)
			for i := 0; i < 20; i++ {
				if _, err := v.ReadAt(buf, 0); err != nil {
					t.Error(err)
					return
				}
				if _, err := c.Flush(); err != nil {
					t.Error(err)
					return
				}
			}
		}(v)
	}
	wg.Wait()
	for vi := 0; vi < nvol; vi++ {
		verifyWhole(t, c.Volume(string(rune('a'+vi))), shadows[vi])
	}
	h.close()
	for vi := 0; vi < nvol; vi++ {
		if !bytes.Equal(h.hdds[string(rune('a'+vi))].Bytes(), shadows[vi]) {
			t.Fatalf("volume %d HDD content wrong after close", vi)
		}
	}
}

func TestLRUOrderPersists(t *testing.T) {
	h := newHarness(t, 4, Options{})
	h.addVolume("a", 8*testBS, PolicyWriteThrough)
	c := h.open()
	v := c.Volume("a")
	one := make([]byte, testBS)
	for blk := 0; blk < 4; blk++ {
		v.ReadAt(one, int64(blk)*testBS)
	}
	v.ReadAt(one, 0) // block 0 becomes MRU; block 1 is now LRU
	h.close()
	c = h.open()
	v = c.Volume("a")
	v.ReadAt(one, 5*testBS) // evicts the LRU block
	if _, ok := c.index[Key{Vol: v.id, Block: 1}]; ok {
		t.Fatal("expected block 1 (LRU) to be evicted")
	}
	if _, ok := c.index[Key{Vol: v.id, Block: 0}]; !ok {
		t.Fatal("expected block 0 (MRU) to survive")
	}
	h.close()
}
