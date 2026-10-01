package cache

import (
	"fmt"
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/xtrbig/bigcache/internal/backend"
)

// alignedBackend fails any I/O that is not sector aligned, as raw disks on
// Windows (and O_DIRECT files on Linux) do.
type alignedBackend struct {
	*backend.Mem
	sector int64
	mu     sync.Mutex
	errs   []string
}

func (a *alignedBackend) check(op string, n int, off int64) {
	if off%a.sector != 0 || int64(n)%a.sector != 0 {
		a.mu.Lock()
		a.errs = append(a.errs, fmt.Sprintf("%s of %d bytes at %d is not %d-byte aligned", op, n, off, a.sector))
		a.mu.Unlock()
	}
}

func (a *alignedBackend) ReadAt(p []byte, off int64) (int, error) {
	a.check("read", len(p), off)
	return a.Mem.ReadAt(p, off)
}

func (a *alignedBackend) WriteAt(p []byte, off int64) (int, error) {
	a.check("write", len(p), off)
	return a.Mem.WriteAt(p, off)
}

func TestAllDeviceIOIsSectorAligned(t *testing.T) {
	const sector = 4096
	l, err := ComputeLayout(int64(superblockSize+tableSize)+300*(testBS+metaEntrySize)+testBS, testBS)
	if err != nil {
		t.Fatal(err)
	}
	ssd := &alignedBackend{Mem: backend.NewMem(l.DeviceSize), sector: sector}
	hdd := &alignedBackend{Mem: backend.NewMem(64 * testBS), sector: sector}
	if _, err := Format(ssd, testBS); err != nil {
		t.Fatal(err)
	}
	c, err := Open(ssd, Options{FlushInterval: 10 * time.Millisecond, MaxDirtyPercent: 20, Logger: quietLogger()})
	if err != nil {
		t.Fatal(err)
	}
	v, err := c.AttachVolume(VolumeConfig{Name: "a", Backend: hdd, Policy: PolicyWriteBack, ReadCache: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	r := rand.New(rand.NewSource(9))
	buf := make([]byte, 8*sector)
	for i := 0; i < 3000; i++ {
		n := (1 + r.Intn(8)) * sector
		off := r.Int63n(64*testBS/sector) * sector
		if off+int64(n) > 64*testBS {
			n = int(64*testBS - off)
		}
		if r.Intn(2) == 0 {
			r.Read(buf[:n])
			if _, err := v.WriteAt(buf[:n], off); err != nil {
				t.Fatal(err)
			}
		} else if _, err := v.ReadAt(buf[:n], off); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := c.DropClean(""); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Inspect(ssd); err != nil {
		t.Fatal(err)
	}
	for _, e := range append(ssd.errs, hdd.errs...) {
		t.Error(e)
	}
}
