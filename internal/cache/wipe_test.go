package cache

import (
	"errors"
	"testing"
)

func TestWipe(t *testing.T) {
	h := newHarness(t, 16, Options{KeepDirtyOnClose: true})
	h.addVolume("a", 8*testBS, PolicyWriteBack)
	c := h.open()
	if _, err := c.Volume("a").WriteAt(make([]byte, testBS), 0); err != nil {
		t.Fatal(err)
	}
	h.close()
	if err := Wipe(h.ssd, false); err == nil {
		t.Fatal("wipe must refuse a cache with dirty blocks")
	}
	if _, err := Inspect(h.ssd); err != nil {
		t.Fatalf("refused wipe must leave the cache intact: %v", err)
	}
	if err := Wipe(h.ssd, true); err != nil {
		t.Fatal(err)
	}
	if _, err := Inspect(h.ssd); !errors.Is(err, ErrNotFormatted) {
		t.Fatalf("expected ErrNotFormatted after wipe, got %v", err)
	}
	if err := Wipe(h.ssd, false); err != nil {
		t.Fatalf("wiping an unformatted device must be a no-op: %v", err)
	}
}
