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
	"syscall"
	"time"

	_ "net/http/pprof" // registers profiling handlers on the pprof mux

	"github.com/AbishekRaj2007/Strata/internal/engine"
	"github.com/AbishekRaj2007/Strata/internal/log"
	"github.com/AbishekRaj2007/Strata/internal/server"
	"github.com/AbishekRaj2007/Strata/internal/wal"
)

// version is stamped at build time via -ldflags.
var version = "dev"

// shutdownTimeout bounds the drain so a stuck client cannot hold the process
// open indefinitely. Phase 2 revisits it once a WAL fsync is on the path.
const shutdownTimeout = 30 * time.Second

type config struct {
	addr        string
	dataDir     string
	syncPolicy  string
	memtableMB  int
	cacheMB     int
	logLevel    string
	pprofAddr   string
	showVersion bool
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
	return nil
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

	// The map engine is Phase 1's stand-in; the LSM engine replaces it in
	// Phase 3 behind the same interface. Nothing here is durable yet, and
	// saying so at startup is cheaper than a bug report.
	eng := engine.NewMemory()
	// The sync policy is parsed and reported, but nothing acts on it until the
	// WAL writer exists (T2.1). Saying so keeps -sync=always from reading as a
	// durability guarantee the engine cannot currently make.
	logger.Warn("using the in-memory engine; data is not durable and -sync has no effect yet (see plan.md T2.1, T3.5)",
		"sync", cfg.syncPolicy)

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
