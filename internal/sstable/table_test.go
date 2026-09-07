package sstable

import (
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/AbishekRaj2007/Strata/internal/memtable"
)

// sliceIterator feeds a sorted slice to WriteTable, standing in for a
// memtable iterator without depending on internal/memtable's skip list.
type sliceIterator struct {
	entries []memtable.Entry
	pos     int
}

func (it *sliceIterator) Next() bool {
	it.pos++
	return it.pos < len(it.entries)
}

func (it *sliceIterator) Entry() memtable.Entry { return it.entries[it.pos] }

func newSliceIterator(entries []memtable.Entry) *sliceIterator {
	return &sliceIterator{entries: entries, pos: -1}
}

func sortEntries(entries []memtable.Entry) {
	sort.Slice(entries, func(i, j int) bool {
		return memtable.Compare(entries[i].Key, entries[i].Sequence, entries[j].Key, entries[j].Sequence) < 0
	})
}

// TestWriteTableRoundTripsSmall checks the basics: write, open, get, iterate.
func TestWriteTableRoundTripsSmall(t *testing.T) {
	dir := t.TempDir()
	entries := []memtable.Entry{
		{Key: []byte("a"), Sequence: 1, Value: []byte("1")},
		{Key: []byte("b"), Sequence: 2, Value: []byte("2")},
		{Key: []byte("c"), Sequence: 3, Tombstone: true},
	}

	info, err := WriteTable(dir, 1, newSliceIterator(entries))
	if err != nil {
		t.Fatalf("WriteTable: %v", err)
	}
	if info.EntryCount != 3 {
		t.Errorf("EntryCount = %d, want 3", info.EntryCount)
	}
	if string(info.SmallestKey) != "a" || string(info.LargestKey) != "c" {
		t.Errorf("Smallest/Largest = %q/%q, want a/c", info.SmallestKey, info.LargestKey)
	}

	tbl, err := Open(info.Path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer tbl.Close()

	e, found, err := tbl.Get([]byte("b"))
	if err != nil || !found {
		t.Fatalf("Get(b) = %+v, %v, %v", e, found, err)
	}
	if string(e.Value) != "2" {
		t.Errorf("Get(b).Value = %q, want 2", e.Value)
	}

	e, found, err = tbl.Get([]byte("c"))
	if err != nil || !found || !e.Tombstone {
		t.Fatalf("Get(c) = %+v, %v, %v, want a found tombstone", e, found, err)
	}

	if _, found, err := tbl.Get([]byte("missing")); err != nil || found {
		t.Fatalf("Get(missing) = found=%v err=%v, want not found", found, err)
	}

	it := tbl.NewIterator()
	var got []memtable.Entry
	for it.Next() {
		got = append(got, it.Entry())
	}
	if err := it.Err(); err != nil {
		t.Fatalf("iterator error: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("iterated %d entries, want 3", len(got))
	}
}

// TestHundredThousandRandomPairsRoundTrip is T3.4's first done-when
// condition: build from 100k random pairs, reopen, and verify every key plus
// full iteration order.
func TestHundredThousandRandomPairsRoundTrip(t *testing.T) {
	const n = 100_000
	rng := rand.New(rand.NewSource(7))

	seen := map[string]bool{}
	var entries []memtable.Entry
	for len(entries) < n {
		key := fmt.Sprintf("key-%d", rng.Intn(n*2))
		if seen[key] {
			continue
		}
		seen[key] = true
		entries = append(entries, memtable.Entry{
			Key:      []byte(key),
			Sequence: uint64(len(entries) + 1),
			Value:    []byte(fmt.Sprintf("value-%d", rng.Intn(1_000_000))),
		})
	}
	sortEntries(entries)

	dir := t.TempDir()
	info, err := WriteTable(dir, 1, newSliceIterator(entries))
	if err != nil {
		t.Fatalf("WriteTable: %v", err)
	}
	if info.EntryCount != n {
		t.Fatalf("EntryCount = %d, want %d", info.EntryCount, n)
	}

	tbl, err := Open(info.Path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer tbl.Close()

	// Every key, via Get.
	for _, e := range entries {
		got, found, err := tbl.Get(e.Key)
		if err != nil {
			t.Fatalf("Get(%q): %v", e.Key, err)
		}
		if !found {
			t.Fatalf("Get(%q): not found", e.Key)
		}
		if string(got.Value) != string(e.Value) || got.Sequence != e.Sequence {
			t.Fatalf("Get(%q) = %+v, want %+v", e.Key, got, e)
		}
	}

	// Full iteration order.
	it := tbl.NewIterator()
	i := 0
	for it.Next() {
		got := it.Entry()
		if string(got.Key) != string(entries[i].Key) || got.Sequence != entries[i].Sequence {
			t.Fatalf("entry %d = %+v, want %+v", i, got, entries[i])
		}
		i++
	}
	if err := it.Err(); err != nil {
		t.Fatalf("iterator error: %v", err)
	}
	if i != n {
		t.Fatalf("iterated %d entries, want %d", i, n)
	}
}

// TestCorruptedByteIsCaughtByChecksum is T3.4's second done-when condition:
// a single corrupted byte anywhere in the file must be caught by a checksum
// rather than silently returning wrong data. It sweeps every data-block byte
// plus the index and footer, which is where a checksum omission would hide.
func TestCorruptedByteIsCaughtByChecksum(t *testing.T) {
	var entries []memtable.Entry
	for i := 0; i < 200; i++ {
		entries = append(entries, memtable.Entry{
			Key:      []byte(fmt.Sprintf("k%04d", i)),
			Sequence: 1,
			Value:    []byte(fmt.Sprintf("value-%d", i)),
		})
	}

	dir := t.TempDir()
	info, err := WriteTable(dir, 1, newSliceIterator(entries))
	if err != nil {
		t.Fatalf("WriteTable: %v", err)
	}

	original, err := os.ReadFile(info.Path)
	if err != nil {
		t.Fatalf("read original: %v", err)
	}

	// The magic bytes are deliberately excluded: flipping them changes the
	// error (rejected as "not a Strata SSTable") rather than as corruption,
	// which is exercised separately in TestOpenRejectsBadMagic. The footer's
	// reserved field (docs/format.md §3.5, offset 36) is also excluded: per
	// docs/format.md line 17, reserved fields are "written as zero, ignored
	// on read" and readers must not reject a non-zero reserved field, so
	// corruption there is defined to be silently tolerated. Every other byte
	// must be caught by a checksum, whichever section it lands in.
	footerStart := len(original) - footerSize
	magicStart := len(original) - 8
	reservedStart, reservedEnd := footerStart+36, footerStart+40

	for i := 0; i < len(original); i++ {
		if i >= magicStart || (i >= reservedStart && i < reservedEnd) {
			continue
		}
		corrupted := append([]byte(nil), original...)
		corrupted[i] ^= 0xFF

		path := filepath.Join(dir, fmt.Sprintf("corrupt-%d.sst", i))
		if err := os.WriteFile(path, corrupted, 0o600); err != nil {
			t.Fatalf("byte %d: write corrupted copy: %v", i, err)
		}

		tbl, err := Open(path)
		if err != nil {
			// Rejected at open, e.g. a damaged footer or index checksum: the
			// durability invariant held before a single block was read.
			continue
		}

		// The damage is in a data block: Open succeeds (index and footer were
		// untouched), but reading every entry back must surface the checksum
		// failure rather than return wrong bytes.
		it := tbl.NewIterator()
		for it.Next() {
		}
		if it.Err() == nil {
			t.Errorf("byte %d: table opened and iterated with no error, want a checksum failure somewhere", i)
		} else if !errors.Is(it.Err(), ErrCorruptBlock) {
			t.Errorf("byte %d: error = %v, want it to wrap ErrCorruptBlock", i, it.Err())
		}
		_ = tbl.Close()
	}
}

// TestOpenRejectsBadMagic checks a file that is not a Strata SSTable is
// rejected before any other field is read.
func TestOpenRejectsBadMagic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "notasstable.sst")
	if err := os.WriteFile(path, make([]byte, footerSize), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, err := Open(path)
	if err == nil {
		t.Fatal("Open succeeded on a file with no magic, want an error")
	}
	if !errors.Is(err, ErrCorruptTable) {
		t.Errorf("error = %v, want it to wrap ErrCorruptTable", err)
	}
}

// TestOpenRejectsShortFile checks a file too short to hold a footer is
// rejected rather than panicking on a slice bound.
func TestOpenRejectsShortFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "short.sst")
	if err := os.WriteFile(path, []byte("too short"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	if _, err := Open(path); !errors.Is(err, ErrCorruptTable) {
		t.Errorf("error = %v, want it to wrap ErrCorruptTable", err)
	}
}

// TestOpenRejectsUnsupportedVersion checks a recognised table with an
// unknown format_version is rejected with a version-mismatch error rather
// than a generic parse error.
func TestOpenRejectsUnsupportedVersion(t *testing.T) {
	dir := t.TempDir()
	info, err := WriteTable(dir, 1, newSliceIterator([]memtable.Entry{
		{Key: []byte("k"), Sequence: 1, Value: []byte("v")},
	}))
	if err != nil {
		t.Fatalf("WriteTable: %v", err)
	}

	data, err := os.ReadFile(info.Path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	// The version field lives at footer offset 32 (docs/format.md §3.5), not
	// absolute offset 32 -- it must be located relative to the footer's
	// actual position, which varies with the size of the sections before it.
	data[len(data)-footerSize+32] = 99 // format_version field, little-endian low byte
	if err := os.WriteFile(info.Path, data, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	_, err = Open(info.Path)
	if !errors.Is(err, ErrUnsupportedVersion) {
		t.Errorf("error = %v, want it to wrap ErrUnsupportedVersion", err)
	}
}

// TestSeekAcrossBlockBoundary checks a seek whose target falls in the gap
// between the last key of one block and the first key of the next still
// lands on the correct successor.
func TestSeekAcrossBlockBoundary(t *testing.T) {
	// Large values force many small blocks, guaranteeing multiple blocks for
	// a modest entry count.
	var entries []memtable.Entry
	for i := 0; i < 50; i++ {
		entries = append(entries, memtable.Entry{
			Key:      []byte(fmt.Sprintf("k%04d", i*2)), // even keys only
			Sequence: 1,
			Value:    make([]byte, 1024),
		})
	}

	dir := t.TempDir()
	info, err := WriteTable(dir, 1, newSliceIterator(entries))
	if err != nil {
		t.Fatalf("WriteTable: %v", err)
	}

	tbl, err := Open(info.Path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer tbl.Close()

	if len(tbl.index) < 2 {
		t.Fatalf("need at least two blocks for this test to mean anything, got %d", len(tbl.index))
	}

	it, err := tbl.Seek([]byte("k0055")) // between k0054 and k0056
	if err != nil {
		t.Fatalf("Seek: %v", err)
	}
	if !it.Next() {
		t.Fatal("Seek: no successor, want k0056")
	}
	if got := string(it.Entry().Key); got != "k0056" {
		t.Errorf("Seek(k0055) landed on %q, want k0056", got)
	}

	// A target past every key finds nothing.
	it, err = tbl.Seek([]byte("zzzz"))
	if err != nil {
		t.Fatalf("Seek(zzzz): %v", err)
	}
	if it.Next() {
		t.Errorf("Seek(zzzz) found %q, want no successor", it.Entry().Key)
	}
}

// TestWriteTableCreatesDurableFile checks the file and its directory entry
// are visible after WriteTable returns -- the observable half of the
// directory-fsync requirement (fsync itself is not observable from a test).
func TestWriteTableCreatesDurableFile(t *testing.T) {
	dir := t.TempDir()
	info, err := WriteTable(dir, 1, newSliceIterator([]memtable.Entry{
		{Key: []byte("k"), Sequence: 1, Value: []byte("v")},
	}))
	if err != nil {
		t.Fatalf("WriteTable: %v", err)
	}

	if _, err := os.Stat(info.Path); err != nil {
		t.Errorf("stat %s: %v", info.Path, err)
	}
	if filepath.Base(info.Path) != "000001.sst" {
		t.Errorf("path = %s, want basename 000001.sst", info.Path)
	}
}

// TestEmptyTableRoundTrips checks a table with zero entries is well-formed:
// no blocks, an empty index, Get always reports absent, iteration is empty.
func TestEmptyTableRoundTrips(t *testing.T) {
	dir := t.TempDir()
	info, err := WriteTable(dir, 1, newSliceIterator(nil))
	if err != nil {
		t.Fatalf("WriteTable: %v", err)
	}
	if info.EntryCount != 0 {
		t.Errorf("EntryCount = %d, want 0", info.EntryCount)
	}

	tbl, err := Open(info.Path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer tbl.Close()

	if _, found, err := tbl.Get([]byte("anything")); err != nil || found {
		t.Errorf("Get on empty table = found=%v err=%v, want not found", found, err)
	}
	it := tbl.NewIterator()
	if it.Next() {
		t.Error("empty table iterated an entry")
	}
}
