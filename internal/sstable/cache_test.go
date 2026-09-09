package sstable

import (
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/AbishekRaj2007/Strata/internal/cache"
)

// openCached writes a table and opens it against a fresh cache.
func openCached(t *testing.T, entries int) (*Table, *cache.Cache, string) {
	t.Helper()

	dir := t.TempDir()
	e := presentEntries(entries)
	sortEntries(e)
	info, err := WriteTableOpts(dir, 7, newSliceIterator(e), WriterOptions{})
	if err != nil {
		t.Fatalf("WriteTableOpts: %v", err)
	}

	c := cache.New(4 << 20)
	tbl, err := OpenWith(info.Path, OpenOptions{Number: 7, Cache: c})
	if err != nil {
		t.Fatalf("OpenWith: %v", err)
	}
	t.Cleanup(func() { _ = tbl.Close() })
	return tbl, c, info.Path
}

func TestRepeatedReadsAreServedFromTheCache(t *testing.T) {
	tbl, c, _ := openCached(t, 5_000)

	key := []byte(fmt.Sprintf("present:%06d", 0))
	for i := 0; i < 50; i++ {
		if _, found, err := tbl.Get(key); err != nil || !found {
			t.Fatalf("Get: found=%v err=%v", found, err)
		}
	}

	st := c.Stats()
	if st.Misses != 1 {
		t.Errorf("Misses = %d, want exactly 1: the first read populates and the rest hit", st.Misses)
	}
	if st.Hits != 49 {
		t.Errorf("Hits = %d, want 49", st.Hits)
	}
}

// TestChecksumIsVerifiedOnACacheHit is the invariant CLAUDE.md §11 states:
// checksums are consulted on every applicable read path, cache hits included.
//
// The temptation is to verify once on insert and treat resident blocks as
// trusted. That makes the fast path the one without the safety check, and
// process memory is not a medium that only fails at rest -- a bit flipped in
// a cached block would be returned as data, silently, for as long as the
// block stayed resident.
func TestChecksumIsVerifiedOnACacheHit(t *testing.T) {
	tbl, c, _ := openCached(t, 5_000)

	key := []byte(fmt.Sprintf("present:%06d", 0))
	if _, found, err := tbl.Get(key); err != nil || !found {
		t.Fatalf("warming Get: found=%v err=%v", found, err)
	}

	// Damage the resident copy without touching the file, which is exactly
	// what a memory fault looks like from the read path's point of view.
	blockKey := cache.Key{FileNumber: 7, BlockOffset: tbl.index[0].blockOffset}
	resident, ok := c.Get(blockKey)
	if !ok {
		t.Fatal("the block was not cached after a read")
	}
	damaged := append([]byte(nil), resident...)
	damaged[len(damaged)/2] ^= 0xFF
	c.Put(blockKey, damaged)

	_, _, err := tbl.Get(key)
	if !errors.Is(err, ErrCorruptBlock) {
		t.Fatalf("Get over a corrupted cached block = %v, want ErrCorruptBlock", err)
	}
}

// TestAFailedBlockIsNotCached keeps one bad read from becoming a permanent
// bad read. Caching bytes that failed verification would mean every
// subsequent lookup is served the same broken block from memory, never going
// back to the file that might since have been repaired or replaced.
func TestAFailedBlockIsNotCached(t *testing.T) {
	dir := t.TempDir()
	entries := presentEntries(2_000)
	sortEntries(entries)
	info, err := WriteTableOpts(dir, 7, newSliceIterator(entries), WriterOptions{})
	if err != nil {
		t.Fatalf("WriteTableOpts: %v", err)
	}

	// Corrupt a byte inside the first data block, which starts at offset 0.
	raw, err := os.ReadFile(info.Path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	raw[64] ^= 0xFF
	if err := os.WriteFile(info.Path, raw, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	c := cache.New(4 << 20)
	tbl, err := OpenWith(info.Path, OpenOptions{Number: 7, Cache: c})
	if err != nil {
		t.Fatalf("OpenWith: %v", err)
	}
	defer func() { _ = tbl.Close() }()

	key := []byte(fmt.Sprintf("present:%06d", 0))
	if _, _, err := tbl.Get(key); !errors.Is(err, ErrCorruptBlock) {
		t.Fatalf("Get = %v, want ErrCorruptBlock", err)
	}
	if st := c.Stats(); st.Entries != 0 {
		t.Fatalf("the cache holds %d blocks after a failed read, want 0", st.Entries)
	}
}

// TestTwoTablesDoNotShareCachedBlocks is the reason the cache key carries the
// file number. Both tables have a block at offset 0, and both blocks pass
// their own checksum, so a key without the file number would hand one table's
// data to the other with nothing detecting it.
func TestTwoTablesDoNotShareCachedBlocks(t *testing.T) {
	dir := t.TempDir()
	c := cache.New(4 << 20)

	makeTable := func(number uint64, prefix string) *Table {
		e := presentEntries(500)
		for i := range e {
			e[i].Key = []byte(fmt.Sprintf("%s:%06d", prefix, i))
			e[i].Value = []byte(prefix)
		}
		sortEntries(e)
		info, err := WriteTableOpts(dir, number, newSliceIterator(e), WriterOptions{})
		if err != nil {
			t.Fatalf("WriteTableOpts: %v", err)
		}
		tbl, err := OpenWith(info.Path, OpenOptions{Number: number, Cache: c})
		if err != nil {
			t.Fatalf("OpenWith: %v", err)
		}
		t.Cleanup(func() { _ = tbl.Close() })
		return tbl
	}

	a := makeTable(1, "aaa")
	b := makeTable(2, "bbb")

	got, found, err := a.Get([]byte("aaa:000000"))
	if err != nil || !found || string(got.Value) != "aaa" {
		t.Fatalf("table 1: value=%q found=%v err=%v", got.Value, found, err)
	}
	got, found, err = b.Get([]byte("bbb:000000"))
	if err != nil || !found || string(got.Value) != "bbb" {
		t.Fatalf("table 2: value=%q found=%v err=%v", got.Value, found, err)
	}

	// Each table's block is cached separately.
	if st := c.Stats(); st.Entries != 2 {
		t.Fatalf("cache holds %d blocks, want 2 -- one per table", st.Entries)
	}
}
