package engine

import (
	"errors"
	"fmt"
	"testing"

	"github.com/AbishekRaj2007/Strata/internal/compaction"
	"github.com/AbishekRaj2007/Strata/internal/manifest"
	"github.com/AbishekRaj2007/Strata/internal/wal"
)

// compactingEngine opens an engine tuned so a handful of small writes is
// enough to drive several flushes and several compactions.
func compactingEngine(t *testing.T, opts compaction.Options) *LSM {
	t.Helper()

	// Every compaction in these tests is followed by a full invariant
	// check. It is far too slow for production and exactly right here: a
	// violation is reported at the compaction that caused it rather than at
	// the read that eventually trips over it.
	opts.Verify = true

	e, err := Open(Options{
		Dir:        t.TempDir(),
		Threshold:  1 << 10,
		SyncPolicy: wal.SyncNever,
		Compaction: opts,
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = e.Close() })
	return e
}

func writeKeys(t *testing.T, e *LSM, n int, value string) {
	t.Helper()

	for i := 0; i < n; i++ {
		if err := e.Put([]byte(fmt.Sprintf("key%05d", i)), []byte(value)); err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
	}
}

// The end-to-end claim of Phase 6: writes flow through L0 into the levels
// below, and nothing is lost on the way.
func TestEngineCompactsL0IntoLowerLevels(t *testing.T) {
	e := compactingEngine(t, compaction.Options{
		L0Trigger:       2,
		BaseLevelBytes:  8 << 10,
		TargetFileBytes: 4 << 10,
	})

	const keys = 600
	writeKeys(t, e, keys, "the quick brown fox jumps over the lazy dog")

	if err := e.Compact(); err != nil {
		t.Fatalf("compact: %v", err)
	}

	// "Settled" means no level is over budget, not that L0 is empty: an L0
	// below its trigger is costing a reader less than a compaction would
	// cost to run, which is exactly the trade the trigger encodes.
	v := e.vs.Current()
	if v.NumFiles(0) >= 2 {
		t.Errorf("L0 holds %d files after settling, want fewer than the trigger of 2", v.NumFiles(0))
	}
	// Somewhere below L0, not L1 specifically: this much data puts L1 over
	// its own budget too, so a fully settled tree may have cascaded all of
	// it down to L2.
	below := 0
	for level := 1; level < manifest.NumLevels; level++ {
		below += v.NumFiles(level)
	}
	if below == 0 {
		t.Fatalf("nothing reached a level below L0; compaction never ran (shape %s)", levelShape(v))
	}
	if err := v.CheckInvariants(); err != nil {
		t.Errorf("the settled tree is invalid: %v", err)
	}

	for i := 0; i < keys; i++ {
		key := fmt.Sprintf("key%05d", i)
		got, err := e.Get([]byte(key))
		if err != nil {
			t.Fatalf("get %q after compaction: %v", key, err)
		}
		if string(got) != "the quick brown fox jumps over the lazy dog" {
			t.Fatalf("key %q = %q after compaction", key, got)
		}
	}

	st, err := e.Stats()
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if st.Compaction == nil || st.Compaction.Compactions == 0 {
		t.Errorf("compaction stats = %+v, want a non-zero count", st.Compaction)
	}
	if len(st.Levels) == 0 {
		t.Error("level stats are empty for a tree with files in it")
	}
}

// A deleted key must stay deleted through every level it passes, which is
// the engine-level statement of T6.2's rule.
func TestEngineKeepsDeletesThroughCompaction(t *testing.T) {
	e := compactingEngine(t, compaction.Options{
		L0Trigger:       2,
		BaseLevelBytes:  4 << 10,
		TargetFileBytes: 2 << 10,
	})

	const keys = 400
	writeKeys(t, e, keys, "original")

	// Delete every third key, then overwrite the rest, so the tree holds a
	// mixture of tombstones and superseded values at several levels.
	for i := 0; i < keys; i += 3 {
		if _, err := e.Delete([]byte(fmt.Sprintf("key%05d", i))); err != nil {
			t.Fatalf("delete %d: %v", i, err)
		}
	}
	for i := 1; i < keys; i += 3 {
		if err := e.Put([]byte(fmt.Sprintf("key%05d", i)), []byte("rewritten")); err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
	}

	if err := e.Compact(); err != nil {
		t.Fatalf("compact: %v", err)
	}

	for i := 0; i < keys; i++ {
		key := fmt.Sprintf("key%05d", i)
		got, err := e.Get([]byte(key))

		switch i % 3 {
		case 0:
			if !errors.Is(err, ErrNotFound) {
				t.Fatalf("deleted key %q came back as %q (err %v)", key, got, err)
			}
		case 1:
			if err != nil || string(got) != "rewritten" {
				t.Fatalf("key %q = %q (err %v), want \"rewritten\"", key, got, err)
			}
		default:
			if err != nil || string(got) != "original" {
				t.Fatalf("key %q = %q (err %v), want \"original\"", key, got, err)
			}
		}
	}
}

// Compaction must survive a restart: the manifest is the only record of what
// it did, and a reopened engine has to agree with the one that closed.
func TestCompactedTreeSurvivesReopen(t *testing.T) {
	dir := t.TempDir()
	opts := Options{
		Dir:        dir,
		Threshold:  1 << 10,
		SyncPolicy: wal.SyncAlways,
		Compaction: compaction.Options{L0Trigger: 2, BaseLevelBytes: 8 << 10, TargetFileBytes: 4 << 10, Verify: true},
	}

	e, err := Open(opts)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	const keys = 400
	writeKeys(t, e, keys, "durable")
	if err := e.Compact(); err != nil {
		t.Fatalf("compact: %v", err)
	}

	before := levelShape(e.vs.Current())
	if err := e.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	reopened, err := Open(opts)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer func() { _ = reopened.Close() }()

	if after := levelShape(reopened.vs.Current()); after != before {
		t.Errorf("tree shape after reopen = %s, want %s", after, before)
	}
	for i := 0; i < keys; i++ {
		key := fmt.Sprintf("key%05d", i)
		got, err := reopened.Get([]byte(key))
		if err != nil || string(got) != "durable" {
			t.Fatalf("key %q = %q (err %v) after reopen", key, got, err)
		}
	}
}

func levelShape(v *manifest.Version) string {
	shape := ""
	for level := 0; level < manifest.NumLevels; level++ {
		if v.NumFiles(level) > 0 {
			shape += fmt.Sprintf("L%d=%d ", level, v.NumFiles(level))
		}
	}
	return shape
}

// Closing while compaction is in flight must be clean, and must not leave
// debris the next open has to reason about.
func TestCloseDuringCompactionIsClean(t *testing.T) {
	dir := t.TempDir()
	opts := Options{
		Dir:        dir,
		Threshold:  1 << 10,
		SyncPolicy: wal.SyncNever,
		Compaction: compaction.Options{L0Trigger: 1, BaseLevelBytes: 1 << 10, TargetFileBytes: 1 << 10, Verify: true},
	}

	e, err := Open(opts)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	writeKeys(t, e, 800, "busy")

	// Close without settling, so the compactor is very likely mid-merge.
	if err := e.Close(); err != nil {
		t.Fatalf("close during compaction: %v", err)
	}

	reopened, err := Open(opts)
	if err != nil {
		t.Fatalf("reopen after a close during compaction: %v", err)
	}
	defer func() { _ = reopened.Close() }()

	if err := reopened.vs.Current().CheckInvariants(); err != nil {
		t.Errorf("the reopened tree is invalid: %v", err)
	}
	for i := 0; i < 800; i++ {
		key := fmt.Sprintf("key%05d", i)
		if _, err := reopened.Get([]byte(key)); err != nil {
			t.Fatalf("key %q was lost by a close during compaction: %v", key, err)
		}
	}
}
