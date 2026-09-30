package nbd

import (
	"bytes"
	"errors"
	"io"
	"log"
	"math/rand"
	"net"
	"sync"
	"syscall"
	"testing"

	"github.com/xtrbig/bigcache/internal/backend"
)

type memExport struct {
	*backend.Mem
	ro bool
}

func (m memExport) ReadOnly() bool { return m.ro }

func startServer(t *testing.T) (*Server, string, *backend.Mem) {
	t.Helper()
	s := NewServer(log.New(io.Discard, "", 0))
	mem := backend.NewMem(1 << 20)
	s.AddExport("disk", memExport{Mem: mem})
	s.AddExport("ro", memExport{Mem: backend.NewMem(4096), ro: true})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go s.Serve(ln)
	t.Cleanup(s.Close)
	return s, ln.Addr().String(), mem
}

func TestListAndNegotiate(t *testing.T) {
	_, addr, _ := startServer(t)
	names, err := List(addr)
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 2 || names[0] != "disk" || names[1] != "ro" {
		t.Fatalf("unexpected export list %v", names)
	}
	if _, err := Dial(addr, "nope"); err == nil {
		t.Fatal("expected unknown export error")
	}
	c, err := Dial(addr, "ro")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if c.Size != 4096 || c.Flags&TxReadOnly == 0 || c.Flags&TxSendFlush == 0 {
		t.Fatalf("size=%d flags=%#x", c.Size, c.Flags)
	}
	if err := c.Write(0, []byte{1}, false); !errors.Is(err, syscall.EPERM) {
		t.Fatalf("expected EPERM, got %v", err)
	}
}

func TestReadWriteFlush(t *testing.T) {
	_, addr, mem := startServer(t)
	c, err := Dial(addr, "disk")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if c.Size != 1<<20 {
		t.Fatalf("size %d", c.Size)
	}
	r := rand.New(rand.NewSource(1))
	data := make([]byte, 70000)
	r.Read(data)
	if err := c.Write(12345, data, true); err != nil {
		t.Fatal(err)
	}
	got, err := c.Read(12345, uint32(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("read back mismatch")
	}
	if err := c.Flush(); err != nil {
		t.Fatal(err)
	}
	if mem.Syncs < 2 {
		t.Fatalf("expected FUA and FLUSH to sync, got %d syncs", mem.Syncs)
	}
	if err := c.WriteZeroes(12345, 1000); err != nil {
		t.Fatal(err)
	}
	got, _ = c.Read(12345, 1000)
	if !bytes.Equal(got, make([]byte, 1000)) {
		t.Fatal("write zeroes failed")
	}
	// Out of range requests are rejected without killing the connection.
	if _, err := c.Read(1<<20-10, 100); !errors.Is(err, syscall.EINVAL) {
		t.Fatalf("expected EINVAL, got %v", err)
	}
	if err := c.Write(1<<20-10, make([]byte, 100), false); !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("expected ENOSPC, got %v", err)
	}
	if _, err := c.Read(0, 16); err != nil {
		t.Fatalf("connection should still work: %v", err)
	}
}

func TestExportNameNegotiation(t *testing.T) {
	// Old-style NBD_OPT_EXPORT_NAME path.
	_, addr, _ := startServer(t)
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := clientHandshake(conn); err != nil {
		t.Fatal(err)
	}
	if err := sendOption(conn, optExportName, []byte("disk")); err != nil {
		t.Fatal(err)
	}
	var reply [10]byte
	if _, err := io.ReadFull(conn, reply[:]); err != nil {
		t.Fatal(err)
	}
	c := &Client{conn: conn}
	if err := c.Write(0, []byte("hello"), false); err != nil {
		t.Fatal(err)
	}
	got, err := c.Read(0, 5)
	if err != nil || string(got) != "hello" {
		t.Fatalf("got %q err %v", got, err)
	}
}

func TestConcurrentClients(t *testing.T) {
	_, addr, mem := startServer(t)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c, err := Dial(addr, "disk")
			if err != nil {
				t.Error(err)
				return
			}
			defer c.Close()
			base := uint64(i) * 65536
			pattern := bytes.Repeat([]byte{byte(i + 1)}, 4096)
			for j := 0; j < 16; j++ {
				if err := c.Write(base+uint64(j)*4096, pattern, false); err != nil {
					t.Error(err)
					return
				}
			}
			got, err := c.Read(base, 65536)
			if err != nil {
				t.Error(err)
				return
			}
			if !bytes.Equal(got, bytes.Repeat(pattern, 16)) {
				t.Error("mismatch")
			}
		}(i)
	}
	wg.Wait()
	if mem.Bytes()[7*65536] != 8 {
		t.Fatal("data not written")
	}
}
