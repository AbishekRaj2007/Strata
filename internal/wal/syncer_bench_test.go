package wal

import (
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fileTarget is a real file, so the benchmark measures actual fsync cost
// rather than a simulated delay. Group commit's benefit is entirely about
// amortising a syscall that genuinely takes about a millisecond.
type fileTarget struct {
	mu     sync.Mutex
	f      *os.File
	offset int64
}

// newFileTarget creates the file on the filesystem holding the repository
// rather than in t.TempDir().
//
// t.TempDir() lives under TMPDIR, which is tmpfs on most Linux hosts. fsync on
// tmpfs never reaches a device and returns in microseconds, so a group commit
// measurement taken there shows no speedup at all: with nothing to amortise,
// batching cannot help. Measuring durability machinery on a filesystem that
// does not implement durability produces a number that means nothing.
func newFileTarget(tb testing.TB) *fileTarget {
	tb.Helper()

	dir, err := os.MkdirTemp(benchDir(tb), "syncer-*")
	if err != nil {
		tb.Fatalf("mkdir: %v", err)
	}
	tb.Cleanup(func() { _ = os.RemoveAll(dir) })

	f, err := os.Create(filepath.Join(dir, "bench.wal"))
	if err != nil {
		tb.Fatalf("create: %v", err)
	}
	tb.Cleanup(func() { _ = f.Close() })
	return &fileTarget{f: f}
}

// benchDir returns a directory on real storage. STRATA_BENCH_DIR overrides it
// for hosts whose working tree is itself on tmpfs or a network filesystem.
func benchDir(tb testing.TB) string {
	tb.Helper()

	if d := os.Getenv("STRATA_BENCH_DIR"); d != "" {
		return d
	}
	// The working directory is the package directory, which is inside the
	// repository and therefore on whatever real filesystem holds the checkout.
	wd, err := os.Getwd()
	if err != nil {
		tb.Fatalf("getwd: %v", err)
	}
	return wd
}

func (t *fileTarget) Offset() int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.offset
}

func (t *fileTarget) append(b []byte) (int64, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	n, err := t.f.Write(b)
	t.offset += int64(n)
	return t.offset, err
}

func (t *fileTarget) Sync() error { return t.f.Sync() }

// TruncateTo implements Syncable. Unused by this benchmark, which never
// fails a sync, but required to satisfy the interface.
func (t *fileTarget) TruncateTo(offset int64) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if err := t.f.Truncate(offset); err != nil {
		return err
	}
	t.offset = offset
	return nil
}

// TestGroupCommitScalesWithConcurrency is T2.2's done-when condition:
// throughput at sync=always with 32 concurrent writers must be at least 5x the
// single-writer figure. Without group commit the two are identical, because
// every writer pays its own fsync.
//
// It is a test rather than a benchmark so it runs in CI, but it measures wall
// time and so is skipped under -short.
func TestGroupCommitScalesWithConcurrency(t *testing.T) {
	if testing.Short() {
		t.Skip("measures wall-clock fsync throughput; skipped under -short")
	}

	const (
		writesPerClient = 40
		concurrent      = 32
		wantSpeedup     = 5.0
	)

	single := measureThroughput(t, 1, writesPerClient*4)
	many := measureThroughput(t, concurrent, writesPerClient)

	speedup := many / single
	t.Logf("1 writer: %.0f writes/sec", single)
	t.Logf("%d writers: %.0f writes/sec", concurrent, many)
	t.Logf("speedup: %.1fx", speedup)

	if speedup < wantSpeedup {
		t.Errorf("speedup = %.1fx with %d writers, want at least %.0fx; group commit is not amortising fsyncs",
			speedup, concurrent, wantSpeedup)
	}
}

// measureThroughput runs clients concurrently, each performing writes writes,
// and reports writes per second.
func measureThroughput(t *testing.T, clients, writes int) float64 {
	t.Helper()

	target := newFileTarget(t)
	s := NewSyncer(target, SyncAlways)
	defer func() { _ = s.Close() }()

	payload := make([]byte, 256)
	var done atomic.Int64

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < clients; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start

			for j := 0; j < writes; j++ {
				end, err := target.append(payload)
				if err != nil {
					t.Errorf("append: %v", err)
					return
				}
				if err := s.AwaitDurable(end); err != nil {
					t.Errorf("AwaitDurable: %v", err)
					return
				}
				done.Add(1)
			}
		}()
	}

	began := time.Now()
	close(start)
	wg.Wait()
	elapsed := time.Since(began)

	return float64(done.Load()) / elapsed.Seconds()
}

// BenchmarkAwaitDurable reports per-write cost at each concurrency level, for
// the T8 profiling work. Run with:
//
//	go test ./internal/wal -run '^$' -bench AwaitDurable
func BenchmarkAwaitDurable(b *testing.B) {
	for _, clients := range []int{1, 8, 32, 128} {
		b.Run(itoa(int64(clients)), func(b *testing.B) {
			target := newFileTarget(b)
			s := NewSyncer(target, SyncAlways)
			defer func() { _ = s.Close() }()

			payload := make([]byte, 256)
			b.ResetTimer()
			b.SetParallelism(clients)
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					end, err := target.append(payload)
					if err != nil {
						b.Fatalf("append: %v", err)
					}
					if err := s.AwaitDurable(end); err != nil {
						b.Fatalf("AwaitDurable: %v", err)
					}
				}
			})

			st := s.Stats()
			b.ReportMetric(float64(st.Batched)/float64(st.Syncs), "writers/fsync")
		})
	}
}
