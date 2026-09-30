package config

import (
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "cfg.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadDefaults(t *testing.T) {
	c, err := Load(write(t, `{"cache_device":"/tmp/ssd.img","volumes":[{"name":"a","device":"/tmp/a.img"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if c.BlockSizeBytes() != 64<<10 || c.Listen != DefaultListen || c.MaxDirtyPercent != DefaultMaxDirtyPercent {
		t.Fatalf("defaults not applied: %+v", c)
	}
	if c.Volumes[0].WritePolicy != "writeback" || !*c.Volumes[0].ReadCache || !*c.FlushOnExit {
		t.Fatalf("volume defaults not applied: %+v", c.Volumes[0])
	}
}

func TestLoadErrors(t *testing.T) {
	bad := []string{
		`{}`,
		`{"cache_device":"x"}`,
		`{"cache_device":"x","block_size":"3K","volumes":[{"name":"a","device":"b"}]}`,
		`{"cache_device":"x","volumes":[{"name":"a","device":"x"}]}`,
		`{"cache_device":"x","volumes":[{"name":"a","device":"b"},{"name":"a","device":"c"}]}`,
		`{"cache_device":"x","volumes":[{"name":"a","device":"b","write_policy":"lazy"}]}`,
		`{"cache_device":"x","unknown":1,"volumes":[{"name":"a","device":"b"}]}`,
		`{"cache_device":"x","flush_interval":"soon","volumes":[{"name":"a","device":"b"}]}`,
	}
	for _, b := range bad {
		if _, err := Load(write(t, b)); err == nil {
			t.Fatalf("expected error for %s", b)
		}
	}
}
