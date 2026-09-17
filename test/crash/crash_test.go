package crash

import (
	"context"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// durableEngineAvailable reports whether the server persists data across a
// restart. Until T3.5 closes the write path the engine is an in-memory map, so
// every acknowledged write is legitimately lost on restart and the durability
// assertion below would be asserting a guarantee the project has not made yet.
//
// The gate is an environment variable rather than a build tag so that Phase 3
// turns these tests on by setting it in CI, with no edit here.
func durableEngineAvailable() bool {
	return os.Getenv("STRATA_DURABLE") != ""
}

func requireDurableEngine(t *testing.T) {
	t.Helper()
	if !durableEngineAvailable() {
		t.Skip("no durable engine yet (plan.md T3.5); set STRATA_DURABLE=1 once the LSM engine lands")
	}
}

// buildOnce compiles the server a single time for the whole package, since
// each iteration otherwise pays a full build.
func buildServer(t *testing.T) string {
	t.Helper()
	bin, err := Build(t.TempDir())
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	return bin
}

// TestHarnessDrivesServer is the harness's own smoke test, and it runs
// unconditionally. It proves the plumbing works — spawn, ready-wait, write,
// acknowledge, kill — so that when the durability assertions switch on in
// Phase 3, a failure means a durability bug rather than a broken harness.
func TestHarnessDrivesServer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	s, err := StartServer(ctx, ServerConfig{
		Binary:     buildServer(t),
		DataDir:    t.TempDir(),
		SyncPolicy: "always",
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _ = s.Kill() }()

	c, err := Dial(s.Addr())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = c.Close() }()

	ok, err := c.Set("k", "v")
	if err != nil {
		t.Fatalf("set: %v", err)
	}
	if !ok {
		t.Fatal("SET was not acknowledged with +OK")
	}

	got, found, err := c.Get("k")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !found || got != "v" {
		t.Errorf("GET k = %q, found=%v; want %q, true", got, found, "v")
	}

	if err := s.Kill(); err != nil {
		t.Errorf("kill: %v", err)
	}
}

// TestWorkloadRecordsOnlyAcknowledgedWrites guards the trap T2.4 names: the
// harness must record what the server confirmed, not what the client sent.
// A regression here makes every later crash test wrong in a way that looks
// like a durability bug, so it is checked directly.
func TestWorkloadRecordsOnlyAcknowledgedWrites(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	s, err := StartServer(ctx, ServerConfig{
		Binary:     buildServer(t),
		DataDir:    t.TempDir(),
		SyncPolicy: "always",
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer func() { _ = s.Kill() }()

	stop := make(chan struct{})
	done := make(chan *Acked, 1)
	go func() {
		done <- RunWorkload(WorkloadConfig{
			Addr:    s.Addr(),
			Clients: 4,
			Rand:    rand.New(rand.NewSource(1)),
		}, stop)
	}()

	time.Sleep(250 * time.Millisecond)
	close(stop)
	acked := <-done

	if acked.Len() == 0 {
		t.Fatal("workload acknowledged no writes; the harness is not driving the server")
	}

	// Every recorded key must actually be readable on the live server. If the
	// recorder were logging sends, keys the server never processed would
	// appear here and this would fail.
	problems, err := Verify(s.Addr(), acked.Snapshot())
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	for _, p := range problems {
		t.Error(p)
	}
}

// TestAcknowledgedWritesSurviveKill is T2.4's actual condition: 100
// randomised crash iterations at sync=always, every acknowledged write present
// after recovery.
func TestAcknowledgedWritesSurviveKill(t *testing.T) {
	requireDurableEngine(t)

	if testing.Short() {
		t.Skip("crash iterations are slow; skipped under -short")
	}

	const iterations = 100
	bin := buildServer(t)
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))

	for i := 0; i < iterations; i++ {
		// The data directory persists across the restart within an iteration;
		// that is what recovery reads.
		dataDir := filepath.Join(t.TempDir(), "data")
		if err := os.MkdirAll(dataDir, 0o755); err != nil {
			t.Fatalf("iteration %d: mkdir: %v", i, err)
		}

		// A randomised run duration puts the kill at a different point in the
		// write path each time, which is what makes repetition worthwhile.
		runFor := time.Duration(50+rng.Intn(250)) * time.Millisecond
		acked := runUntilKill(t, i, bin, dataDir, runFor)

		if acked.Len() == 0 {
			t.Fatalf("iteration %d: no writes acknowledged before the kill", i)
		}

		problems := restartAndVerify(t, i, bin, dataDir, acked.Snapshot())
		for _, p := range problems {
			t.Errorf("iteration %d: %s", i, p)
		}
		if t.Failed() {
			t.Fatalf("iteration %d: durability violated; stopping rather than repeating the failure 99 times", i)
		}
	}
}

// runUntilKill starts a server, drives writes for d, then SIGKILLs it.
func runUntilKill(t *testing.T, iter int, bin, dataDir string, d time.Duration) *Acked {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	s, err := StartServer(ctx, ServerConfig{
		Binary:     bin,
		DataDir:    dataDir,
		SyncPolicy: "always",
	})
	if err != nil {
		t.Fatalf("iteration %d: start: %v", iter, err)
	}

	stop := make(chan struct{})
	done := make(chan *Acked, 1)
	go func() {
		done <- RunWorkload(WorkloadConfig{
			Addr:    s.Addr(),
			Clients: 8,
			Rand:    rand.New(rand.NewSource(int64(iter))),
		}, stop)
	}()

	time.Sleep(d)

	// Killed while writers are mid-flight, deliberately: the writes in flight
	// at this instant are the ones whose acknowledgement status decides
	// whether the contract holds.
	if err := s.Kill(); err != nil {
		t.Fatalf("iteration %d: kill: %v", iter, err)
	}
	close(stop)
	return <-done
}

// restartAndVerify brings the server back on the same data directory and
// checks every acknowledged write survived.
func restartAndVerify(t *testing.T, iter int, bin, dataDir string, want map[string]string) []string {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	s, err := StartServer(ctx, ServerConfig{
		Binary:     bin,
		DataDir:    dataDir,
		SyncPolicy: "always",
	})
	if err != nil {
		t.Fatalf("iteration %d: restart: %v", iter, err)
	}
	defer func() { _ = s.Kill() }()

	problems, err := Verify(s.Addr(), want)
	if err != nil {
		t.Fatalf("iteration %d: verify: %v", iter, err)
	}
	return problems
}

// TestFlushWindowSurvivesKill is T3.5's remaining done-when clause: 50 kills
// targeted at the flush window lose nothing. Unlike TestAcknowledgedWritesSurviveKill's
// randomised timing, this arms KillPointDuringFlush directly, so every
// iteration actually lands inside a flush rather than merely being likely to
// -- the same reasoning TestTargetedKillPoints already applies to the WAL
// points, extended here to run enough iterations to satisfy T3.5's "50
// kills" condition rather than TestTargetedKillPoints' one-shot check.
//
// A small -memtable-bytes threshold is what makes this practical: the
// workload's values are a few dozen bytes, so the server's real 4 MB default
// would need tens of thousands of writes per iteration to rotate even once.
func TestFlushWindowSurvivesKill(t *testing.T) {
	requireDurableEngine(t)

	if testing.Short() {
		t.Skip("crash iterations are slow; skipped under -short")
	}

	const iterations = 50
	bin := buildServer(t)

	for i := 0; i < iterations; i++ {
		dataDir := filepath.Join(t.TempDir(), "data")
		if err := os.MkdirAll(dataDir, 0o755); err != nil {
			t.Fatalf("iteration %d: mkdir: %v", i, err)
		}

		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)

		rng := rand.New(rand.NewSource(int64(i)))
		// Varying the occurrence count spreads the kill across different
		// flushes within the run -- the first, a middle one, a late one --
		// rather than always the same one.
		killAfter := 1 + rng.Intn(20)

		s, err := StartServer(ctx, ServerConfig{
			Binary:        bin,
			DataDir:       dataDir,
			SyncPolicy:    "always",
			MemtableBytes: 16 * 1024,
			KillAt:        KillPointDuringFlush,
			KillAfterN:    killAfter,
		})
		if err != nil {
			cancel()
			t.Fatalf("iteration %d: start: %v", i, err)
		}

		stop := make(chan struct{})
		done := make(chan *Acked, 1)
		go func() {
			done <- RunWorkload(WorkloadConfig{
				Addr:    s.Addr(),
				Clients: 8,
				Rand:    rand.New(rand.NewSource(int64(i))),
			}, stop)
		}()

		// The server aborts itself via os.Exit at the injected point rather
		// than waiting for a signal, so Wait is what observes the "kill".
		_ = s.Wait()
		close(stop)
		acked := <-done
		cancel()

		if acked.Len() == 0 {
			t.Fatalf("iteration %d: no writes acknowledged before the flush kill", i)
		}

		problems := restartAndVerify(t, i, bin, dataDir, acked.Snapshot())
		for _, p := range problems {
			t.Errorf("iteration %d: %s", i, p)
		}
		if t.Failed() {
			t.Fatalf("iteration %d: durability violated at the flush window; stopping rather than repeating the failure", i)
		}
	}
}

// TestTargetedKillPoints exercises the fault injection hook at each named
// point. The points beyond the WAL ones are inert until the phases that
// introduce them, so this test grows as those land rather than being rewritten.
func TestTargetedKillPoints(t *testing.T) {
	requireDurableEngine(t)

	points := []KillPoint{
		KillPointAfterWALWrite,
		KillPointAfterWALSync,
	}

	bin := buildServer(t)
	for _, p := range points {
		t.Run(string(p), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()

			dataDir := filepath.Join(t.TempDir(), "data")
			if err := os.MkdirAll(dataDir, 0o755); err != nil {
				t.Fatalf("mkdir: %v", err)
			}

			s, err := StartServer(ctx, ServerConfig{
				Binary:     bin,
				DataDir:    dataDir,
				SyncPolicy: "always",
				KillAt:     p,
				KillAfterN: 50,
			})
			if err != nil {
				t.Fatalf("start: %v", err)
			}

			stop := make(chan struct{})
			done := make(chan *Acked, 1)
			go func() {
				done <- RunWorkload(WorkloadConfig{
					Addr:    s.Addr(),
					Clients: 4,
					Rand:    rand.New(rand.NewSource(2)),
				}, stop)
			}()

			// The server aborts itself at the injected point rather than
			// waiting for a signal.
			_ = s.Wait()
			close(stop)
			acked := <-done

			problems := restartAndVerify(t, 0, bin, dataDir, acked.Snapshot())
			for _, msg := range problems {
				t.Error(msg)
			}
		})
	}
}
