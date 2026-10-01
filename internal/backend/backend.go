// Package backend abstracts the storage devices BIGCache reads from and
// writes to: the slow HDD volumes being cached and the fast SSD device
// holding the cache. Both are plain random access byte ranges.
package backend

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
)

// Backend is a fixed-size random access byte range (a file, a partition or a
// whole disk).
type Backend interface {
	io.ReaderAt
	io.WriterAt
	// Size returns the size of the device in bytes.
	Size() int64
	// Sync flushes device write caches so that completed writes are durable.
	Sync() error
	Close() error
}

// File is a Backend over a regular file or a block device.
type File struct {
	f    *os.File
	size int64
	path string
}

// OpenFile opens a regular file or block device. When readOnly is true the
// device is opened O_RDONLY and writes fail.
func OpenFile(path string, readOnly bool) (*File, error) {
	flag := os.O_RDWR
	if readOnly {
		flag = os.O_RDONLY
	}
	f, err := os.OpenFile(path, flag, 0)
	if err != nil {
		return nil, err
	}
	size, err := deviceSize(f)
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("%s: determine size: %w", path, err)
	}
	return &File{f: f, size: size, path: path}, nil
}

// deviceSize works for regular files and block devices. Regular files report
// their size from Stat; block devices are handled per platform (see
// size_unix.go and size_windows.go).
func deviceSize(f *os.File) (int64, error) {
	if st, err := f.Stat(); err == nil && st.Mode().IsRegular() {
		return st.Size(), nil
	}
	return blockDeviceSize(f)
}

// CreateFile creates (or truncates) a sparse regular file of the given size.
func CreateFile(path string, size int64) (*File, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, err
	}
	if err := f.Truncate(size); err != nil {
		f.Close()
		return nil, err
	}
	return &File{f: f, size: size, path: path}, nil
}

func (b *File) ReadAt(p []byte, off int64) (int, error) {
	n, err := b.f.ReadAt(p, off)
	if err == io.EOF && n == len(p) {
		err = nil
	}
	return n, err
}

func (b *File) WriteAt(p []byte, off int64) (int, error) { return b.f.WriteAt(p, off) }
func (b *File) Size() int64                              { return b.size }
func (b *File) Sync() error                              { return b.f.Sync() }
func (b *File) Close() error                             { return b.f.Close() }
func (b *File) Path() string                             { return b.path }

// Mem is an in-memory Backend, used by tests and by the benchmark command.
type Mem struct {
	mu     sync.RWMutex
	data   []byte
	Syncs  int64
	closed bool
	// FailWrites makes WriteAt fail when true (tests).
	FailWrites bool
	// FailReads makes ReadAt fail when true (tests).
	FailReads bool
}

// ErrInjected is returned by Mem when a failure has been injected.
var ErrInjected = errors.New("injected I/O failure")

// NewMem returns a zero filled in-memory backend of the given size.
func NewMem(size int64) *Mem { return &Mem{data: make([]byte, size)} }

func (m *Mem) check(p []byte, off int64) error {
	if off < 0 || off+int64(len(p)) > int64(len(m.data)) {
		return fmt.Errorf("mem backend: range [%d,%d) outside size %d", off, off+int64(len(p)), len(m.data))
	}
	return nil
}

func (m *Mem) ReadAt(p []byte, off int64) (int, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.FailReads {
		return 0, ErrInjected
	}
	if err := m.check(p, off); err != nil {
		return 0, err
	}
	return copy(p, m.data[off:]), nil
}

func (m *Mem) WriteAt(p []byte, off int64) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.FailWrites {
		return 0, ErrInjected
	}
	if err := m.check(p, off); err != nil {
		return 0, err
	}
	return copy(m.data[off:], p), nil
}

func (m *Mem) Size() int64 { m.mu.RLock(); defer m.mu.RUnlock(); return int64(len(m.data)) }

func (m *Mem) Sync() error { m.mu.Lock(); m.Syncs++; m.mu.Unlock(); return nil }

func (m *Mem) Close() error { m.mu.Lock(); m.closed = true; m.mu.Unlock(); return nil }

// Bytes returns a copy of the backend contents.
func (m *Mem) Bytes() []byte {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]byte, len(m.data))
	copy(out, m.data)
	return out
}
