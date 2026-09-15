package engine

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/AbishekRaj2007/Strata/internal/wal"
)

func openLSM(t *testing.T, dir string, threshold int) *LSM {
	t.Helper()
	e, err := Open(Options{Dir: dir, Threshold: threshold, MaxImmutable: 4, SyncPolicy: wal.SyncAlways})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return e
}

func TestLSMBasicRoundTrip(t *testing.T) {
	e := openLSM(t, t.TempDir(), 1<<20)
	defer e.Close()

	if err := e.Put([]byte("k"), []byte("v")); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, err := e.Get([]byte("k"))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got) != "v" {
		t.Errorf("Get = %q, want \"v\"", got)
	}

	if _, err := e.Get([]byte("absent")); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get(absent) = %v, want ErrNotFound", err)
	}

	existed, err := e.Delete([]byte("k"))
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if !existed {
		t.Error("Delete reported the key did not exist")
	}
	if _, err := e.Get([]byte("k")); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get after Delete = %v, want ErrNotFound", err)
	}

	existed, err = e.Delete([]byte("k"))
	if err != nil {
		t.Fatalf("second Delete: %v", err)
	}
	if existed {
		t.Error("deleting an already-deleted key reported it existed")
	}
}

// TestLSMDataSurvivesReopen is the point of the whole engine: what was
// acknowledged before Close must be readable after a fresh Open, whether it
// was still in a memtable or had already been flushed.
func TestLSMDataSurvivesReopen(t *testing.T) {
	dir := t.TempDir()

	// A small threshold so some of this flushes and some does not, exercising
	// both the SSTable path and the WAL-replay path in one run.
	e := openLSM(t, dir, 4<<10)

	const n = 500
	want := map[string]string{}
	for i := 0; i < n; i++ {
		k, v := fmt.Sprintf("key-%04d", i), fmt.Sprintf("value-%04d", i)
		if err := e.Put([]byte(k), []byte(v)); err != nil {
			t.Fatalf("Put %d: %v", i, err)
		}
		want[k] = v
	}
	// Delete a slice of them, so tombstones have to survive too.
	for i := 0; i < n; i += 7 {
		k := fmt.Sprintf("key-%04d", i)
		if _, err := e.Delete([]byte(k)); err != nil {
			t.Fatalf("Delete %d: %v", i, err)
		}
		delete(want, k)
	}
	if err := e.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened := openLSM(t, dir, 4<<10)
	defer reopened.Close()

	for k, v := range want {
		got, err := reopened.Get([]byte(k))
		if err != nil {
			t.Fatalf("Get(%q) after reopen: %v", k, err)
		}
		if string(got) != v {
			t.Fatalf("Get(%q) = %q, want %q", k, got, v)
		}
	}
	for i := 0; i < n; i += 7 {
		k := fmt.Sprintf("key-%04d", i)
		if _, err := reopened.Get([]byte(k)); !errors.Is(err, ErrNotFound) {
			t.Fatalf("deleted key %q came back after reopen: %v", k, err)
		}
	}
}

// TestLSMRecoversFromWALWithoutAcleanClose simulates a crash: the engine is
// abandoned without Close, so nothing is flushed and the WAL is the only
// record of the writes. Reopening must replay it.
func TestLSMRecoversFromWALWithoutACleanClose(t *testing.T) {
	dir := t.TempDir()

	e := openLSM(t, dir, 1<<30) // nothing will ever flush
	for i := 0; i < 50; i++ {
		if err := e.Put([]byte(fmt.Sprintf("k%02d", i)), []byte(fmt.Sprintf("v%02d", i))); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	// Deliberately no Close: the writes exist only in the WAL. The directory
	// lock is released by hand, because a real crash drops it too -- the
	// kernel releases every flock an exiting process held -- and this test
	// simulates the crash within one process rather than across two.
	if err := e.lock.unlock(); err != nil {
		t.Fatalf("unlock: %v", err)
	}

	reopened := openLSM(t, dir, 1<<30)
	defer reopened.Close()

	for i := 0; i < 50; i++ {
		got, err := reopened.Get([]byte(fmt.Sprintf("k%02d", i)))
		if err != nil {
			t.Fatalf("Get(k%02d) after recovery: %v", i, err)
		}
		if string(got) != fmt.Sprintf("v%02d", i) {
			t.Errorf("k%02d = %q, want v%02d", i, got, i)
		}
	}
}

// TestLSMSweepsOrphanedTables checks the startup sweep: a table on disk that
// no committed version names is the debris of a crash between the SSTable
// fsync and the manifest fsync, and must be removed.
func TestLSMSweepsOrphanedTables(t *testing.T) {
	dir := t.TempDir()

	e := openLSM(t, dir, 1<<20)
	if err := e.Put([]byte("k"), []byte("v")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := e.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Plant a table the manifest has never heard of.
	orphan := filepath.Join(dir, "999999.sst")
	if err := os.WriteFile(orphan, []byte("not a real table"), 0o600); err != nil {
		t.Fatalf("plant orphan: %v", err)
	}

	reopened := openLSM(t, dir, 1<<20)
	defer reopened.Close()

	if _, err := os.Stat(orphan); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("orphaned table survived startup: %v", err)
	}

	// And the real data is still there.
	if got, err := reopened.Get([]byte("k")); err != nil || string(got) != "v" {
		t.Errorf("Get(k) = %q, %v; want \"v\", nil", got, err)
	}
}

// TestLSMSequenceNumbersContinueAfterRecovery checks the recovered engine
// does not restart its sequence counter. Reusing a sequence would let a new
// write lose to an old one under the comparator.
func TestLSMSequenceNumbersContinueAfterRecovery(t *testing.T) {
	dir := t.TempDir()

	e := openLSM(t, dir, 1<<30)
	for i := 0; i < 20; i++ {
		if err := e.Put([]byte("k"), []byte(fmt.Sprintf("v%d", i))); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	before, err := e.Stats()
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	_ = e.Close()

	reopened := openLSM(t, dir, 1<<30)
	defer reopened.Close()

	after, err := reopened.Stats()
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if after.Memtable.Sequence < before.Memtable.Sequence {
		t.Errorf("sequence went backwards across recovery: %d then %d",
			before.Memtable.Sequence, after.Memtable.Sequence)
	}

	// A write after recovery must win over everything replayed.
	if err := reopened.Put([]byte("k"), []byte("newest")); err != nil {
		t.Fatalf("Put after recovery: %v", err)
	}
	got, err := reopened.Get([]byte("k"))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got) != "newest" {
		t.Errorf("Get = %q, want \"newest\" -- a replayed write outranked a newer one", got)
	}
}

// TestLSMCloseIsIdempotent checks the interface's contract that Close may be
// called repeatedly and that the engine refuses work afterwards.
func TestLSMCloseIsIdempotent(t *testing.T) {
	e := openLSM(t, t.TempDir(), 1<<20)

	if err := e.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := e.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if err := e.Put([]byte("k"), []byte("v")); !errors.Is(err, ErrClosed) {
		t.Errorf("Put after Close = %v, want ErrClosed", err)
	}
	if _, err := e.Get([]byte("k")); !errors.Is(err, ErrClosed) {
		t.Errorf("Get after Close = %v, want ErrClosed", err)
	}
}

// TestLSMOpenOnAFreshDirectory checks a directory that has never held a
// database comes up clean, with a manifest and CURRENT laid down.
func TestLSMOpenOnAFreshDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "does-not-exist-yet")

	e := openLSM(t, dir, 1<<20)
	defer e.Close()

	if _, err := os.Stat(filepath.Join(dir, "CURRENT")); err != nil {
		t.Errorf("CURRENT was not created: %v", err)
	}
	if _, err := e.Get([]byte("anything")); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get on an empty database = %v, want ErrNotFound", err)
	}
}
