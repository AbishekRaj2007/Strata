// Command strata-server runs the Strata key-value server.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"

	_ "net/http/pprof" // registers profiling handlers on the pprof mux

	"github.com/AbishekRaj2007/Strata/internal/engine"
	"github.com/AbishekRaj2007/Strata/internal/log"
	"github.com/AbishekRaj2007/Strata/internal/server"
	"github.com/AbishekRaj2007/Strata/internal/wal"
)

// The environment variables test/crash's harness sets to arm fault
// injection (T2.4). Duplicated here rather than imported: test/crash is a
// test-only package and this binary must not depend on it.
const (
	envCrashAt    = "STRATA_CRASH_AT"
	envCrashAfter = "STRATA_CRASH_AFTER_N"
)

// crashHook builds engine.Options.CrashHook from the environment. It aborts
// the process, uncleanly and by name, the Nth time the named point is
// reached -- "uncleanly" is the point: a graceful exit would flush and close
// exactly the state a crash test needs to catch unflushed and unclosed.
//
// Returns nil when STRATA_CRASH_AT is unset or empty, which is every normal
// run: the hook is then never installed and Add/FlushOldest never pay the
// cost of checking it.
func crashHook() func(point string) error {
	at := os.Getenv(envCrashAt)
	if at == "" {
		return nil
	}
	after, _ := strconv.Atoi(os.Getenv(envCrashAfter))

	var count atomic.Int64
	return func(point string) error {
		if point != at {
			return nil
		}
		if count.Add(1) < int64(after) {
			return nil
		}
		// os.Exit skips every deferred cleanup -- no WAL sync, no manifest
		// flush, no graceful anything -- which is what makes this a stand-in
		// for a real kill rather than a clean shutdown that happens to be
		// early.
		os.Exit(137)
		return nil // unreachable; satisfies the func(string) error signature
	}
}

// version is stamped at build time via -ldflags.
var version = "dev"

// shutdownTimeout bounds the drain so a stuck client cannot hold the process
// open indefinitely.
const shutdownTimeout = 30 * time.Second

type config struct {
	addr          string
	dataDir       string
	syncPolicy    string
	memtableMB    int
	memtableBytes int
	cacheMB       int
	logLevel      string
	pprofAddr     string
	showVersion   bool
}

func parseFlags(args []string, stderr *os.File) (config, error) {
	fs := flag.NewFlagSet("strata-server", flag.ContinueOnError)
	fs.SetOutput(stderr)

	var c config
	// Port 6380 deliberately, so a real Redis on 6379 stays usable alongside it.
	fs.StringVar(&c.addr, "addr", ":6380", "address to listen on")
	fs.StringVar(&c.dataDir, "data-dir", "./data", "directory holding WAL, SSTables, and manifest")
	fs.StringVar(&c.syncPolicy, "sync", "interval", "WAL sync policy: always, interval, or never")
	fs.IntVar(&c.memtableMB, "memtable-mb", 4, "memtable size threshold in megabytes")
	fs.IntVar(&c.memtableBytes, "memtable-bytes", 0, "memtable size threshold in bytes; overrides -memtable-mb when positive (test use, e.g. test/crash forcing flushes on a short run)")
	fs.IntVar(&c.cacheMB, "cache-mb", 64, "block cache capacity in megabytes")
	fs.StringVar(&c.logLevel, "log-level", log.LevelInfo, "log level: debug, info, warn, or error")
	fs.StringVar(&c.pprofAddr, "pprof-addr", "", "serve pprof on this address; empty disables it")
	fs.BoolVar(&c.showVersion, "version", false, "print version and exit")

	if err := fs.Parse(args); err != nil {
		return config{}, err
	}
	return c, c.validate()
}

func (c config) validate() error {
	// Parsed by the wal package rather than re-listed here, so the flag and
	// the policy it selects cannot drift apart.
	if _, err := wal.ParseSyncPolicy(c.syncPolicy); err != nil {
		return fmt.Errorf("invalid -sync: %w", err)
	}
	if c.memtableMB <= 0 {
		return fmt.Errorf("invalid -memtable-mb %d: want a positive value", c.memtableMB)
	}
	if c.cacheMB < 0 {
		return fmt.Errorf("invalid -cache-mb %d: want zero or more", c.cacheMB)
	}
	if c.memtableBytes < 0 {
		return fmt.Errorf("invalid -memtable-bytes %d: want zero or more", c.memtableBytes)
	}
	return nil
}

// threshold resolves the configured memtable size in bytes, applying the
// -memtable-bytes override when set.
func (c config) threshold() int {
	if c.memtableBytes > 0 {
		return c.memtableBytes
	}
	return c.memtableMB << 20
}

func main() {
	cfg, err := parseFlags(os.Args[1:], os.Stderr)
	if err != nil {
		// flag already reported usage for parse errors; only validation
		// failures need a message here.
		if !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintf(os.Stderr, "strata-server: %v\n", err)
		}
		os.Exit(2)
	}

	if cfg.showVersion {
		fmt.Printf("strata-server %s\n", version)
		return
	}

	if err := run(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "strata-server: %v\n", err)
		os.Exit(1)
	}
}

func run(cfg config) error {
	logger := log.New(os.Stderr, cfg.logLevel)

	if cfg.pprofAddr != "" {
		go func() {
			// Profiling is best-effort; a failure here must not take the
			// server down with it.
			if err := http.ListenAndServe(cfg.pprofAddr, nil); err != nil {
				logger.Error("pprof listener stopped", "err", err)
			}
		}()
		logger.Info("pprof enabled", "addr", cfg.pprofAddr)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	logger.Info("strata starting",
		"version", version,
		"addr", cfg.addr,
		"data_dir", cfg.dataDir,
		"sync", cfg.syncPolicy,
		"memtable_mb", cfg.memtableMB,
		"cache_mb", cfg.cacheMB,
	)

	policy, err := wal.ParseSyncPolicy(cfg.syncPolicy)
	if err != nil {
		return fmt.Errorf("parse sync policy: %w", err)
	}

	eng, err := engine.Open(engine.Options{
		Dir:             cfg.dataDir,
		Threshold:       cfg.threshold(),
		SyncPolicy:      policy,
		BlockCacheBytes: int64(cfg.cacheMB) << 20,
		Logger:          logger,
		CrashHook:       crashHook(),
	})
	if err != nil {
		return fmt.Errorf("open engine: %w", err)
	}

	srv, err := server.New(server.Config{
		Addr:    cfg.addr,
		Engine:  eng,
		Logger:  logger,
		Version: version,
	})
	if err != nil {
		return fmt.Errorf("create server: %w", err)
	}

	// Binding before announcing means a port conflict is reported as a
	// startup failure rather than logged after a "started" line.
	if err := srv.Listen(); err != nil {
		return err
	}

	served := make(chan error, 1)
	go func() { served <- srv.Serve() }()

	select {
	case err := <-served:
		// Serve returned on its own, which means accept failed rather than
		// a signal arriving; the engine still needs closing.
		if cerr := eng.Close(); cerr != nil {
			logger.Error("close engine", "err", cerr)
		}
		return err

	case <-ctx.Done():
		logger.Info("shutdown signal received; draining")
	}

	// Bounding the drain means a stuck client cannot hold the process open
	// forever, while a well-behaved one still finishes its command.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	if err := <-served; err != nil {
		return err
	}

	logger.Info("shutdown complete")
	return nil
}
