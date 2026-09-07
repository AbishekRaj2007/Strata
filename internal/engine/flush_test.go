package engine

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/AbishekRaj2007/Strata/internal/manifest"
	"github.com/AbishekRaj2007/Strata/internal/sstable"
)

// flushEnv is a memtable set, a manifest, and a flusher over one directory --
// the whole write path from an accepted Put to a committed SSTable.
type flushEnv struct {
	dir string
	set *memtableSet
	vs  *manifest.VersionSet
	log *manifest.Log
	f   *Flusher
}

func newFlushEnv(t *testing.T, threshold, maxImmutable int) *flushEnv {
	t.Helper()
	dir := t.TempDir()

	vs := manifest.NewVersionSet()
	log, err := manifest.CreateLog(dir, vs.NextFileNumber())
	if err != nil {
		t.Fatalf("CreateLog: %v", err)
	}
	if err := manifest.WriteCurrent(dir, log.Name()); err != nil {
		t.Fatalf("WriteCurrent: %v", err)
	}

	set, err := newMemtableSet(RotationConfig{
		Dir:            dir,
		Threshold:      threshold,
		MaxImmutable:   maxImmutable,
		New:            newTestTable,
		NextFileNumber: vs.NextFileNumber,
	})
	if err != nil {
		t.Fatalf("newMemtableSet: %v", err)
	}
	t.Cleanup(func() { _ = set.Close() })

	return &flushEnv{dir: dir, set: set, vs: vs, log: log, f: NewFlusher(set, dir, log, vs)}
}

func (e *flushEnv) put(t *testing.T, key, value string) {
	t.Helper()
	if _, _, err := e.set.Add([]byte(key), []byte(value), false); err != nil {
		t.Fatalf("Add(%q): %v", key, err)
	}
}

func (e *flushEnv) del(t *testing.T, key string) {
	t.Helper()
	if _, _, err := e.set.Add([]byte(key), nil, true); err != nil {
		t.Fatalf("Delete(%q): %v", key, err)
	}
}

// walExists reports whether the WAL for a slot number is still on disk.
func (e *flushEnv) walExists(number uint64) bool {
	_, err := os.Stat(filepath.Join(e.dir, fmt.Sprintf("%06d.wal", number)))
	return err == nil
}

func (e *flushEnv) sstCount(t *testing.T) int {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(e.dir, "*.sst"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	return len(matches)
}

// TestFlushCommitsAndRetiresTheWAL walks one memtable all the way through:
// rotated, written, committed to the manifest, and only then retired.
func TestFlushCommitsAndRetiresTheWAL(t *testing.T) {
	env := newFlushEnv(t, 1, 4) // threshold 1 forces a rotation per write

	env.put(t, "alpha", "1")
	env.put(t, "beta", "2")

	sl, ok := env.set.Oldest()
	if !ok {
		t.Fatal("nothing queued after writes with a threshold of 1")
	}
	walNumber := sl.number
	if !env.walExists(walNumber) {
		t.Fatalf("WAL %d missing before the flush", walNumber)
	}

	if _, err := env.f.FlushOldest(); err != nil {
		t.Fatalf("FlushOldest: %v", err)
	}

	if env.walExists(walNumber) {
		t.Errorf("WAL %d still on disk after a committed flush", walNumber)
	}
	if got := env.vs.Current().NumFiles(0); got != 1 {
		t.Errorf("L0 holds %d files after one flush, want 1", got)
	}
	if got := env.sstCount(t); got != 1 {
		t.Errorf("%d .sst files on disk, want 1", got)
	}
}

// TestWALSurvivesUntilTheManifestCommits is T3.5's trap, stated directly.
//
// Between the SSTable fsync and the manifest fsync the data is on disk but
// nothing durable references it. If the WAL were deleted in that window a
// crash would lose acknowledged writes, because the SSTable is an orphan that
// startup sweeps and the WAL that protected the data is gone. The test stops
// the flush at exactly that instant and asserts the WAL is still there.
func TestWALSurvivesUntilTheManifestCommits(t *testing.T) {
	env := newFlushEnv(t, 1, 4)
	env.put(t, "alpha", "1")
	env.put(t, "beta", "2")

	sl, ok := env.set.Oldest()
	if !ok {
		t.Fatal("nothing queued")
	}
	walNumber := sl.number

	env.f.onStep = func(s flushStep) error {
		if s == StepAfterTableSync {
			// The dangerous window: table durable, manifest not yet written.
			if !env.walExists(walNumber) {
				t.Errorf("WAL %d was deleted after the SSTable fsync but before the manifest fsync", walNumber)
			}
			if got := env.vs.Current().NumFiles(0); got != 0 {
				t.Errorf("version already names %d files before the manifest commit", got)
			}
			return ErrFlushAborted
		}
		return nil
	}

	if _, err := env.f.FlushOldest(); !errors.Is(err, ErrFlushAborted) {
		t.Fatalf("FlushOldest = %v, want ErrFlushAborted", err)
	}

	// Having aborted before the commit point, the WAL must still protect the
	// data: this is the state a crash in that window would leave behind.
	if !env.walExists(walNumber) {
		t.Error("WAL is gone after an abort before the commit point")
	}
	if got := env.vs.Current().NumFiles(0); got != 0 {
		t.Errorf("version names %d files after an aborted flush, want 0", got)
	}
}

// TestAbortAfterManifestSyncLeavesCommittedData checks the other side of the
// commit point: once the manifest fsync returns, the flush has happened even
// if the process dies before the WAL is removed. The leftover WAL is
// redundant, not a loss.
func TestAbortAfterManifestSyncLeavesCommittedData(t *testing.T) {
	env := newFlushEnv(t, 1, 4)
	env.put(t, "alpha", "1")
	env.put(t, "beta", "2")

	env.f.onStep = func(s flushStep) error {
		if s == StepAfterManifestSync {
			return ErrFlushAborted
		}
		return nil
	}
	if _, err := env.f.FlushOldest(); !errors.Is(err, ErrFlushAborted) {
		t.Fatalf("FlushOldest = %v, want ErrFlushAborted", err)
	}

	// The commit happened, so a replay must see the file.
	vs, err := manifest.Recover(env.dir)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if got := vs.Current().NumFiles(0); got != 1 {
		t.Errorf("replayed version names %d L0 files, want 1", got)
	}
}

// TestFlushedDataIsReadableAfterReplay is the end-to-end claim: what the
// flusher committed is what a fresh process reads back, both from the
// manifest and from the table bytes themselves.
func TestFlushedDataIsReadableAfterReplay(t *testing.T) {
	env := newFlushEnv(t, 1<<12, 8)

	const n = 500
	want := map[string]string{}
	for i := 0; i < n; i++ {
		k, v := fmt.Sprintf("key-%04d", i), fmt.Sprintf("value-%04d", i)
		env.put(t, k, v)
		want[k] = v
	}
	if err := env.f.DrainQueue(); err != nil {
		t.Fatalf("DrainQueue: %v", err)
	}

	vs, err := manifest.Recover(env.dir)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	files := vs.Current().Files(0)
	if len(files) == 0 {
		t.Fatal("no L0 files after draining the queue")
	}

	// Every key that reached a flushed table must be readable from it. Keys
	// still in the active memtable are not on disk yet and are not the
	// flusher's claim to make, so only committed files are consulted.
	found := map[string]string{}
	for _, fm := range files {
		tbl, err := sstable.Open(filepath.Join(env.dir, fm.Name()))
		if err != nil {
			t.Fatalf("open %s: %v", fm.Name(), err)
		}
		it := tbl.NewIterator()
		for it.Next() {
			e := it.Entry()
			if !e.Tombstone {
				found[string(e.Key)] = string(e.Value)
			}
		}
		if err := it.Err(); err != nil {
			t.Fatalf("iterate %s: %v", fm.Name(), err)
		}
		_ = tbl.Close()
	}

	if len(found) == 0 {
		t.Fatal("committed tables held no entries")
	}
	for k, v := range found {
		if want[k] != v {
			t.Fatalf("key %q = %q, want %q", k, v, want[k])
		}
	}
}

// TestFlushProducesOneTablePerMemtable is the scaled form of T3.5's ratio
// claim. The done-when is 1 GB against a 4 MB memtable giving roughly 250
// tables; running that in a unit test would take minutes, so the same
// invariant -- one table per rotated memtable, total bytes over threshold --
// is measured at a size the suite can afford.
func TestFlushProducesOneTablePerMemtable(t *testing.T) {
	const (
		threshold = 32 << 10
		value     = 256
		n         = 2000
	)
	env := newFlushEnv(t, threshold, 4)

	env.f.Start()
	payload := make([]byte, value)
	for i := 0; i < n; i++ {
		env.put(t, fmt.Sprintf("key-%06d", i), string(payload))
		env.f.Trigger()
	}
	if err := env.f.Stop(); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := env.f.DrainQueue(); err != nil {
		t.Fatalf("DrainQueue: %v", err)
	}

	// Rotation happens when the memtable crosses the threshold, so the table
	// count tracks total bytes divided by the threshold. The bound is loose
	// on purpose: per-entry overhead and the final partial memtable both
	// move the exact figure, and pinning it would make the test brittle
	// without making it more meaningful.
	written := n * (value + len("key-000000") + perEntryOverhead)
	expected := written / threshold

	got := env.sstCount(t)
	if got < expected/2 || got > expected*2+2 {
		t.Errorf("%d tables for ~%d bytes at a %d threshold, want roughly %d",
			got, written, threshold, expected)
	}
	if got != int(env.f.Flushed()) {
		t.Errorf("%d tables on disk but Flushed() = %d", got, env.f.Flushed())
	}
	t.Logf("%d tables for %d bytes at a %d-byte threshold (expected ~%d)", got, written, threshold, expected)
}

// TestFlushMemoryStaysFlat is T3.5's memory claim: the flusher streams a
// memtable into a table rather than materialising it, and the queue bound
// caps how many memtables can be resident, so heap use must not track the
// dataset. Growth is compared between two runs an order of magnitude apart.
func TestFlushMemoryStaysFlat(t *testing.T) {
	if testing.Short() {
		t.Skip("allocation measurement is slow under -short")
	}

	measure := func(writes int) uint64 {
		env := newFlushEnv(t, 32<<10, 2)
		env.f.Start()

		payload := make([]byte, 256)
		for i := 0; i < writes; i++ {
			env.put(t, fmt.Sprintf("key-%08d", i), string(payload))
			env.f.Trigger()
		}
		_ = env.f.Stop()
		_ = env.f.DrainQueue()

		runtime.GC()
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		return ms.HeapAlloc
	}

	small := measure(1000)
	large := measure(10000)

	// Ten times the data must not cost anything like ten times the heap.
	// The threshold is deliberately generous: this is a guard against
	// retaining the dataset, not a precise budget.
	if large > small*3 {
		t.Errorf("heap grew from %d to %d bytes for 10x the writes, which suggests the dataset is being retained", small, large)
	}
	t.Logf("heap after 1k writes: %d bytes; after 10k writes: %d bytes", small, large)
}

// TestFlushOnEmptyQueueIsANoop checks the flusher does not invent work.
func TestFlushOnEmptyQueueIsANoop(t *testing.T) {
	env := newFlushEnv(t, 1<<20, 4)

	empty, err := env.f.FlushOldest()
	if err != nil {
		t.Fatalf("FlushOldest: %v", err)
	}
	if !empty {
		t.Error("FlushOldest reported work on an empty queue")
	}
	if got := env.sstCount(t); got != 0 {
		t.Errorf("%d tables written from an empty queue", got)
	}
}

// TestFlushReleasesStalledWriters checks the queue bound is actually
// released by a flush: writers stall when the queue is full, and completing a
// flush must let them through.
func TestFlushReleasesStalledWriters(t *testing.T) {
	env := newFlushEnv(t, 1, 1)

	env.put(t, "a", "1")
	env.put(t, "b", "2")

	done := make(chan error, 1)
	go func() {
		_, _, err := env.set.Add([]byte("c"), []byte("3"), false)
		done <- err
	}()

	// The writer above is stalled on a full queue. Draining must free it.
	if err := env.f.DrainQueue(); err != nil {
		t.Fatalf("DrainQueue: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("stalled writer failed: %v", err)
	}
}
