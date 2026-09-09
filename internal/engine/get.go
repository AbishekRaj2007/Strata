package engine

import (
	"fmt"
	"path/filepath"

	"github.com/AbishekRaj2007/Strata/internal/manifest"
	"github.com/AbishekRaj2007/Strata/internal/memtable"
	"github.com/AbishekRaj2007/Strata/internal/sstable"
)

// tableReader resolves a file number to a point lookup in that SSTable.
//
// It is an interface so that T5.1's bloom filter and T5.2's block cache slot
// in without touching the lookup order below, which is the part that is easy
// to get subtly wrong and must not be disturbed for a performance change.
type tableReader interface {
	lookup(number uint64, key []byte) (memtable.Entry, bool, error)
}

// dirTables reads tables straight from the data directory, opening and
// closing one per lookup.
//
// That is deliberately the naive implementation: it is obviously correct, and
// correctness is what Phase 4 is establishing. T5.2 replaces it with a cache,
// and the interface above is the seam where that happens.
type dirTables struct {
	dir string
}

func (d *dirTables) lookup(number uint64, key []byte) (memtable.Entry, bool, error) {
	path := filepath.Join(d.dir, fmt.Sprintf("%06d.sst", number))

	tbl, err := sstable.Open(path)
	if err != nil {
		return memtable.Entry{}, false, fmt.Errorf("get: open table %d: %w", number, err)
	}
	defer func() { _ = tbl.Close() }()

	e, found, err := tbl.Get(key)
	if err != nil {
		return memtable.Entry{}, false, fmt.Errorf("get: read table %d: %w", number, err)
	}
	return e, found, nil
}

// lookup walks the whole LSM for the newest version of key, in the one order
// that is correct:
//
//	active memtable
//	immutable memtables, newest first
//	L0 tables, newest first
//	L1 and below, one binary-searched candidate per level
//
// The first match wins and the search stops there. A tombstone is a match: it
// is returned with Tombstone set rather than as "absent", because the caller
// must stop rather than fall through to a stale value further down. Treating
// a tombstone as "keep looking" is how a deleted key comes back to life.
//
// The asymmetry between L0 and the levels below is the invariant that makes
// reads scale. L0 files come from independent memtable dumps, so their key
// ranges overlap arbitrarily and every one of them has to be checked, newest
// first. Below L0 compaction guarantees non-overlap, so at most one file per
// level can hold a given key and a binary search finds it -- turning the cost
// of a miss from "every file" into "one file per level".
//
// found reports whether any version exists at all.
func lookup(set *memtableSet, v *manifest.Version, tables tableReader, key []byte) (memtable.Entry, bool, error) {
	// The memtables, active then immutable newest-first.
	if set != nil {
		if e, found := set.Get(key); found {
			return e, true, nil
		}
	}

	if v == nil {
		return memtable.Entry{}, false, nil
	}

	// L0, newest file first. The ordering comes from the version, which sorts
	// L0 by file number descending -- never by a key-range heuristic. Newest
	// file wins, always; two L0 files can both contain the key and only the
	// newer one is the answer.
	for _, fm := range v.Files(0) {
		if !fm.Contains(key) {
			continue
		}
		e, found, err := tables.lookup(fm.Number, key)
		if err != nil {
			return memtable.Entry{}, false, err
		}
		if found {
			return e, true, nil
		}
	}

	// L1 and below: one candidate per level, found by binary search.
	for level := 1; level < manifest.NumLevels; level++ {
		fm, ok := v.Candidate(level, key)
		if !ok {
			continue
		}
		e, found, err := tables.lookup(fm.Number, key)
		if err != nil {
			return memtable.Entry{}, false, err
		}
		if found {
			return e, true, nil
		}
	}

	return memtable.Entry{}, false, nil
}
