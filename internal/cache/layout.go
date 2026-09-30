package cache

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"time"

	"github.com/xtrbig/bigcache/internal/backend"
)

// On-disk layout of the SSD cache device:
//
//	offset 0            superblock (4 KiB)
//	offset 4 KiB        volume table (64 KiB, JSON)
//	MetaOff             one 32 byte metadata entry per slot
//	DataOff             one block-size data slot per entry
//
// Every state change of a slot is written to its metadata entry, so the
// cache index survives a restart. Dirty (not yet written back) blocks are
// protected by a CRC so that a torn write during a crash is detected.
const (
	superblockSize = 4096
	tableSize      = 64 << 10
	metaEntrySize  = 32
	magic          = "BIGCACH1"
	formatVersion  = 1

	sbFlagClean = 1 // set on clean shutdown, cleared while the cache is open

	metaValid = 1
	metaDirty = 2
)

// ErrNotFormatted is returned when the device holds no BIGCache superblock.
var ErrNotFormatted = errors.New("device is not a BIGCache cache device (run 'bigcache init')")

// Layout describes where the regions of the cache device live.
type Layout struct {
	BlockSize  uint32 `json:"block_size"`
	SlotCount  uint64 `json:"slot_count"`
	DeviceSize int64  `json:"device_size"`
	TableOff   int64  `json:"table_offset"`
	MetaOff    int64  `json:"meta_offset"`
	DataOff    int64  `json:"data_offset"`
}

func (l Layout) dataOffset(idx uint32) int64 {
	return l.DataOff + int64(idx)*int64(l.BlockSize)
}

func (l Layout) metaOffset(idx uint32) int64 {
	return l.MetaOff + int64(idx)*metaEntrySize
}

// ComputeLayout fits as many slots as possible on a device of the given size.
func ComputeLayout(devSize int64, blockSize uint32) (Layout, error) {
	if blockSize < 4096 || blockSize&(blockSize-1) != 0 {
		return Layout{}, fmt.Errorf("block size %d must be a power of two >= 4096", blockSize)
	}
	hdr := int64(superblockSize + tableSize)
	if devSize <= hdr {
		return Layout{}, fmt.Errorf("device too small (%d bytes)", devSize)
	}
	bs := int64(blockSize)
	slots := (devSize - hdr) / (bs + metaEntrySize)
	var dataOff int64
	for {
		if slots <= 0 {
			return Layout{}, fmt.Errorf("device too small for a single %d byte block", blockSize)
		}
		dataOff = alignUp(hdr+slots*metaEntrySize, bs)
		if dataOff+slots*bs <= devSize {
			break
		}
		slots--
	}
	if slots > int64(^uint32(0)) {
		slots = int64(^uint32(0))
	}
	return Layout{
		BlockSize:  blockSize,
		SlotCount:  uint64(slots),
		DeviceSize: devSize,
		TableOff:   superblockSize,
		MetaOff:    hdr,
		DataOff:    dataOff,
	}, nil
}

func alignUp(v, a int64) int64 { return (v + a - 1) / a * a }

// superblock is the fixed header at offset 0.
type superblock struct {
	Layout
	Flags   uint32
	Created int64
}

func (sb *superblock) encode() []byte {
	b := make([]byte, superblockSize)
	copy(b[0:8], magic)
	binary.LittleEndian.PutUint32(b[8:], formatVersion)
	binary.LittleEndian.PutUint32(b[12:], sb.BlockSize)
	binary.LittleEndian.PutUint64(b[16:], sb.SlotCount)
	binary.LittleEndian.PutUint64(b[24:], uint64(sb.DeviceSize))
	binary.LittleEndian.PutUint64(b[32:], uint64(sb.TableOff))
	binary.LittleEndian.PutUint64(b[40:], uint64(sb.MetaOff))
	binary.LittleEndian.PutUint64(b[48:], uint64(sb.DataOff))
	binary.LittleEndian.PutUint32(b[56:], sb.Flags)
	binary.LittleEndian.PutUint64(b[64:], uint64(sb.Created))
	binary.LittleEndian.PutUint32(b[superblockSize-4:], crc32.ChecksumIEEE(b[:superblockSize-4]))
	return b
}

func decodeSuperblock(b []byte) (*superblock, error) {
	if len(b) != superblockSize || string(b[0:8]) != magic {
		return nil, ErrNotFormatted
	}
	if got, want := binary.LittleEndian.Uint32(b[superblockSize-4:]), crc32.ChecksumIEEE(b[:superblockSize-4]); got != want {
		return nil, fmt.Errorf("superblock checksum mismatch (got %08x want %08x)", got, want)
	}
	if v := binary.LittleEndian.Uint32(b[8:]); v != formatVersion {
		return nil, fmt.Errorf("unsupported cache format version %d", v)
	}
	sb := &superblock{}
	sb.BlockSize = binary.LittleEndian.Uint32(b[12:])
	sb.SlotCount = binary.LittleEndian.Uint64(b[16:])
	sb.DeviceSize = int64(binary.LittleEndian.Uint64(b[24:]))
	sb.TableOff = int64(binary.LittleEndian.Uint64(b[32:]))
	sb.MetaOff = int64(binary.LittleEndian.Uint64(b[40:]))
	sb.DataOff = int64(binary.LittleEndian.Uint64(b[48:]))
	sb.Flags = binary.LittleEndian.Uint32(b[56:])
	sb.Created = int64(binary.LittleEndian.Uint64(b[64:]))
	return sb, nil
}

func readSuperblock(dev backend.Backend) (*superblock, error) {
	b := make([]byte, superblockSize)
	if dev.Size() < superblockSize {
		return nil, ErrNotFormatted
	}
	if _, err := dev.ReadAt(b, 0); err != nil {
		return nil, fmt.Errorf("read superblock: %w", err)
	}
	return decodeSuperblock(b)
}

func writeSuperblock(dev backend.Backend, sb *superblock) error {
	if _, err := dev.WriteAt(sb.encode(), 0); err != nil {
		return fmt.Errorf("write superblock: %w", err)
	}
	return dev.Sync()
}

// metaEntry is the persisted state of one slot.
type metaEntry struct {
	Vol   uint32
	Flags uint32
	Block uint64
	CRC   uint32
	Stamp uint64
}

func (m metaEntry) encode(b []byte) {
	binary.LittleEndian.PutUint32(b[0:], m.Vol)
	binary.LittleEndian.PutUint32(b[4:], m.Flags)
	binary.LittleEndian.PutUint64(b[8:], m.Block)
	binary.LittleEndian.PutUint32(b[16:], m.CRC)
	binary.LittleEndian.PutUint64(b[20:], m.Stamp)
	binary.LittleEndian.PutUint32(b[28:], 0)
}

func decodeMeta(b []byte) metaEntry {
	return metaEntry{
		Vol:   binary.LittleEndian.Uint32(b[0:]),
		Flags: binary.LittleEndian.Uint32(b[4:]),
		Block: binary.LittleEndian.Uint64(b[8:]),
		CRC:   binary.LittleEndian.Uint32(b[16:]),
		Stamp: binary.LittleEndian.Uint64(b[20:]),
	}
}

// volumeTable maps volume names to the numeric IDs used in slot metadata.
type volumeTable struct {
	NextID  uint32           `json:"next_id"`
	Volumes []volumeTableRow `json:"volumes"`
}

type volumeTableRow struct {
	ID     uint32 `json:"id"`
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	Device string `json:"device,omitempty"`
}

func (t *volumeTable) find(name string) *volumeTableRow {
	for i := range t.Volumes {
		if t.Volumes[i].Name == name {
			return &t.Volumes[i]
		}
	}
	return nil
}

func (t *volumeTable) byID(id uint32) *volumeTableRow {
	for i := range t.Volumes {
		if t.Volumes[i].ID == id {
			return &t.Volumes[i]
		}
	}
	return nil
}

func readTable(dev backend.Backend, l Layout) (*volumeTable, error) {
	b := make([]byte, tableSize)
	if _, err := dev.ReadAt(b, l.TableOff); err != nil {
		return nil, fmt.Errorf("read volume table: %w", err)
	}
	n := binary.LittleEndian.Uint32(b[0:])
	if n > tableSize-8 {
		return nil, fmt.Errorf("volume table length %d is corrupt", n)
	}
	body := b[8 : 8+n]
	if got, want := binary.LittleEndian.Uint32(b[4:]), crc32.ChecksumIEEE(body); got != want {
		return nil, fmt.Errorf("volume table checksum mismatch")
	}
	t := &volumeTable{NextID: 1}
	if n > 0 {
		if err := json.Unmarshal(body, t); err != nil {
			return nil, fmt.Errorf("decode volume table: %w", err)
		}
	}
	if t.NextID == 0 {
		t.NextID = 1
	}
	return t, nil
}

func writeTable(dev backend.Backend, l Layout, t *volumeTable) error {
	body, err := json.Marshal(t)
	if err != nil {
		return err
	}
	if len(body) > tableSize-8 {
		return fmt.Errorf("too many volumes: volume table exceeds %d bytes", tableSize)
	}
	b := make([]byte, tableSize)
	binary.LittleEndian.PutUint32(b[0:], uint32(len(body)))
	binary.LittleEndian.PutUint32(b[4:], crc32.ChecksumIEEE(body))
	copy(b[8:], body)
	if _, err := dev.WriteAt(b, l.TableOff); err != nil {
		return fmt.Errorf("write volume table: %w", err)
	}
	return dev.Sync()
}

// Format initialises a cache device: it writes a fresh superblock, an empty
// volume table and zeroes the metadata region. Any previous cache contents
// are discarded.
func Format(dev backend.Backend, blockSize uint32) (Layout, error) {
	l, err := ComputeLayout(dev.Size(), blockSize)
	if err != nil {
		return Layout{}, err
	}
	// Invalidate first so that a crash mid-format leaves an unusable device
	// rather than one with a valid superblock and stale metadata.
	if _, err := dev.WriteAt(make([]byte, superblockSize), 0); err != nil {
		return Layout{}, fmt.Errorf("clear superblock: %w", err)
	}
	zeros := make([]byte, 1<<20)
	for off := l.MetaOff; off < l.DataOff; {
		n := int64(len(zeros))
		if off+n > l.DataOff {
			n = l.DataOff - off
		}
		if _, err := dev.WriteAt(zeros[:n], off); err != nil {
			return Layout{}, fmt.Errorf("clear metadata region: %w", err)
		}
		off += n
	}
	if err := writeTable(dev, l, &volumeTable{NextID: 1}); err != nil {
		return Layout{}, err
	}
	sb := &superblock{Layout: l, Flags: sbFlagClean, Created: time.Now().Unix()}
	if err := writeSuperblock(dev, sb); err != nil {
		return Layout{}, err
	}
	return l, nil
}

// Info describes a cache device without opening it for use.
type Info struct {
	Layout
	Clean       bool              `json:"clean"`
	Created     time.Time         `json:"created"`
	ValidSlots  uint64            `json:"valid_slots"`
	DirtySlots  uint64            `json:"dirty_slots"`
	Volumes     []volumeTableRow  `json:"volumes"`
	DirtyByVol  map[uint32]uint64 `json:"dirty_by_volume"`
	CachedByVol map[uint32]uint64 `json:"cached_by_volume"`
}

// Inspect reads the superblock, the volume table and the metadata region.
func Inspect(dev backend.Backend) (*Info, error) {
	sb, err := readSuperblock(dev)
	if err != nil {
		return nil, err
	}
	t, err := readTable(dev, sb.Layout)
	if err != nil {
		return nil, err
	}
	info := &Info{
		Layout:      sb.Layout,
		Clean:       sb.Flags&sbFlagClean != 0,
		Created:     time.Unix(sb.Created, 0),
		Volumes:     t.Volumes,
		DirtyByVol:  map[uint32]uint64{},
		CachedByVol: map[uint32]uint64{},
	}
	err = forEachMeta(dev, sb.Layout, func(idx uint32, m metaEntry) {
		if m.Flags&metaValid == 0 {
			return
		}
		info.ValidSlots++
		info.CachedByVol[m.Vol]++
		if m.Flags&metaDirty != 0 {
			info.DirtySlots++
			info.DirtyByVol[m.Vol]++
		}
	})
	return info, err
}

// forEachMeta streams the metadata region in large chunks.
func forEachMeta(dev backend.Backend, l Layout, fn func(idx uint32, m metaEntry)) error {
	const chunkEntries = 32768
	buf := make([]byte, chunkEntries*metaEntrySize)
	for start := uint64(0); start < l.SlotCount; start += chunkEntries {
		n := uint64(chunkEntries)
		if start+n > l.SlotCount {
			n = l.SlotCount - start
		}
		b := buf[:n*metaEntrySize]
		if _, err := dev.ReadAt(b, l.metaOffset(uint32(start))); err != nil {
			return fmt.Errorf("read metadata: %w", err)
		}
		for i := uint64(0); i < n; i++ {
			fn(uint32(start+i), decodeMeta(b[i*metaEntrySize:]))
		}
	}
	return nil
}
