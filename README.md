# BIGCache

BIGCache turns an SSD into a persistent block cache for one or more slow
HDDs, in the spirit of [PrimoCache](https://www.romexsoftware.com/en/primo-cache/)'s
level‑2 cache. It runs in user space on Linux and Windows and exposes every cached HDD
as a regular block device (Linux: the kernel's NBD driver; Windows: the
open source WNBD driver), so any filesystem, VM or application can sit on
top of it.

```
              ┌──────────────────────── bigcache serve ────────────────────────┐
 /dev/nbd0 ──►│ NBD export "media"  ─┐                                          │
 /dev/nbd1 ──►│ NBD export "backup" ─┼─► cache engine ─► SSD  (/dev/nvme0n1p3) │
 /dev/nbd2 ──►│ NBD export "vm"     ─┘        │  ▲                             │
              │                               ▼  │ write-back                  │
              │                     HDDs: /dev/sda /dev/sdb /dev/sdc           │
              └───────────────────────────────────────────────────────────────┘
```

## Features

* **One SSD, many HDDs.** A single cache device is shared by any number of
  volumes; hot blocks of every volume compete for the same space under a
  global LRU.
* **Read caching.** Blocks read from an HDD are copied to the SSD; repeated
  reads are served from the SSD.
* **Write caching with three policies** per volume:
  * `writeback` – writes are acknowledged once they are on the SSD and
    written to the HDD later ("deferred write"). Adjacent dirty blocks are
    coalesced into large sequential HDD writes.
  * `writethrough` – writes go to the HDD immediately and a copy is kept on
    the SSD.
  * `none` – writes go straight to the HDD; only reads are cached.
* **Persistent cache.** The cache index lives on the SSD. After a clean
  restart previously cached blocks are still hits; after a crash, dirty
  write-back blocks are recovered (verified by CRC) and written to the
  HDD, so no acknowledged data is lost as long as the SSD survives.
* **Correct flush semantics.** NBD `FLUSH`/`FUA` requests are honoured, so
  journaling filesystems on top of a write-back volume stay consistent.
* **Live statistics and control** through a JSON HTTP API and the
  `bigcache stats` / `flush` / `drop` commands.
* **Self-contained benchmark** (`bigcache bench`) to see the hit rate and
  the effect of block size and policy without touching real disks.
* Runs as a systemd service on Linux or a Windows service, with a
  Windows installer.

## Downloads

Pre-built binaries and the Windows installer are published on the
[GitHub Releases](https://github.com/XTRBIG/BIGCache/releases) page for
every version tag:

| file | platform |
|------|----------|
| `BIGCache-Setup-<version>-windows-x64.exe` | Windows 10/11 or Server 2016+ installer (service, PATH, Start menu) |
| `bigcache-<version>-windows-amd64.zip` / `-arm64.zip` | Windows, portable `bigcache.exe` |
| `bigcache-<version>-linux-amd64.tar.gz` / `-arm64.tar.gz` | Linux |
| `bigcache-<version>-darwin-*.tar.gz` | macOS (NBD server, stats and bench only; no block device front end) |
| `SHA256SUMS.txt` | checksums |

Every push to `main` also produces the same files as build artifacts on
the [Actions](https://github.com/XTRBIG/BIGCache/actions) tab (open the
latest *Release* run, scroll to *Artifacts*). To cut a release:

```sh
git tag v0.1.0 && git push origin v0.1.0
```

The executables are not code-signed, so Windows SmartScreen shows a
warning on first run; choose *More info → Run anyway*, or build from
source with `go build ./cmd/bigcache`.

## Quick start (Linux)

```sh
go build -o bigcache ./cmd/bigcache        # or: make

cat > /etc/bigcache/config.json <<'EOF2'
{
  "cache_device": "/dev/nvme0n1p3",
  "block_size": "64K",
  "volumes": [
    {"name": "media",  "device": "/dev/sda", "write_policy": "writeback"},
    {"name": "backup", "device": "/dev/sdb", "write_policy": "writethrough"}
  ]
}
EOF2

bigcache init  -c /etc/bigcache/config.json     # formats the SSD partition (destroys its contents!)
bigcache serve -c /etc/bigcache/config.json     # runs in the foreground; use the systemd unit for production

# In another shell: attach the cached volumes as block devices.
modprobe nbd max_part=16
bigcache attach media  /dev/nbd0 &               # or: nbd-client 127.0.0.1 10809 /dev/nbd0 -N media
bigcache attach backup /dev/nbd1 &
mount /dev/nbd0 /mnt/media                       # use it like any other disk

bigcache stats --watch 2s                        # hit rates, dirty blocks, HDD traffic
```

**Important:** once a volume is cached, always access it through
`/dev/nbdN`, never through the raw HDD device directly. With write-back
caching the HDD lags behind the cache; with any policy, direct writes to the
HDD are invisible to the cache.

To stop: `umount`, `bigcache detach /dev/nbd0` (or `nbd-client -d`), then
stop `bigcache serve`. On shutdown all dirty blocks are written to the HDDs
(set `flush_on_exit` to `false` to keep them on the SSD instead and write
them back after the next start).

You can also try it without root or real disks: the smoke test in
`scripts/smoke.sh` creates file-backed devices, serves them on localhost and
exercises them with the built-in NBD client.

## Quick start (Windows)

1. **Install the WNBD driver.** It maps NBD exports into Windows as disks
   and is free, open source and signed:
   <https://github.com/cloudbase/wnbd/releases> (it is also part of *Ceph
   for Windows*). Reboot if the installer asks.
2. **Run the BIGCache installer** (`BIGCache-Setup-<version>-windows-x64.exe`)
   or unzip the portable build somewhere on the PATH. The installer
   registers the `BIGCache` service and writes
   `C:\ProgramData\BIGCache\config.json`.
3. **Edit the configuration** (Start menu → *Edit BIGCache configuration*):

   ```json
   {
     "cache_device": "D:\\bigcache.cache",
     "cache_size": "64G",
     "block_size": "64K",
     "volumes": [
       {"name": "media", "device": "\\\\.\\PhysicalDrive1", "write_policy": "writeback"}
     ]
   }
   ```

   * `cache_device`: a **new file on the SSD** is the easiest choice; it is
     created and formatted automatically on first start with `cache_size`.
     A raw partition (`\\.\HarddiskVolume5`) works too but must be
     formatted with `bigcache init` and must not have a drive letter.
   * `volumes[].device`: `\\.\PhysicalDriveN` where N is the disk number
     shown in Disk Management. **Take that disk offline** there (right-click
     the disk → *Offline*) so Windows stops using it directly; Windows
     blocks raw writes to disks with mounted volumes.
4. **Start the service and map the cached disk** from an Administrator
   prompt:

   ```bat
   bigcache service start
   bigcache attach media            :: appears in Disk Management with its partitions and drive letters
   bigcache stats --watch 2s
   ```

   `bigcache detach media` unmaps the disk; `bigcache service stop` writes
   all dirty blocks back to the HDD and stops the service. The log is in
   `C:\ProgramData\BIGCache\bigcache.log`, and service failures are also
   in the Windows Event Log (source *BIGCache*).

The portable build can also run in the foreground without a service:
`bigcache serve -c config.json`. The service can be managed with
`bigcache service install|uninstall|start|stop|status`.

Because WNBD reconnects to `127.0.0.1:10809` on its own, keep the `listen`
address on the loopback interface; the NBD protocol has no
authentication.

## How it works

The SSD is divided into fixed-size **slots** (the *block size*, 64 KiB by
default). An in-memory index maps `(volume, block number)` to a slot and an
LRU list orders slots for eviction. The layout on the SSD is:

| region        | content                                                     |
|---------------|-------------------------------------------------------------|
| superblock    | magic, block size, slot count, offsets, clean-shutdown flag |
| volume table  | volume name → numeric ID, size, device path (JSON)           |
| metadata      | 32 bytes per slot: volume, block, valid/dirty, CRC, LRU stamp |
| data          | one block per slot                                           |

Every slot state change is written to its metadata entry, which is what
makes the cache persistent:

* **Read miss** – the whole block is read from the HDD, written to a slot,
  and its metadata entry is written as *valid, clean*.
* **Write (write-back)** – the data is written into the slot (allocating and
  filling it from the HDD first if the write covers only part of a block),
  the metadata entry is written as *valid, dirty* and the write is
  acknowledged. A background flusher writes dirty blocks back once they are
  older than `flush_interval`, or earlier once more than `max_dirty_percent`
  of the cache is dirty. Consecutive dirty blocks are merged into one HDD
  write (up to 4 MiB), the HDD is synced, and only then are the blocks
  marked clean.
* **Eviction** – the least recently used clean slot is reused. If the tail
  of the LRU is all dirty, the victim is written back first.
* **NBD FLUSH / FUA** – for write-back volumes the SSD is synced (dirty data
  must be durable *somewhere*, not necessarily on the HDD); for the other
  policies the HDD is synced.
* **Restart** – the metadata region is scanned. After a clean shutdown every
  valid block is restored in its LRU order. After a crash, clean blocks are
  discarded (cheap to re-read) and dirty blocks are kept only if their data
  matches the stored CRC, which protects against torn writes.

Volumes are identified by their **name**, not their device path, so you can
move a disk to a different `/dev/sdX` without losing its cache. If a volume
with dirty blocks is removed from the configuration, `serve` refuses to
start until the volume is added back or you pass
`--discard-orphan-dirty`.

## Configuration

```json
{
  "cache_device": "/dev/nvme0n1p3",
  "cache_size": "20G",
  "block_size": "64K",
  "listen": "127.0.0.1:10809",
  "control_listen": "127.0.0.1:10810",
  "flush_interval": "10s",
  "max_dirty_percent": 50,
  "durable_writes": false,
  "verify_reads": false,
  "flush_on_exit": true,
  "volumes": [
    {"name": "media",  "device": "/dev/sda",  "write_policy": "writeback"},
    {"name": "backup", "device": "/dev/sdb",  "write_policy": "writethrough"},
    {"name": "iso",    "device": "/dev/sdc1", "write_policy": "none", "read_only": true},
    {"name": "scratch","device": "/dev/sdd",  "read_cache": false}
  ]
}
```

| key                 | default            | meaning |
|---------------------|--------------------|---------|
| `cache_device`      | required           | SSD partition, whole SSD, or a regular file. |
| `cache_size`        | –                  | Only for `init` when `cache_device` is a file that does not exist yet. |
| `block_size`        | `64K`              | Cache block size, power of two from 4K to 16M. See *Choosing a block size*. |
| `listen`            | `127.0.0.1:10809`  | NBD server address (`host:port` or `unix:/path`). Do not expose it on untrusted networks; NBD has no authentication. |
| `control_listen`    | `127.0.0.1:10810`  | HTTP control API. |
| `flush_interval`    | `10s`              | Age after which dirty blocks are written back. |
| `max_dirty_percent` | `50`               | Dirty share that triggers immediate write-back. |
| `durable_writes`    | `false`            | fsync the SSD after every write-back write. Not needed for filesystems, which issue FLUSH/FUA themselves. |
| `verify_reads`      | `false`            | Check the CRC of every block served from the SSD (detects SSD bit rot at some CPU cost). |
| `flush_on_exit`     | `true`             | Write dirty blocks back on shutdown. |
| `volumes[].name`    | required           | Stable identifier and NBD export name. |
| `volumes[].device`  | required           | HDD block device or file. |
| `volumes[].write_policy` | `writeback`   | `writeback`, `writethrough` or `none`. |
| `volumes[].read_cache`   | `true`        | Populate the cache on read misses. |
| `volumes[].read_only`    | `false`       | Export read-only; never writes to the HDD. |

### Choosing a block size

Like PrimoCache, BIGCache caches whole blocks. Larger blocks mean less
memory and metadata, more sequential HDD traffic and better read-ahead for
large files; smaller blocks waste less cache space and avoid
read-modify-write for small random writes (a 4 KiB write into an uncached
64 KiB block has to read the rest of the block from the HDD first).

Memory use is roughly **90 bytes per slot**: a 512 GiB cache needs about
0.7 GiB of RAM at 64 KiB blocks, 2.9 GiB at 16 KiB blocks. Metadata on the
SSD is 32 bytes per slot.

Changing the block size requires `bigcache init --force`, which discards
the cache (flush dirty blocks first by stopping `serve` normally).

## Commands

| command | purpose |
|---------|---------|
| `bigcache init -c CFG [--force]` | Format the cache device (creates the file if `cache_size` is set). |
| `bigcache serve -c CFG [--discard-orphan-dirty]` | Run the daemon: NBD server + control API + background write-back. |
| `bigcache inspect -c CFG [--json]` | Offline view of the cache device: slots, dirty blocks, volumes. |
| `bigcache stats [--control ADDR] [--json] [--watch 2s]` | Live statistics. |
| `bigcache flush [--volume NAME]` | Write dirty blocks back now. |
| `bigcache drop [--volume NAME]` | Drop clean cached blocks (e.g. before a benchmark). |
| `bigcache list [--server ADDR]` | List NBD exports. |
| `bigcache attach NAME [DEVICE]` | Attach an export as a disk. Linux: `DEVICE` is `/dev/nbdN` (root, blocks until detached). Windows: via WNBD, `DEVICE` is an optional instance name. |
| `bigcache detach DEVICE` | Disconnect a device. |
| `bigcache service install\|uninstall\|start\|stop\|status` | Manage the Windows service. |
| `bigcache bench [flags]` | Self-contained benchmark with temporary devices. |

The control API (`GET /api/v1/stats`, `GET /api/v1/volumes`,
`POST /api/v1/flush?volume=`, `POST /api/v1/drop?volume=`, `GET /healthz`)
returns JSON and is easy to scrape.

## Running as a service

**Linux:** `examples/bigcache.service` runs the daemon under systemd and
`examples/config.json` is a complete configuration. After
`systemctl enable --now bigcache`, attach devices with
`systemctl enable --now bigcache-attach@media:nbd0` (unit in
`examples/attach@.service`), or with `bigcache attach` / `nbd-client`.

**Windows:** the installer (or `bigcache service install`) registers a
service that starts automatically, logs to
`%ProgramData%\BIGCache\bigcache.log` and writes dirty blocks back when
it is stopped. `examples/config.windows.json` is a complete configuration.

## Durability model

* Acknowledged write-back writes live on the SSD until flushed. If the SSD
  fails before that, those writes are lost; this is the same trade-off
  PrimoCache's deferred write makes. Use `writethrough` for data you cannot
  afford to lose on SSD failure.
* An NBD `FLUSH` makes all earlier writes durable on the SSD (write-back)
  or the HDD (other policies). Filesystems rely on this, so a power loss
  never leaves the filesystem inconsistent.
* Without `durable_writes`, a write-back write that was acknowledged but
  not yet followed by a FLUSH may be lost on power failure, exactly like a
  disk with a volatile write cache.
* A crash while a dirty block was being written leaves either the old or
  the new data, detected by CRC; a torn block is dropped with a log message.

## Limitations

* The block device front end needs a kernel NBD driver: `nbd` on Linux,
  WNBD on Windows. The NBD server itself works with qemu, `nbdfuse` and
  other NBD clients on any OS. PrimoCache's own filter driver caches a
  disk in place; BIGCache instead presents the cached HDD as a new disk
  and the original must be taken offline.
* On Windows all requests reaching raw disks must be sector aligned; the
  NBD front end guarantees this and BIGCache itself only issues aligned
  I/O, but a custom NBD client with unaligned requests gets EIO.
* Whole-block granularity: partial writes to uncached blocks read the rest
  of the block from the HDD first.
* No dedicated RAM (level‑1) cache; the kernel page cache above the NBD
  device already serves that role.
* Requests pass through user space, which adds latency compared with an
  in-kernel solution such as `bcache`/`dm-cache`. BIGCache's advantages are
  a persistent cache that needs no re-formatting of the HDDs, per-volume
  policies, one cache for many disks and simple observability.

## Development

```sh
make test          # unit tests with the race detector
make smoke         # end-to-end test with file-backed devices
make bench         # in-memory benchmark
make release       # cross-compile all platforms into dist/
```

The Windows installer is built with Inno Setup from `installer/bigcache.iss`
(`ISCC.exe /DAppVersion=1.2.3 installer\bigcache.iss` after
`scripts/build-release.sh`); the release workflow does this on a Windows
runner.

The engine (`internal/cache`) is independent of NBD and can be embedded:
`cache.Open` a formatted backend, `AttachVolume` your `backend.Backend`
implementations, `Start`, and use the returned `*cache.Volume` as an
`io.ReaderAt`/`io.WriterAt`.

## License

MIT, see `LICENSE`.
