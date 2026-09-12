package compaction

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/AbishekRaj2007/Strata/internal/manifest"
	"github.com/AbishekRaj2007/Strata/internal/memtable"
	"github.com/AbishekRaj2007/Strata/internal/sstable"
)

// This file builds the resurrection bug on purpose, before the executor that
// must not have it.
//
// T6.2's trap is that the failure is silent, rare and catastrophic: a
// tombstone dropped one level too early does not error, does not corrupt
// anything a checksum would catch, and only shows up as a deleted key
// returning data -- possibly months later, when the older version finally
// gets compacted into view. Casual testing will not find it. So the rule is
// pinned from both sides here: the wrong policy is run and shown to
// resurrect the key, and the right policy is run and shown not to.
//
// Keeping the wrong policy in the tree is deliberate. It is what makes the
// right one an assertion about behaviour rather than a comment nobody checks.

// entry is shorthand for a live version of a key.
func entry(key string, seq uint64, value string) memtable.Entry {
	return memtable.Entry{Key: []byte(key), Sequence: seq, Value: []byte(value)}
}

// deletion is shorthand for a tombstone.
func deletion(key string, seq uint64) memtable.Entry {
	return memtable.Entry{Key: []byte(key), Sequence: seq, Tombstone: true}
}

// writeTable builds a real SSTable from entries and returns the metadata a
// version would carry for it. The entries are sorted by the skip list, so
// callers may pass them in any order.
func writeTable(t *testing.T, dir string, number uint64, entries []memtable.Entry) *manifest.FileMetadata {
	t.Helper()

	sl := memtable.NewSkipList()
	for _, e := range entries {
		if err := sl.Insert(e); err != nil {
			t.Fatalf("insert %q: %v", e.Key, err)
		}
	}

	info, err := sstable.WriteTable(dir, number, sl.NewIterator())
	if err != nil {
		t.Fatalf("write table %d: %v", number, err)
	}

	return &manifest.FileMetadata{
		Number:      number,
		Size:        uint64(info.Size),
		Smallest:    info.SmallestKey,
		Largest:     info.LargestKey,
		SmallestSeq: info.SmallestSeq,
		LargestSeq:  info.LargestSeq,
	}
}

// mergeTables merges the inputs into one output table, dropping tombstones if
// asked. It is the minimum of a compaction -- no rolling, no commit -- so
// that the tombstone rule can be exercised on its own, with the policy as a
// parameter rather than as whatever the executor happens to do.
func mergeTables(t *testing.T, dir string, inputs []*manifest.FileMetadata, number uint64, dropTombstones bool) *manifest.FileMetadata {
	t.Helper()

	var sources []memtable.Iterator
	for _, f := range inputs {
		tbl, err := sstable.Open(filepath.Join(dir, f.Name()))
		if err != nil {
			t.Fatalf("open %s: %v", f.Name(), err)
		}
		defer func() { _ = tbl.Close() }()
		sources = append(sources, tbl.NewIterator())
	}

	it := sstable.NewMergeIterator(sources, dropTombstones)
	info, err := sstable.WriteTable(dir, number, it)
	if err != nil {
		t.Fatalf("write merged table %d: %v", number, err)
	}
	if err := it.Err(); err != nil {
		t.Fatalf("merge: %v", err)
	}

	return &manifest.FileMetadata{
		Number:      number,
		Size:        uint64(info.Size),
		Smallest:    info.SmallestKey,
		Largest:     info.LargestKey,
		SmallestSeq: info.SmallestSeq,
		LargestSeq:  info.LargestSeq,
	}
}

// readLevels walks levels top-down the way the engine's read path does,
// stopping at the first version of the key it finds -- tombstone included.
// It returns the value and whether the key is visible.
func readLevels(t *testing.T, dir string, levels [][]*manifest.FileMetadata, key string) (string, bool) {
	t.Helper()

	for _, files := range levels {
		for _, f := range files {
			tbl, err := sstable.Open(filepath.Join(dir, f.Name()))
			if err != nil {
				t.Fatalf("open %s: %v", f.Name(), err)
			}
			e, found, err := tbl.Get([]byte(key))
			_ = tbl.Close()
			if err != nil {
				t.Fatalf("get %q from %s: %v", key, f.Name(), err)
			}
			if !found {
				continue
			}
			if e.Tombstone {
				return "", false
			}
			return string(e.Value), true
		}
	}
	return "", false
}

// scenario is the shape every resurrection test needs: an old value hidden at
// a deep level, and a tombstone above it that is the only thing keeping it
// hidden.
type scenario struct {
	dir string

	l1 []*manifest.FileMetadata // the tombstone
	l2 []*manifest.FileMetadata // the compaction's output level, initially empty
	l3 []*manifest.FileMetadata // the old value nobody must see again
}

func newScenario(t *testing.T) *scenario {
	t.Helper()
	dir := t.TempDir()

	return &scenario{
		dir: dir,
		l3:  []*manifest.FileMetadata{writeTable(t, dir, 1, []memtable.Entry{entry("k", 1, "the old value")})},
		l1:  []*manifest.FileMetadata{writeTable(t, dir, 2, []memtable.Entry{deletion("k", 2)})},
	}
}

func (s *scenario) levels() [][]*manifest.FileMetadata {
	return [][]*manifest.FileMetadata{s.l1, s.l2, s.l3}
}

// TestDroppingATombstoneEarlyResurrectsTheKey is the bug, built deliberately.
//
// L1 holds a tombstone for "k" and L3 still holds the value it deletes.
// Compacting L1 into L2 with tombstones dropped -- the policy that looks
// obviously right, since a tombstone carries no data -- removes the only
// record that the key was deleted. The read path then walks past an empty L2
// and finds the L3 value, which the user deleted.
func TestDroppingATombstoneEarlyResurrectsTheKey(t *testing.T) {
	s := newScenario(t)

	if _, visible := readLevels(t, s.dir, s.levels(), "k"); visible {
		t.Fatal("the key is visible before the compaction; the scenario is not set up")
	}

	// The wrong rule: drop tombstones without asking whether anything below
	// could still be shadowed by them.
	s.l2 = []*manifest.FileMetadata{mergeTables(t, s.dir, s.l1, 3, true)}
	s.l1 = nil

	value, visible := readLevels(t, s.dir, s.levels(), "k")
	if !visible {
		t.Fatal("dropping the tombstone no longer resurrects the key -- the failure this " +
			"test exists to demonstrate is gone, and the rule below is no longer pinned to anything")
	}
	t.Logf("resurrected: a deleted key reads back as %q", value)
}

// TestKeepingATombstoneAboveTheBottomPreservesTheDelete is the rule.
//
// The same compaction, with the tombstone kept because L3 overlaps the
// compaction's key range, leaves the delete visible. The tombstone travels
// down with the data it shadows and is discarded only when it reaches a level
// below which nothing can hide.
func TestKeepingATombstoneAboveTheBottomPreservesTheDelete(t *testing.T) {
	s := newScenario(t)

	s.l2 = []*manifest.FileMetadata{mergeTables(t, s.dir, s.l1, 3, false)}
	s.l1 = nil

	if value, visible := readLevels(t, s.dir, s.levels(), "k"); visible {
		t.Errorf("the deleted key reads back as %q", value)
	}
}

// TestDroppingATombstoneAtTheBottomIsSafe is the other half of the rule: a
// tombstone that reaches a level with nothing below it is shadowing nothing,
// and keeping it forever would mean deletes never reclaim their space.
func TestDroppingATombstoneAtTheBottomIsSafe(t *testing.T) {
	dir := t.TempDir()

	// Only one level holds the key, so the compaction of it is bottom-most.
	f := writeTable(t, dir, 1, []memtable.Entry{
		entry("a", 1, "kept"),
		deletion("k", 2),
	})

	out := mergeTables(t, dir, []*manifest.FileMetadata{f}, 2, true)

	levels := [][]*manifest.FileMetadata{{out}}
	if _, visible := readLevels(t, dir, levels, "k"); visible {
		t.Error("the deleted key came back")
	}
	if v, visible := readLevels(t, dir, levels, "a"); !visible || v != "kept" {
		t.Errorf("live key = %q, %v; want \"kept\", true", v, visible)
	}

	// The point of dropping it: the tombstone is not merely invisible, it is
	// gone from the file.
	tbl, err := sstable.Open(filepath.Join(dir, out.Name()))
	if err != nil {
		t.Fatalf("open output: %v", err)
	}
	defer func() { _ = tbl.Close() }()

	var keys []string
	for it := tbl.NewIterator(); it.Next(); {
		keys = append(keys, string(it.Entry().Key))
	}
	if fmt.Sprint(keys) != "[a]" {
		t.Errorf("output holds %v, want just [a]: the tombstone was not reclaimed", keys)
	}
}
