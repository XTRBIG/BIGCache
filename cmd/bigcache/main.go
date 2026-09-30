// Command bigcache uses an SSD as a persistent block cache in front of one
// or more HDDs and exposes the cached volumes as NBD block devices.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/xtrbig/bigcache/internal/api"
	"github.com/xtrbig/bigcache/internal/backend"
	"github.com/xtrbig/bigcache/internal/cache"
	"github.com/xtrbig/bigcache/internal/config"
	"github.com/xtrbig/bigcache/internal/nbd"
	"github.com/xtrbig/bigcache/internal/units"
)

var version = "dev"

const usage = `bigcache - SSD cache for HDD volumes (PrimoCache style)

Usage:
  bigcache init     -c CONFIG [--force]        format the cache device
  bigcache serve    -c CONFIG [flags]          run the cache daemon (NBD + API)
  bigcache inspect  -c CONFIG                  show what is on the cache device
  bigcache stats    [--control ADDR] [--watch] show live statistics
  bigcache flush    [--control ADDR] [--volume NAME]
  bigcache drop     [--control ADDR] [--volume NAME]   drop clean cached blocks
  bigcache list     [--server ADDR]            list NBD exports
  bigcache attach   NAME [DEVICE] [--server ADDR]      attach an export as a disk
                    Linux: DEVICE is /dev/nbdN (root, nbd module)
                    Windows: DEVICE is an optional WNBD instance name
  bigcache detach   DEVICE
  bigcache service  install|uninstall|start|stop|status [-c CONFIG]   (Windows)
  bigcache bench    [flags]                    self-contained benchmark
  bigcache version

Run "bigcache COMMAND -h" for the flags of a command.
`

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "init":
		err = cmdInit(os.Args[2:])
	case "serve":
		err = cmdServe(os.Args[2:])
	case "inspect":
		err = cmdInspect(os.Args[2:])
	case "stats":
		err = cmdStats(os.Args[2:])
	case "flush":
		err = cmdControl("flush", os.Args[2:])
	case "drop":
		err = cmdControl("drop", os.Args[2:])
	case "list":
		err = cmdList(os.Args[2:])
	case "attach":
		err = cmdAttach(os.Args[2:])
	case "detach":
		err = cmdDetach(os.Args[2:])
	case "service":
		err = cmdService(os.Args[2:])
	case "bench":
		err = cmdBench(os.Args[2:])
	case "version":
		fmt.Println("bigcache", version)
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func configFlag(fs *flag.FlagSet) *string {
	return fs.String("c", defaultConfigPath(), "configuration file")
}

// ---- init ----

func cmdInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	cfgPath := configFlag(fs)
	force := fs.Bool("force", false, "reformat even if the device already holds a cache")
	fs.Parse(args)
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	var dev *backend.File
	if _, err := os.Stat(cfg.CacheDevice); errors.Is(err, os.ErrNotExist) {
		if cfg.CacheSizeBytes() == 0 {
			return fmt.Errorf("%s does not exist; set cache_size to create a file-backed cache", cfg.CacheDevice)
		}
		dev, err = backend.CreateFile(cfg.CacheDevice, cfg.CacheSizeBytes())
		if err != nil {
			return err
		}
		fmt.Printf("created %s (%s)\n", cfg.CacheDevice, units.FormatSize(cfg.CacheSizeBytes()))
	} else {
		dev, err = backend.OpenFile(cfg.CacheDevice, false)
		if err != nil {
			return err
		}
		if _, ierr := cache.Inspect(dev); ierr == nil && !*force {
			dev.Close()
			return fmt.Errorf("%s already holds a BIGCache cache; use --force to discard it", cfg.CacheDevice)
		}
	}
	defer dev.Close()
	l, err := cache.Format(dev, cfg.BlockSizeBytes())
	if err != nil {
		return err
	}
	fmt.Printf("formatted %s: %d slots of %s (%s usable), metadata %s\n",
		cfg.CacheDevice, l.SlotCount, units.FormatSize(int64(l.BlockSize)),
		units.FormatSize(int64(l.SlotCount)*int64(l.BlockSize)),
		units.FormatSize(l.DataOff-l.MetaOff))
	return nil
}

// ---- inspect ----

func cmdInspect(args []string) error {
	fs := flag.NewFlagSet("inspect", flag.ExitOnError)
	cfgPath := configFlag(fs)
	asJSON := fs.Bool("json", false, "print JSON")
	fs.Parse(args)
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	dev, err := backend.OpenFile(cfg.CacheDevice, true)
	if err != nil {
		return err
	}
	defer dev.Close()
	info, err := cache.Inspect(dev)
	if err != nil {
		return err
	}
	if *asJSON {
		return printJSON(info)
	}
	bs := int64(info.BlockSize)
	fmt.Printf("cache device:   %s\n", cfg.CacheDevice)
	fmt.Printf("created:        %s\n", info.Created.Format(time.RFC3339))
	fmt.Printf("clean shutdown: %v\n", info.Clean)
	fmt.Printf("block size:     %s\n", units.FormatSize(bs))
	fmt.Printf("slots:          %d (%s)\n", info.SlotCount, units.FormatSize(int64(info.SlotCount)*bs))
	fmt.Printf("cached:         %d blocks (%s)\n", info.ValidSlots, units.FormatSize(int64(info.ValidSlots)*bs))
	fmt.Printf("dirty:          %d blocks (%s)\n", info.DirtySlots, units.FormatSize(int64(info.DirtySlots)*bs))
	fmt.Println("volumes:")
	for _, v := range info.Volumes {
		state := "configured"
		if cfg.Volume(v.Name) == nil {
			state = "NOT in config"
		}
		fmt.Printf("  %-16s id=%-3d size=%-10s cached=%-8d dirty=%-8d %s (%s)\n",
			v.Name, v.ID, units.FormatSize(v.Size), info.CachedByVol[v.ID], info.DirtyByVol[v.ID], v.Device, state)
	}
	return nil
}

// ---- serve ----

type engine struct {
	cfg      *config.Config
	cache    *cache.Cache
	dev      *backend.File
	backends []*backend.File
}

func openEngine(cfg *config.Config, discardOrphans bool, logger *log.Logger) (*engine, error) {
	dev, err := backend.OpenFile(cfg.CacheDevice, false)
	if err != nil {
		return nil, fmt.Errorf("open cache device: %w", err)
	}
	e := &engine{cfg: cfg, dev: dev}
	c, err := cache.Open(dev, cache.Options{
		FlushInterval:      cfg.FlushIntervalDuration(),
		MaxDirtyPercent:    cfg.MaxDirtyPercent,
		DurableWrites:      cfg.DurableWrites,
		VerifyReads:        cfg.VerifyReads,
		KeepDirtyOnClose:   !*cfg.FlushOnExit,
		DiscardOrphanDirty: discardOrphans,
		Logger:             logger,
	})
	if err != nil {
		dev.Close()
		return nil, err
	}
	e.cache = c
	for _, vc := range cfg.Volumes {
		b, err := backend.OpenFile(vc.Device, vc.ReadOnly)
		if err != nil {
			e.closeAll()
			return nil, fmt.Errorf("volume %s: %w", vc.Name, err)
		}
		e.backends = append(e.backends, b)
		policy, _ := cache.ParsePolicy(vc.WritePolicy)
		if _, err := c.AttachVolume(cache.VolumeConfig{
			Name:      vc.Name,
			Device:    vc.Device,
			Backend:   b,
			Policy:    policy,
			ReadCache: *vc.ReadCache,
			ReadOnly:  vc.ReadOnly,
		}); err != nil {
			e.closeAll()
			return nil, fmt.Errorf("volume %s: %w", vc.Name, err)
		}
	}
	if err := c.Start(); err != nil {
		e.closeAll()
		return nil, err
	}
	return e, nil
}

func (e *engine) closeAll() error {
	var err error
	if e.cache != nil {
		err = e.cache.Close()
	}
	for _, b := range e.backends {
		b.Close()
	}
	e.dev.Close()
	return err
}

// volumeExport adapts a cache.Volume to nbd.Export.
type volumeExport struct{ *cache.Volume }

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	cfgPath := configFlag(fs)
	discard := fs.Bool("discard-orphan-dirty", false, "discard dirty blocks of volumes that are no longer configured")
	logPath := fs.String("log", "", "append log output to this file (default: stderr, or the platform log file when running as a service)")
	fs.Parse(args)
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	service := runningAsService()
	if *logPath == "" && service {
		*logPath = defaultLogPath()
	}
	logger := log.Default()
	if *logPath != "" {
		if err := os.MkdirAll(filepath.Dir(*logPath), 0o755); err != nil {
			return err
		}
		f, err := os.OpenFile(*logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return fmt.Errorf("open log file: %w", err)
		}
		defer f.Close()
		logger = log.New(f, "", log.LstdFlags|log.Lmsgprefix)
		log.SetOutput(f)
	}
	run := func(stop <-chan struct{}) error { return runServe(cfg, *discard, logger, stop) }
	if service {
		return runAsService(run)
	}
	return run(nil)
}

// runServe runs the daemon until a signal arrives or stop is closed.
func runServe(cfg *config.Config, discard bool, logger *log.Logger, stop <-chan struct{}) error {
	if err := autoInit(cfg, logger); err != nil {
		return err
	}
	e, err := openEngine(cfg, discard, logger)
	if err != nil {
		return err
	}
	c := e.cache
	l := c.Layout()
	rep := c.Recovery()
	logger.Printf("bigcache %s starting", version)
	logger.Printf("cache %s: %d slots x %s = %s, clean_shutdown=%v restored=%d dirty=%d",
		cfg.CacheDevice, l.SlotCount, units.FormatSize(int64(l.BlockSize)),
		units.FormatSize(int64(l.SlotCount)*int64(l.BlockSize)), rep.CleanShutdown, rep.Restored, rep.RestoredDirty)
	if rep.DroppedClean > 0 || rep.Corrupt > 0 {
		logger.Printf("recovery after unclean shutdown: dropped %d clean blocks, %d corrupt dirty blocks lost", rep.DroppedClean, rep.Corrupt)
	}

	nbdSrv := nbd.NewServer(logger)
	nbdSrv.PreferredBlockSize = l.BlockSize
	for _, v := range c.Volumes() {
		nbdSrv.AddExport(v.Name(), volumeExport{v})
		logger.Printf("volume %s: %s (%s) policy=%s cached=%d dirty=%d",
			v.Name(), units.FormatSize(v.Size()), cfg.Volume(v.Name()).Device, v.Policy(), v.Stats().CachedBlocks, v.Stats().DirtyBlocks)
	}
	ln, err := nbd.Listen(cfg.Listen)
	if err != nil {
		e.closeAll()
		return fmt.Errorf("nbd listen: %w", err)
	}
	logger.Printf("NBD server listening on %s (exports: %s)", cfg.Listen, strings.Join(nbdSrv.ExportNames(), ", "))
	apiSrv := &http.Server{Addr: cfg.ControlListen, Handler: api.Handler(c, logger)}
	errCh := make(chan error, 2)
	go func() { errCh <- nbdSrv.Serve(ln) }()
	go func() {
		logger.Printf("control API listening on http://%s/api/v1/stats", cfg.ControlListen)
		if err := apiSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("control API: %w", err)
		}
	}()

	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	select {
	case sig := <-sigCh:
		logger.Printf("received %s, shutting down", sig)
	case <-stop:
		logger.Printf("stop requested, shutting down")
	case err := <-errCh:
		if err != nil {
			logger.Printf("server error: %v", err)
		}
	}
	nbdSrv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	apiSrv.Shutdown(ctx)
	cancel()
	if *cfg.FlushOnExit {
		logger.Printf("writing %d dirty blocks back to the HDDs", c.Stats().DirtySlots)
	}
	if err := e.closeAll(); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	logger.Printf("cache closed cleanly")
	return nil
}

// autoInit creates and formats a file-backed cache when cache_device does
// not exist yet and cache_size is configured. Existing devices are never
// touched; those need an explicit "bigcache init".
func autoInit(cfg *config.Config, logger *log.Logger) error {
	if _, err := os.Stat(cfg.CacheDevice); !errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if cfg.CacheSizeBytes() == 0 {
		return fmt.Errorf("cache device %s does not exist (set cache_size to create a file-backed cache, or run 'bigcache init')", cfg.CacheDevice)
	}
	if err := os.MkdirAll(filepath.Dir(cfg.CacheDevice), 0o755); err != nil {
		return err
	}
	dev, err := backend.CreateFile(cfg.CacheDevice, cfg.CacheSizeBytes())
	if err != nil {
		return err
	}
	defer dev.Close()
	l, err := cache.Format(dev, cfg.BlockSizeBytes())
	if err != nil {
		return err
	}
	logger.Printf("created cache file %s: %d slots x %s", cfg.CacheDevice, l.SlotCount, units.FormatSize(int64(l.BlockSize)))
	return nil
}

// ---- stats / flush / drop (via the control API) ----

func controlFlag(fs *flag.FlagSet) *string {
	return fs.String("control", config.DefaultControlListen, "control API address of a running bigcache serve")
}

func cmdStats(args []string) error {
	fs := flag.NewFlagSet("stats", flag.ExitOnError)
	ctl := controlFlag(fs)
	asJSON := fs.Bool("json", false, "print raw JSON")
	watch := fs.Duration("watch", 0, "refresh every interval (e.g. 2s)")
	fs.Parse(args)
	for {
		var st cache.CacheStats
		if err := getJSON("http://"+*ctl+"/api/v1/stats", &st); err != nil {
			return err
		}
		if *asJSON {
			if err := printJSON(st); err != nil {
				return err
			}
		} else {
			if *watch > 0 {
				fmt.Print("\033[H\033[2J")
			}
			printStats(st)
		}
		if *watch <= 0 {
			return nil
		}
		time.Sleep(*watch)
	}
}

func printStats(st cache.CacheStats) {
	pct := func(a, b int64) string {
		if b == 0 {
			return "0.0%"
		}
		return fmt.Sprintf("%.1f%%", 100*float64(a)/float64(b))
	}
	c := st.Counters
	fmt.Printf("BIGCache  uptime %s  block %s  cache %s\n", (time.Duration(st.UptimeSecs) * time.Second).String(),
		units.FormatSize(int64(st.BlockSize)), units.FormatSize(st.CacheSize))
	fmt.Printf("  used   %s (%s)   dirty %s (%s)\n", units.FormatSize(st.UsedBytes), pct(st.UsedSlots, int64(st.Slots)),
		units.FormatSize(st.DirtyBytes), pct(st.DirtySlots, int64(st.Slots)))
	fmt.Printf("  reads  %d blocks  hits %s  (%s served, %s from HDD)\n", c.Reads, pct(c.ReadHits, c.Reads),
		units.FormatSize(c.BytesRead), units.FormatSize(c.HDDBytesRead))
	fmt.Printf("  writes %d blocks  cached %s  (%s accepted, %s to HDD, %d write-back runs)\n", c.Writes,
		pct(c.WriteHits+c.WriteMisses, c.Writes), units.FormatSize(c.BytesWritten), units.FormatSize(c.HDDBytesWrite), c.FlushRuns)
	fmt.Printf("  evictions %d  flushed %d  errors %d  flush passes %d\n", c.Evictions, c.Flushed, c.Errors, st.FlushPasses)
	if st.LastFlushErr != "" {
		fmt.Printf("  last flush error: %s\n", st.LastFlushErr)
	}
	fmt.Printf("%-14s %-10s %-12s %-9s %-9s %-9s %-9s %-11s\n", "VOLUME", "SIZE", "POLICY", "CACHED", "DIRTY", "READ HIT", "WRITE HIT", "HDD READ")
	for _, v := range st.Volumes {
		vc := v.Counters
		fmt.Printf("%-14s %-10s %-12s %-9d %-9d %-9s %-9s %-11s\n", v.Name, units.FormatSize(v.Size), v.WritePolicy,
			v.CachedBlocks, v.DirtyBlocks, pct(vc.ReadHits, vc.Reads), pct(vc.WriteHits, vc.Writes), units.FormatSize(vc.HDDBytesRead))
	}
}

func cmdControl(op string, args []string) error {
	fs := flag.NewFlagSet(op, flag.ExitOnError)
	ctl := controlFlag(fs)
	vol := fs.String("volume", "", "limit to one volume")
	fs.Parse(args)
	url := "http://" + *ctl + "/api/v1/" + op
	if *vol != "" {
		url += "?volume=" + *vol
	}
	resp, err := http.Post(url, "application/json", nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	fmt.Print(string(body))
	return nil
}

func getJSON(url string, v any) error {
	resp, err := http.Get(url)
	if err != nil {
		return fmt.Errorf("%w (is 'bigcache serve' running?)", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", url, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// ---- NBD helpers ----

func serverFlag(fs *flag.FlagSet) *string {
	return fs.String("server", config.DefaultListen, "NBD server address")
}

func cmdList(args []string) error {
	fs := flag.NewFlagSet("list", flag.ExitOnError)
	srv := serverFlag(fs)
	fs.Parse(args)
	names, err := nbd.List(*srv)
	if err != nil {
		return err
	}
	for _, n := range names {
		fmt.Println(n)
	}
	return nil
}

func cmdAttach(args []string) error {
	fs := flag.NewFlagSet("attach", flag.ExitOnError)
	srv := serverFlag(fs)
	timeout := fs.Duration("timeout", 0, "kernel request timeout (0 = none)")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, attachUsage)
		fs.PrintDefaults()
	}
	fs.Parse(args)
	dev, ok := attachDevice(fs.Arg(0), fs.Arg(1))
	if fs.NArg() < 1 || fs.NArg() > 2 || !ok {
		fs.Usage()
		os.Exit(2)
	}
	name := fs.Arg(0)
	log.Printf("attaching export %q from %s as %s", name, *srv, dev)
	if err := nbd.Attach(dev, *srv, name, *timeout); err != nil {
		return err
	}
	log.Printf("export %q detached from %s", name, dev)
	return nil
}

func cmdDetach(args []string) error {
	fs := flag.NewFlagSet("detach", flag.ExitOnError)
	fs.Parse(args)
	if fs.NArg() != 1 {
		return errors.New("usage: bigcache detach DEVICE   (Linux: /dev/nbdN, Windows: WNBD instance name)")
	}
	return nbd.Detach(fs.Arg(0))
}

// ---- bench ----

func cmdBench(args []string) error {
	fs := flag.NewFlagSet("bench", flag.ExitOnError)
	dir := fs.String("dir", "", "directory for the temporary cache and volume files (default: in memory)")
	cacheSize := fs.String("cache-size", "64M", "cache size")
	volSize := fs.String("volume-size", "256M", "volume size")
	blockSize := fs.String("block-size", "64K", "cache block size")
	ioSize := fs.String("io-size", "4K", "request size")
	working := fs.String("working-set", "32M", "size of the hot region most requests target")
	ops := fs.Int("ops", 200000, "number of requests")
	readRatio := fs.Float64("read-ratio", 0.7, "fraction of reads")
	hot := fs.Float64("hot-ratio", 0.9, "fraction of requests that hit the working set")
	policy := fs.String("policy", "writeback", "write policy: writeback, writethrough, none")
	fs.Parse(args)

	bs, err := units.ParseSize(*blockSize)
	if err != nil {
		return err
	}
	cs, _ := units.ParseSize(*cacheSize)
	vs, _ := units.ParseSize(*volSize)
	is, _ := units.ParseSize(*ioSize)
	ws, _ := units.ParseSize(*working)
	if is <= 0 || vs < is || ws > vs {
		return errors.New("invalid sizes")
	}
	pol, err := cache.ParsePolicy(*policy)
	if err != nil {
		return err
	}

	var ssd, hdd backend.Backend
	if *dir == "" {
		ssd, hdd = backend.NewMem(cs), backend.NewMem(vs)
	} else {
		sf, err := backend.CreateFile(*dir+"/bigcache-bench-cache.img", cs)
		if err != nil {
			return err
		}
		defer os.Remove(sf.Path())
		hf, err := backend.CreateFile(*dir+"/bigcache-bench-volume.img", vs)
		if err != nil {
			return err
		}
		defer os.Remove(hf.Path())
		ssd, hdd = sf, hf
	}
	if _, err := cache.Format(ssd, uint32(bs)); err != nil {
		return err
	}
	c, err := cache.Open(ssd, cache.Options{FlushInterval: 2 * time.Second, Logger: log.New(io.Discard, "", 0)})
	if err != nil {
		return err
	}
	v, err := c.AttachVolume(cache.VolumeConfig{Name: "bench", Backend: hdd, Policy: pol, ReadCache: true})
	if err != nil {
		return err
	}
	if err := c.Start(); err != nil {
		return err
	}
	fmt.Printf("bench: cache %s (%d x %s), volume %s, %d x %s requests, %.0f%% reads, %.0f%% within a %s working set, policy %s\n",
		units.FormatSize(cs), c.Layout().SlotCount, units.FormatSize(bs), units.FormatSize(vs), *ops, units.FormatSize(is),
		*readRatio*100, *hot*100, units.FormatSize(ws), pol)

	buf := make([]byte, is)
	for i := range buf {
		buf[i] = byte(i)
	}
	if *ops < 10 {
		return errors.New("--ops must be at least 10")
	}
	r := newRand(42)
	start := time.Now()
	var readBytes, writeBytes int64
	for i := 0; i < *ops; i++ {
		var off int64
		if r.Float64() < *hot {
			off = r.Int63n(ws/is) * is
		} else {
			off = r.Int63n(vs/is) * is
		}
		if r.Float64() < *readRatio {
			if _, err := v.ReadAt(buf, off); err != nil {
				return err
			}
			readBytes += is
		} else {
			if _, err := v.WriteAt(buf, off); err != nil {
				return err
			}
			writeBytes += is
		}
		if (i+1)%(*ops/10) == 0 {
			st := v.Stats().Counters
			fmt.Printf("  %3d%%  read hit %.1f%%  dirty %d  HDD read %s  HDD write %s\n", (i+1)*100 / *ops,
				100*float64(st.ReadHits)/float64(max(st.Reads, 1)), c.Stats().DirtySlots,
				units.FormatSize(st.HDDBytesRead), units.FormatSize(st.HDDBytesWrite))
		}
	}
	elapsed := time.Since(start)
	st := c.Stats()
	fmt.Printf("done in %s: %.0f req/s, %s/s\n", elapsed.Round(time.Millisecond), float64(*ops)/elapsed.Seconds(),
		units.FormatSize(int64(float64(readBytes+writeBytes)/elapsed.Seconds())))
	printStats(st)
	fstart := time.Now()
	n, err := c.Flush()
	if err != nil {
		return err
	}
	fmt.Printf("flushed %d dirty blocks in %s (%d coalesced runs)\n", n, time.Since(fstart).Round(time.Millisecond), c.Stats().Counters.FlushRuns)
	return c.Close()
}

// tiny deterministic PRNG so bench runs are repeatable.
type prng struct{ s uint64 }

func newRand(seed uint64) *prng { return &prng{s: seed*0x9E3779B97F4A7C15 + 1} }
func (p *prng) next() uint64 {
	p.s ^= p.s << 13
	p.s ^= p.s >> 7
	p.s ^= p.s << 17
	return p.s
}
func (p *prng) Float64() float64     { return float64(p.next()>>11) / (1 << 53) }
func (p *prng) Int63n(n int64) int64 { return int64(p.next()>>1) % n }

var _ = sort.Strings
