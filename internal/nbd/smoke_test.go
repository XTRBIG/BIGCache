package nbd

import (
	"bytes"
	"os"
	"testing"
)

// TestSmokeNBD is driven by scripts/smoke.sh against a running
// 'bigcache serve'; it is skipped otherwise.
func TestSmokeNBD(t *testing.T) {
	addr := os.Getenv("BIGCACHE_SMOKE_ADDR")
	if addr == "" {
		t.Skip("BIGCACHE_SMOKE_ADDR not set")
	}
	c, err := Dial(addr, "hdd1")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if c.Size != 64<<20 {
		t.Fatalf("size %d", c.Size)
	}
	payload := append([]byte("BIGCACHE"), bytes.Repeat([]byte{0xAB}, 100000)...)
	if err := c.Write(1_000_000, payload, false); err != nil {
		t.Fatal(err)
	}
	if err := c.Flush(); err != nil {
		t.Fatal(err)
	}
	got, err := c.Read(1_000_000, uint32(len(payload)))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("read back mismatch")
	}
	// A second connection sees the same data (cache coherence across
	// connections).
	c2, err := Dial(addr, "hdd1")
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	got, err = c2.Read(1_000_000, 8)
	if err != nil || string(got) != "BIGCACHE" {
		t.Fatalf("second connection: %q %v", got, err)
	}
	wt, err := Dial(addr, "hdd2")
	if err != nil {
		t.Fatal(err)
	}
	defer wt.Close()
	if err := wt.Write(0, []byte("writethrough"), true); err != nil {
		t.Fatal(err)
	}
}
