package crash

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

// KillPoint names a place in the server where a crash can be forced. The
// server reads STRATA_CRASH_AT and aborts when it reaches the named point,
// which lets a test kill during a specific operation rather than at a random
// instant.
//
// Random killing finds shallow bugs; the interesting durability windows are
// narrow, and hitting them by chance takes far longer than naming them. The
// points beyond KillPointNone have no effect until the phase that introduces
// the operation they name.
type KillPoint string

// The fault injection points. Each is honoured by the server once the
// corresponding subsystem exists; until then the server ignores the value.
const (
	// KillPointNone leaves the server unmodified, for randomised timing kills.
	KillPointNone KillPoint = ""

	// KillPointAfterWALWrite aborts after a record reaches the WAL file but
	// before the fsync that would make it durable. A write acknowledged here
	// would be a durability violation (T2.2).
	KillPointAfterWALWrite KillPoint = "after_wal_write"

	// KillPointAfterWALSync aborts immediately after the fsync returns and
	// before the client is acknowledged. Recovery must find the record; the
	// client never learned it succeeded, so either outcome is contract-legal.
	KillPointAfterWALSync KillPoint = "after_wal_sync"

	// KillPointDuringFlush aborts partway through writing a memtable to an
	// SSTable, leaving a partial file that recovery must discard in favour of
	// the WAL (T3.5).
	KillPointDuringFlush KillPoint = "during_flush"

	// KillPointBeforeManifestSync aborts after a compaction has written its
	// output files but before the manifest fsync that commits them. The
	// compaction must not have happened (T6.3).
	KillPointBeforeManifestSync KillPoint = "before_manifest_sync"

	// KillPointAfterManifestSync aborts immediately after the manifest fsync.
	// The compaction must be fully visible on restart (T6.3).
	KillPointAfterManifestSync KillPoint = "after_manifest_sync"
)

// The environment variables the server reads to arm fault injection. They are
// defined here because this package is their only legitimate producer.
const (
	EnvCrashAt    = "STRATA_CRASH_AT"
	EnvCrashAfter = "STRATA_CRASH_AFTER_N"
)

// ServerConfig describes one supervised server process.
type ServerConfig struct {
	// Binary is the path to strata-server. Build() supplies it.
	Binary string

	// DataDir persists across restarts within one Run; it is what recovery
	// reads on the second start.
	DataDir string

	// SyncPolicy is passed as -sync. Durability guarantees only hold at
	// "always", so that is what the acknowledgement contract is tested under.
	SyncPolicy string

	// KillAt arms a fault injection point. Empty means timing-based killing.
	KillAt KillPoint

	// KillAfterN, with KillAt, delays the abort until the Nth time the point
	// is reached, which is how a crash lands mid-workload rather than on the
	// first write.
	KillAfterN int
}

// Server is a running strata-server subprocess under test.
type Server struct {
	cmd  *exec.Cmd
	addr string
	cfg  ServerConfig
}

// StartServer launches a server on an ephemeral port and waits for it to
// accept connections.
func StartServer(ctx context.Context, cfg ServerConfig) (*Server, error) {
	port, err := freePort()
	if err != nil {
		return nil, fmt.Errorf("reserve port: %w", err)
	}
	addr := fmt.Sprintf("127.0.0.1:%d", port)

	cmd := exec.CommandContext(ctx, cfg.Binary,
		"-addr", addr,
		"-data-dir", cfg.DataDir,
		"-sync", cfg.SyncPolicy,
	)
	cmd.Env = append(os.Environ(),
		fmt.Sprintf("%s=%s", EnvCrashAt, cfg.KillAt),
		fmt.Sprintf("%s=%d", EnvCrashAfter, cfg.KillAfterN),
	)
	// SIGKILL to the process must not leave the child alive if the harness
	// itself dies, so the child gets its own process group to signal.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start server: %w", err)
	}

	s := &Server{cmd: cmd, addr: addr, cfg: cfg}
	if err := s.waitReady(ctx); err != nil {
		_ = s.Kill()
		return nil, err
	}
	return s, nil
}

// Addr returns the address the server is listening on.
func (s *Server) Addr() string { return s.addr }

// waitReady polls the listener rather than parsing the startup log, so
// readiness means "accepts connections" rather than "claims to have started".
func (s *Server) waitReady(ctx context.Context) error {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		c, err := net.DialTimeout("tcp", s.addr, 200*time.Millisecond)
		if err == nil {
			_ = c.Close()
			return nil
		}
		// A server that exited during startup will never become ready, so
		// report that rather than burning the full timeout.
		if s.exited() {
			return errors.New("server exited during startup")
		}
		time.Sleep(10 * time.Millisecond)
	}
	return fmt.Errorf("server did not accept connections at %s within the timeout", s.addr)
}

func (s *Server) exited() bool {
	return s.cmd.ProcessState != nil && s.cmd.ProcessState.Exited()
}

// Kill sends SIGKILL and waits for the process to die.
//
// SIGKILL, never SIGTERM: the graceful path flushes and closes cleanly, which
// is precisely the code path this harness must not exercise. A test that kills
// gracefully proves nothing about crash durability.
func (s *Server) Kill() error {
	if s.cmd.Process == nil {
		return nil
	}
	if err := s.cmd.Process.Signal(syscall.SIGKILL); err != nil &&
		!errors.Is(err, os.ErrProcessDone) {
		return fmt.Errorf("signal: %w", err)
	}
	// The error from Wait is discarded deliberately: a killed process always
	// reports one, and its content says nothing about durability.
	_ = s.cmd.Wait()
	return nil
}

// Wait blocks until the server exits on its own, which is how a fault-injected
// abort is observed.
func (s *Server) Wait() error {
	return s.cmd.Wait()
}

// freePort asks the kernel for an unused port and releases it immediately.
//
// This races: another process can claim the port before the server binds it.
// The alternative is passing :0 and parsing the chosen port back out of the
// server's log, which couples the harness to log formatting. The race is rare
// enough on a test host, and a bind failure surfaces as a clear startup error
// rather than a silent wrong result.
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// Build compiles strata-server into dir and returns the binary path. Building
// once per test run keeps each crash iteration to a process spawn.
func Build(dir string) (string, error) {
	bin := filepath.Join(dir, "strata-server")
	cmd := exec.Command("go", "build", "-o", bin, "github.com/AbishekRaj2007/Strata/cmd/strata-server")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("build strata-server: %w: %s", err, out)
	}
	return bin, nil
}
