package engine

import (
	"errors"
	"testing"

	"github.com/AbishekRaj2007/Strata/internal/vfs"
	"github.com/AbishekRaj2007/Strata/internal/wal"
)

// TestFailedSyncPoisonsTheEngine is T7.2's trap. A failed fsync is not a
// failure that can be retried: on Linux the error is reported once and the
// dirty page is dropped, so the second attempt succeeds while the data is
// gone. The engine must refuse instead.
func TestFailedSyncPoisonsTheEngine(t *testing.T) {
	dir := t.TempDir()
	in := vfs.NewInjector(nil)

	e, err := Open(Options{Dir: dir, SyncPolicy: wal.SyncAlways, FS: in})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = e.Close() }()

	if err := e.Put([]byte("before"), []byte("v")); err != nil {
		t.Fatalf("Put before the fault: %v", err)
	}

	// Arm the very next operation. Under SyncAlways the write path's next
	// I/O after the WAL append is the fsync, so this lands on it.
	armNextSync(t, in, e)

	if err := e.Put([]byte("during"), []byte("v")); !errors.Is(err, wal.ErrSyncFailed) {
		t.Fatalf("Put during the fault returned %v, want a failed fsync", err)
	}

	// The second write must be refused even though the filesystem is healthy
	// again. This is the whole point: healthy is not the same as known-good.
	err = e.Put([]byte("after"), []byte("v"))
	if !errors.Is(err, ErrUnrecoverable) {
		t.Errorf("Put after the fault returned %v, want ErrUnrecoverable", err)
	}
	if _, err := e.Get([]byte("before")); !errors.Is(err, ErrUnrecoverable) {
		t.Errorf("Get after the fault returned %v, want ErrUnrecoverable", err)
	}
	if _, err := e.Delete([]byte("before")); !errors.Is(err, ErrUnrecoverable) {
		t.Errorf("Delete after the fault returned %v, want ErrUnrecoverable", err)
	}

	// Reopening is the recovery, and it has to actually work: startup reads
	// what is genuinely on disk, so the database comes back at the last
	// durable state rather than the one this process believed in.
	if err := e.Close(); err != nil && !errors.Is(err, wal.ErrSyncFailed) {
		t.Fatalf("Close after the fault: %v", err)
	}
	reopened, err := Open(Options{Dir: dir, SyncPolicy: wal.SyncAlways})
	if err != nil {
		t.Fatalf("reopen after an unrecoverable failure: %v", err)
	}
	defer func() { _ = reopened.Close() }()

	if _, err := reopened.Get([]byte("before")); err != nil {
		t.Errorf("the write acknowledged before the fault did not survive: %v", err)
	}

	// "during" was never acknowledged -- its Put returned wal.ErrSyncFailed,
	// not nil -- so it must not be readable after recovery. Its bytes reached
	// the WAL file via a real WriteAt before the injected fsync failed, and
	// are, by content alone, indistinguishable from a genuinely durable
	// record: same framing, same checksum. Nothing but an explicit truncate
	// on the failure path removes them. Before that truncate was wired in,
	// this assertion failed silently: "during" came back readable, having
	// overwritten nothing here only because it was a new key rather than an
	// overwrite of "before" -- test/fault's sweep is what caught the
	// overwrite case, on a key reused across many writes.
	if _, err := reopened.Get([]byte("during")); !errors.Is(err, ErrNotFound) {
		t.Errorf("the write whose fsync failed came back after recovery (err=%v), want ErrNotFound -- "+
			"its WAL record was written but never truncated away", err)
	}

	if err := reopened.Put([]byte("after-restart"), []byte("v")); err != nil {
		t.Errorf("the reopened engine is still refusing writes: %v", err)
	}
}

// armNextSync arms the injector on the next fsync the engine performs.
//
// It finds the index by tracing one write rather than by counting the write
// path's I/O by hand, because a count written down here would silently stop
// being the fsync the first time anything on that path changed.
func armNextSync(t *testing.T, in *vfs.Injector, e *LSM) {
	t.Helper()

	in.Trace(true)
	before := in.Count()
	if err := e.Put([]byte("probe"), []byte("v")); err != nil {
		t.Fatalf("probe write: %v", err)
	}
	trace := in.Trace(false)

	for _, rec := range trace {
		if rec.Index >= before && rec.Kind == vfs.OpSync {
			// The next write repeats the same sequence, so the fsync lands
			// this far past where the counter is now.
			in.ArmAt(in.Count()+(rec.Index-before), vfs.Fault{})
			return
		}
	}
	t.Fatalf("no fsync in a SyncAlways write path: %v", trace)
}
