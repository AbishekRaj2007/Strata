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

	_ "net/http/pprof" // registers profiling handlers on the pprof mux

	"github.com/AbishekRaj2007/Strata/internal/log"
)

// version is stamped at build time via -ldflags.
var version = "dev"

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
	switch c.syncPolicy {
	case "always", "interval", "never":
	default:
		return fmt.Errorf("invalid -sync %q: want always, interval, or never", c.syncPolicy)
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

	// Signal handling is wired now so that T1.2 extends a working drain
	// rather than retrofitting one.
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

	// The engine and listener arrive in T1.2 and T1.3. Until then the binary
	// exists, parses config, and shuts down cleanly on a signal.
	logger.Warn("no engine wired yet; waiting for shutdown signal (see plan.md T1.2)")

	<-ctx.Done()
	logger.Info("shutdown signal received; exiting")
	return nil
}
