package sstable

import (
	"fmt"
	"math/rand"
	"os"
	"sort"
	"testing"

	"github.com/AbishekRaj2007/Strata/internal/memtable"
)

// collect drains a merge iterator, failing on any source error.
func collectMerged(t *testing.T, m *MergeIterator) []memtable.Entry {
	t.Helper()
	var got []memtable.Entry
	for m.Next() {
		got = append(got, m.Entry())
	}
	if err := m.Err(); err != nil {
		t.Fatalf("merge error: %v", err)
	}
	return got
}

// reference is the model the merge is checked against: sort every entry by
// the canonical comparator, then keep the first occurrence of each user key.
// It is deliberately the slow, obviously-correct formulation.
func reference(all []memtable.Entry, skipTombstones bool) []memtable.Entry {
	sorted := append([]memtable.Entry(nil), all...)
	sort.SliceStable(sorted, func(i, j int) bool {
		return memtable.Compare(sorted[i].Key, sorted[i].Sequence, sorted[j].Key, sorted[j].Sequence) < 0
	})

	var out []memtable.Entry
	seen := map[string]bool{}
	for _, e := range sorted {
		if seen[string(e.Key)] {
			continue
		}
		seen[string(e.Key)] = true
		if skipTombstones && e.Tombstone {
			continue
		}
		out = append(out, e)
	}
	return out
}

func assertSameEntries(t *testing.T, got, want []memtable.Entry) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("merged %d entries, want %d", len(got), len(want))
	}
	for i := range got {
		if string(got[i].Key) != string(want[i].Key) ||
			got[i].Sequence != want[i].Sequence ||
			got[i].Tombstone != want[i].Tombstone ||
			string(got[i].Value) != string(want[i].Value) {
			t.Fatalf("entry %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// sortEntriesForSource puts one source's entries into comparator order, which
// every source must satisfy before it is merged.
func sortEntriesForSource(entries []memtable.Entry) {
	sort.Slice(entries, func(i, j int) bool {
		return memtable.Compare(entries[i].Key, entries[i].Sequence, entries[j].Key, entries[j].Sequence) < 0
	})
}

func TestMergeSingleSourcePassesThrough(t *testing.T) {
	entries := []memtable.Entry{
		{Key: []byte("a"), Sequence: 1, Value: []byte("1")},
		{Key: []byte("b"), Sequence: 2, Value: []byte("2")},
	}
	m := NewMergeIterator([]memtable.Iterator{newSliceIterator(entries)}, false)
	assertSameEntries(t, collectMerged(t, m), entries)
}

func TestMergeKeepsTheNewestVersion(t *testing.T) {
	// The same key at three sequences, spread across three sources so that no
	// single source holds the answer.
	sources := []memtable.Iterator{
		newSliceIterator([]memtable.Entry{{Key: []byte("k"), Sequence: 1, Value: []byte("old")}}),
		newSliceIterator([]memtable.Entry{{Key: []byte("k"), Sequence: 9, Value: []byte("newest")}}),
		newSliceIterator([]memtable.Entry{{Key: []byte("k"), Sequence: 5, Value: []byte("middle")}}),
	}

	got := collectMerged(t, NewMergeIterator(sources, false))
	if len(got) != 1 {
		t.Fatalf("merged %d entries for one key, want 1", len(got))
	}
	if string(got[0].Value) != "newest" || got[0].Sequence != 9 {
		t.Errorf("got %+v, want the sequence-9 version", got[0])
	}
}

// TestMergeSkipsTombstonesAndWhatTheyShadow checks the subtle half of
// tombstone suppression: hiding the delete is not enough, the older versions
// it shadows must be hidden too, or a deleted key reappears with a stale
// value.
func TestMergeSkipsTombstonesAndWhatTheyShadow(t *testing.T) {
	sources := []memtable.Iterator{
		newSliceIterator([]memtable.Entry{{Key: []byte("gone"), Sequence: 7, Tombstone: true}}),
		newSliceIterator([]memtable.Entry{{Key: []byte("gone"), Sequence: 3, Value: []byte("stale")}}),
		newSliceIterator([]memtable.Entry{{Key: []byte("kept"), Sequence: 4, Value: []byte("v")}}),
	}

	got := collectMerged(t, NewMergeIterator(sources, true))
	if len(got) != 1 {
		t.Fatalf("merged %d entries, want 1", len(got))
	}
	if string(got[0].Key) != "kept" {
		t.Errorf("surviving key = %q, want \"kept\"", got[0].Key)
	}
}

// TestMergeSurfacesTombstonesWhenAsked is the compaction-facing behaviour:
// with suppression off, the tombstone itself must come through, because
// dropping it above the bottom level resurrects deleted data.
func TestMergeSurfacesTombstonesWhenAsked(t *testing.T) {
	sources := []memtable.Iterator{
		newSliceIterator([]memtable.Entry{{Key: []byte("gone"), Sequence: 7, Tombstone: true}}),
		newSliceIterator([]memtable.Entry{{Key: []byte("gone"), Sequence: 3, Value: []byte("stale")}}),
	}

	got := collectMerged(t, NewMergeIterator(sources, false))
	if len(got) != 1 {
		t.Fatalf("merged %d entries, want 1", len(got))
	}
	if !got[0].Tombstone || got[0].Sequence != 7 {
		t.Errorf("got %+v, want the sequence-7 tombstone", got[0])
	}
}

// TestMergeTenSourcesWithHeavyOverlap is T4.4's done-when: ten sources whose
// keys overlap heavily must produce exactly the right deduplicated sequence,
// checked against a sorted reference rather than against intuition.
func TestMergeTenSourcesWithHeavyOverlap(t *testing.T) {
	const (
		sourceCount = 10
		perSource   = 500
		keySpace    = 300 // far smaller than the entry count, forcing overlap
	)
	rng := rand.New(rand.NewSource(42))

	var all []memtable.Entry
	var sources []memtable.Iterator
	seq := uint64(0)

	for s := 0; s < sourceCount; s++ {
		var entries []memtable.Entry
		seen := map[string]bool{}
		for i := 0; i < perSource; i++ {
			key := fmt.Sprintf("key-%04d", rng.Intn(keySpace))
			if seen[key] {
				continue // one version per key per source, as a real table has
			}
			seen[key] = true
			seq++
			entries = append(entries, memtable.Entry{
				Key:       []byte(key),
				Sequence:  seq,
				Value:     []byte(fmt.Sprintf("v%d", seq)),
				Tombstone: rng.Intn(5) == 0,
			})
		}
		sortEntriesForSource(entries)
		all = append(all, entries...)
		sources = append(sources, newSliceIterator(entries))
	}

	got := collectMerged(t, NewMergeIterator(sources, false))
	assertSameEntries(t, got, reference(all, false))

	// The output must also be strictly increasing in user key: one entry per
	// distinct key, in order.
	for i := 1; i < len(got); i++ {
		if string(got[i-1].Key) >= string(got[i].Key) {
			t.Fatalf("output not strictly ascending at %d: %q then %q", i, got[i-1].Key, got[i].Key)
		}
	}
	t.Logf("%d entries across %d sources merged to %d distinct keys", len(all), sourceCount, len(got))
}

// TestMergeTenSourcesSkippingTombstones runs the same overlap case through
// the read path's configuration.
func TestMergeTenSourcesSkippingTombstones(t *testing.T) {
	rng := rand.New(rand.NewSource(7))

	var all []memtable.Entry
	var sources []memtable.Iterator
	seq := uint64(0)

	for s := 0; s < 10; s++ {
		var entries []memtable.Entry
		seen := map[string]bool{}
		for i := 0; i < 400; i++ {
			key := fmt.Sprintf("key-%04d", rng.Intn(250))
			if seen[key] {
				continue
			}
			seen[key] = true
			seq++
			entries = append(entries, memtable.Entry{
				Key:       []byte(key),
				Sequence:  seq,
				Value:     []byte(fmt.Sprintf("v%d", seq)),
				Tombstone: rng.Intn(3) == 0,
			})
		}
		sortEntriesForSource(entries)
		all = append(all, entries...)
		sources = append(sources, newSliceIterator(entries))
	}

	assertSameEntries(t, collectMerged(t, NewMergeIterator(sources, true)), reference(all, true))
}

func TestMergeHandlesEmptyAndNilSources(t *testing.T) {
	sources := []memtable.Iterator{
		nil,
		newSliceIterator(nil),
		newSliceIterator([]memtable.Entry{{Key: []byte("a"), Sequence: 1, Value: []byte("1")}}),
		nil,
	}

	got := collectMerged(t, NewMergeIterator(sources, false))
	if len(got) != 1 || string(got[0].Key) != "a" {
		t.Fatalf("got %+v, want the single entry a", got)
	}
}

func TestMergeOfNothingIsEmpty(t *testing.T) {
	m := NewMergeIterator(nil, false)
	if m.Next() {
		t.Error("merge of no sources produced an entry")
	}
	if err := m.Err(); err != nil {
		t.Errorf("Err = %v, want nil", err)
	}
}

// TestMergeMixesRealTablesAndMemtables is the configuration the read path
// actually uses: a memtable iterator alongside real SSTable iterators, merged
// under one comparator.
func TestMergeMixesRealTablesAndMemtables(t *testing.T) {
	dir := t.TempDir()

	// An older table on disk.
	older := []memtable.Entry{
		{Key: []byte("a"), Sequence: 1, Value: []byte("disk-a")},
		{Key: []byte("b"), Sequence: 2, Value: []byte("disk-b")},
	}
	info, err := WriteTable(dir, 1, newSliceIterator(older))
	if err != nil {
		t.Fatalf("WriteTable: %v", err)
	}
	tbl, err := Open(info.Path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer tbl.Close()

	// A newer in-memory version of b, plus a new key.
	mem := memtable.NewSkipList()
	for _, e := range []memtable.Entry{
		{Key: []byte("b"), Sequence: 9, Value: []byte("mem-b")},
		{Key: []byte("c"), Sequence: 10, Value: []byte("mem-c")},
	} {
		if err := mem.Insert(e); err != nil {
			t.Fatalf("Insert: %v", err)
		}
	}

	got := collectMerged(t, NewMergeIterator([]memtable.Iterator{mem.NewIterator(), tbl.NewIterator()}, false))

	want := []memtable.Entry{
		{Key: []byte("a"), Sequence: 1, Value: []byte("disk-a")},
		{Key: []byte("b"), Sequence: 9, Value: []byte("mem-b")},
		{Key: []byte("c"), Sequence: 10, Value: []byte("mem-c")},
	}
	assertSameEntries(t, got, want)
}

// TestMergePropagatesSourceErrors checks a corrupt source stops the merge and
// is reported, rather than being mistaken for the end of the stream.
func TestMergePropagatesSourceErrors(t *testing.T) {
	dir := t.TempDir()
	var entries []memtable.Entry
	for i := 0; i < 200; i++ {
		entries = append(entries, memtable.Entry{
			Key:      []byte(fmt.Sprintf("k%04d", i)),
			Sequence: 1,
			Value:    []byte("value"),
		})
	}
	info, err := WriteTable(dir, 1, newSliceIterator(entries))
	if err != nil {
		t.Fatalf("WriteTable: %v", err)
	}

	// Corrupt a data block: the footer and index stay intact so Open still
	// succeeds and the damage surfaces during iteration.
	raw, err := os.ReadFile(info.Path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	raw[64] ^= 0xFF
	if err := os.WriteFile(info.Path, raw, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	tbl, err := Open(info.Path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer tbl.Close()

	m := NewMergeIterator([]memtable.Iterator{tbl.NewIterator()}, false)
	for m.Next() {
	}
	if m.Err() == nil {
		t.Fatal("merge over a corrupted table reported a clean end of stream")
	}
}
