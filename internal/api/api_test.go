package api

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/xtrbig/bigcache/internal/backend"
	"github.com/xtrbig/bigcache/internal/cache"
)

func newCache(t *testing.T) *cache.Cache {
	t.Helper()
	ssd := backend.NewMem(2 << 20)
	if _, err := cache.Format(ssd, 4096); err != nil {
		t.Fatal(err)
	}
	c, err := cache.Open(ssd, cache.Options{Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.AttachVolume(cache.VolumeConfig{Name: "a", Backend: backend.NewMem(1 << 20), ReadCache: true}); err != nil {
		t.Fatal(err)
	}
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func TestEndpoints(t *testing.T) {
	c := newCache(t)
	srv := httptest.NewServer(Handler(c, log.New(io.Discard, "", 0)))
	defer srv.Close()
	if _, err := c.Volume("a").WriteAt(make([]byte, 8192), 0); err != nil {
		t.Fatal(err)
	}

	resp, err := http.Get(srv.URL + "/api/v1/stats")
	if err != nil {
		t.Fatal(err)
	}
	var st cache.CacheStats
	json.NewDecoder(resp.Body).Decode(&st)
	resp.Body.Close()
	if st.DirtySlots != 2 || len(st.Volumes) != 1 || st.Volumes[0].Name != "a" {
		t.Fatalf("unexpected stats %+v", st)
	}

	resp, err = http.Post(srv.URL+"/api/v1/flush?volume=a", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	var fl map[string]int
	json.NewDecoder(resp.Body).Decode(&fl)
	resp.Body.Close()
	if fl["flushed"] != 2 {
		t.Fatalf("flushed %v", fl)
	}
	resp, _ = http.Post(srv.URL+"/api/v1/flush?volume=zzz", "", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp, _ = http.Post(srv.URL+"/api/v1/drop", "", nil)
	var dr map[string]int
	json.NewDecoder(resp.Body).Decode(&dr)
	resp.Body.Close()
	if dr["dropped"] != 2 {
		t.Fatalf("dropped %v", dr)
	}
	resp, _ = http.Get(srv.URL + "/api/v1/volumes")
	if resp.StatusCode != 200 {
		t.Fatal(resp.Status)
	}
	resp.Body.Close()
}
