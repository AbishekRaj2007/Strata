package engine

import (
	"fmt"
	"testing"

	"github.com/AbishekRaj2007/Strata/internal/manifest"
	"github.com/AbishekRaj2007/Strata/internal/memtable"
)

// fakeTables serves point lookups from memory and records the order files
// were consulted in, which is what the L0 trap is actually about.
type fakeTables struct {
	files   map[uint64]map[string]memtable.Entry
	visited []uint64
}

func newFakeTables() *fakeTables {
	return &fakeTables{files: map[uint64]map[string]memtable.Entry{}}
}

func (f *fakeTables) put(number uint64, e memtable.Entry) {
	if f.files[number] == nil {
		f.files[number] = map[string]memtable.Entry{}
	}
	f.files[number][string(e.Key)] = e
}

func (f *fakeTables) lookup(number uint64, key []byte) (memtable.Entry, bool, error) {
	f.visited = append(f.visited, number)
	e, ok := f.files[number][string(key)]
	return e, ok, nil
}

// versionWith builds a version holding the given files at the given levels.
func versionWith(t *testing.T, placements map[int][]*manifest.FileMetadata) *manifest.Version {
	t.Helper()
	var e manifest.VersionEdit
	for level, files := range placements {
		for _, fm := range files {
			e.AddFile(level, fm)
		}
	}
	v, err := manifest.NewVersion().Apply(&e)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	return v
}

func fileAt(number uint64, smallest, largest string) *manifest.FileMetadata {
	return &manifest.FileMetadata{
		Number:   number,
		Size:     1024,
		Smallest: []byte(smallest),
		Largest:  []byte(largest),
	}
}

// TestLookupPrefersTheNewestL0File is T4.3's trap. Two L0 files both contain
// the key -- which is legal, L0 files overlap arbitrarily -- and only the
// newer one holds the answer. The version orders L0 by file number
// descending, and the lookup must honour that rather than stopping at
// whichever file it happened to reach first.
func TestLookupPrefersTheNewestL0File(t *testing.T) {
	tables := newFakeTables()
	tables.put(1, memtable.Entry{Key: []byte("k"), Sequence: 1, Value: []byte("old")})
	tables.put(2, memtable.Entry{Key: []byte("k"), Sequence: 9, Value: []byte("new")})

	v := versionWith(t, map[int][]*manifest.FileMetadata{
		0: {fileAt(1, "a", "z"), fileAt(2, "a", "z")},
	})

	e, found, err := lookup(nil, v, tables, []byte("k"))
	if err != nil || !found {
		t.Fatalf("lookup = %+v, %v, %v", e, found, err)
	}
	if string(e.Value) != "new" {
		t.Errorf("value = %q, want \"new\" from the higher-numbered L0 file", e.Value)
	}
	if len(tables.visited) == 0 || tables.visited[0] != 2 {
		t.Errorf("visited %v, want file 2 (newest) consulted first", tables.visited)
	}
}

// TestLookupStopsAtATombstone checks a delete shadows lower levels rather
// than falling through to a stale value.
func TestLookupStopsAtATombstone(t *testing.T) {
	tables := newFakeTables()
	tables.put(2, memtable.Entry{Key: []byte("k"), Sequence: 9, Tombstone: true})
	tables.put(1, memtable.Entry{Key: []byte("k"), Sequence: 1, Value: []byte("stale")})

	v := versionWith(t, map[int][]*manifest.FileMetadata{
		0: {fileAt(2, "a", "z")},
		1: {fileAt(1, "a", "z")},
	})

	e, found, err := lookup(nil, v, tables, []byte("k"))
	if err != nil || !found {
		t.Fatalf("lookup = %+v, %v, %v", e, found, err)
	}
	if !e.Tombstone {
		t.Errorf("got %+v, want the tombstone rather than the shadowed value", e)
	}
	for _, n := range tables.visited {
		if n == 1 {
			t.Error("lookup read the L1 file after finding a tombstone in L0")
		}
	}
}

// TestLookupChecksOneCandidatePerLevelBelowL0 is the invariant that makes
// reads scale: below L0 the non-overlap guarantee means at most one file per
// level can hold the key, so a miss costs one file per level rather than
// every file in the level.
func TestLookupChecksOneCandidatePerLevelBelowL0(t *testing.T) {
	tables := newFakeTables()

	// Twenty non-overlapping files at L1; only one can contain "k05-mid".
	var l1 []*manifest.FileMetadata
	for i := 0; i < 20; i++ {
		l1 = append(l1, fileAt(uint64(100+i), fmt.Sprintf("k%02d-a", i), fmt.Sprintf("k%02d-z", i)))
	}
	v := versionWith(t, map[int][]*manifest.FileMetadata{1: l1})

	if _, _, err := lookup(nil, v, tables, []byte("k05-mid")); err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if len(tables.visited) != 1 {
		t.Errorf("consulted %d files at L1 (%v), want exactly 1", len(tables.visited), tables.visited)
	}
	if len(tables.visited) == 1 && tables.visited[0] != 105 {
		t.Errorf("consulted file %d, want 105 (the range holding the key)", tables.visited[0])
	}
}

// TestLookupSkipsLevelsWithNoCandidate checks a key outside every range at a
// level costs no file read at all.
func TestLookupSkipsLevelsWithNoCandidate(t *testing.T) {
	tables := newFakeTables()
	v := versionWith(t, map[int][]*manifest.FileMetadata{
		1: {fileAt(1, "a", "c")},
		2: {fileAt(2, "x", "z")},
	})

	_, found, err := lookup(nil, v, tables, []byte("m"))
	if err != nil {
		t.Fatalf("lookup: %v", err)
	}
	if found {
		t.Error("found a key that is in no file's range")
	}
	if len(tables.visited) != 0 {
		t.Errorf("read %v for a key outside every range, want no reads", tables.visited)
	}
}

// TestLookupPrefersMemtablesOverTables checks the memtables come first: a
// value written but not yet flushed must win over the flushed version.
func TestLookupPrefersMemtablesOverTables(t *testing.T) {
	env := newFlushEnv(t, 1<<20, 4)
	env.put(t, "k", "fresh")

	tables := newFakeTables()
	tables.put(1, memtable.Entry{Key: []byte("k"), Sequence: 1, Value: []byte("flushed")})
	v := versionWith(t, map[int][]*manifest.FileMetadata{0: {fileAt(1, "a", "z")}})

	e, found, err := lookup(env.set, v, tables, []byte("k"))
	if err != nil || !found {
		t.Fatalf("lookup = %+v, %v, %v", e, found, err)
	}
	if string(e.Value) != "fresh" {
		t.Errorf("value = %q, want \"fresh\" from the memtable", e.Value)
	}
	if len(tables.visited) != 0 {
		t.Errorf("read %v after the memtable answered", tables.visited)
	}
}

// TestLookupReadsRealTablesFromDisk exercises the real dirTables reader
// rather than the in-memory double, so the path that production uses is
// covered too.
func TestLookupReadsRealTablesFromDisk(t *testing.T) {
	env := newFlushEnv(t, 1, 4)

	env.put(t, "alpha", "1")
	env.put(t, "beta", "2")
	if err := env.f.DrainQueue(); err != nil {
		t.Fatalf("DrainQueue: %v", err)
	}

	v := env.vs.Current()
	if v.NumFiles(0) == 0 {
		t.Fatal("nothing was flushed")
	}

	got, found, err := lookup(nil, v, &dirTables{dir: env.dir}, []byte("alpha"))
	if err != nil || !found {
		t.Fatalf("lookup(alpha) = %+v, %v, %v", got, found, err)
	}
	if string(got.Value) != "1" {
		t.Errorf("alpha = %q, want \"1\"", got.Value)
	}

	if _, found, err := lookup(nil, v, &dirTables{dir: env.dir}, []byte("absent")); err != nil || found {
		t.Errorf("lookup(absent) = found=%v err=%v, want not found", found, err)
	}
}

// TestLookupPropagatesTableErrors checks a corrupt table surfaces rather than
// being read as an absent key, which would silently lose data.
func TestLookupPropagatesTableErrors(t *testing.T) {
	dir := t.TempDir()

	// A version naming a file that does not exist on disk.
	v := versionWith(t, map[int][]*manifest.FileMetadata{0: {fileAt(7, "a", "z")}})

	if _, _, err := lookup(nil, v, &dirTables{dir: dir}, []byte("k")); err == nil {
		t.Fatal("lookup reported a clean miss for a table it could not open")
	}
}
