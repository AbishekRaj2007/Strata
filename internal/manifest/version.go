package manifest

import (
	"bytes"
	"fmt"
	"sort"
)

// Version is an immutable snapshot of which SSTables are live at each level.
//
// Immutability is the whole design, and the reason a mutable list plus a mutex
// is not sufficient: a reader holding a slice of level 2 across a compaction
// would watch files disappear from underneath it, and the file it was about to
// open would already be deleted from disk. Instead compaction builds an
// entirely new Version and installs it with a single pointer swap. A reader
// holds the version it started with for the whole operation; that version's
// file list cannot change, and the files it names cannot be deleted while it
// is held (see refcount.go).
//
// Nothing in a Version is mutated after Apply returns it. Levels share
// *FileMetadata pointers with the version they were derived from, because
// those values are immutable too.
type Version struct {
	// levels[0] is L0, whose files may overlap and are ordered by file number
	// descending -- newest first, which is the order the read path must use.
	// levels[1:] hold files with strictly non-overlapping ranges, sorted by
	// smallest key ascending so a lookup can binary-search them.
	levels [NumLevels][]*FileMetadata

	// The counters carried forward from the edits that produced this version.
	logNumber      uint64
	nextFileNumber uint64
	lastSequence   uint64

	// refs counts the holders of this version, including the VersionSet
	// itself while the version is current. It is guarded by the owning
	// VersionSet's mutex rather than being atomic, because it must be read
	// and modified in the same critical section as the current-version
	// pointer -- see VersionSet.Acquire.
	refs int
}

// NewVersion returns the empty version a replay starts from.
func NewVersion() *Version { return &Version{} }

// Files returns the files at a level. The slice must not be modified: it is
// shared with every other holder of this version.
func (v *Version) Files(level int) []*FileMetadata {
	return v.levels[level]
}

// NumFiles reports how many files a level holds.
func (v *Version) NumFiles(level int) int { return len(v.levels[level]) }

// TotalFiles reports how many files the version holds across all levels.
func (v *Version) TotalFiles() int {
	n := 0
	for level := range v.levels {
		n += len(v.levels[level])
	}
	return n
}

// LevelBytes reports the total size of a level, which is what the compaction
// picker compares against the level's target.
func (v *Version) LevelBytes(level int) uint64 {
	var total uint64
	for _, f := range v.levels[level] {
		total += f.Size
	}
	return total
}

// LogNumber is the WAL file number this version's state begins at. Every WAL
// numbered below it is redundant and may be deleted.
func (v *Version) LogNumber() uint64 { return v.logNumber }

// NextFileNumber is the file-number allocator's next value.
func (v *Version) NextFileNumber() uint64 { return v.nextFileNumber }

// LastSequence is the highest sequence number assigned.
func (v *Version) LastSequence() uint64 { return v.lastSequence }

// Overlapping returns the files at a level whose key ranges intersect
// [smallest, largest].
//
// L0 is scanned linearly because its files may overlap arbitrarily -- they
// come from independent memtable dumps, so no ordering lets a search rule any
// of them out. Below L0 the non-overlap invariant makes the level sorted and
// binary-searchable, which is what makes reads scale with level count rather
// than with file count.
func (v *Version) Overlapping(level int, smallest, largest []byte) []*FileMetadata {
	files := v.levels[level]

	if level == 0 {
		var out []*FileMetadata
		for _, f := range files {
			if f.Overlaps(smallest, largest) {
				out = append(out, f)
			}
		}
		return out
	}

	// First file whose largest key is at or above smallest.
	i := sort.Search(len(files), func(i int) bool {
		return bytes.Compare(files[i].Largest, smallest) >= 0
	})

	var out []*FileMetadata
	for ; i < len(files) && bytes.Compare(files[i].Smallest, largest) <= 0; i++ {
		out = append(out, files[i])
	}
	return out
}

// Candidate returns the single file at a level below L0 that could contain
// key, and whether one exists.
//
// At most one can, because compaction guarantees non-overlap below L0. It is
// a programming error to call this for level 0, where every file is a
// candidate and the caller must walk them newest first.
func (v *Version) Candidate(level int, key []byte) (*FileMetadata, bool) {
	if level == 0 {
		panic("manifest: Candidate is not defined for L0, whose files overlap")
	}

	files := v.levels[level]
	i := sort.Search(len(files), func(i int) bool {
		return bytes.Compare(files[i].Largest, key) >= 0
	})
	if i < len(files) && files[i].Contains(key) {
		return files[i], true
	}
	return nil, false
}

// CheckInvariants verifies the structural properties every version must hold.
// It is what T6.5's continuous checker calls after each compaction, and it
// runs on every version built by Apply so a violation is caught where it was
// introduced rather than at the read that trips over it.
func (v *Version) CheckInvariants() error {
	seen := make(map[uint64]int, v.TotalFiles())

	for level := range v.levels {
		files := v.levels[level]

		for _, f := range files {
			if err := f.Validate(); err != nil {
				return err
			}
			// A file number is unique for the lifetime of the database
			// (docs/format.md §1), so the same file at two levels means the
			// version describes something impossible.
			if prev, dup := seen[f.Number]; dup {
				return fmt.Errorf("%w: file %d appears at both level %d and level %d",
					ErrCorrupt, f.Number, prev, level)
			}
			seen[f.Number] = level
		}

		if level == 0 {
			// L0 carries no ordering invariant on keys, only that the read
			// path sees it newest first.
			for i := 1; i < len(files); i++ {
				if files[i-1].Number <= files[i].Number {
					return fmt.Errorf("%w: L0 is not ordered by file number descending at index %d (%d then %d)",
						ErrCorrupt, i, files[i-1].Number, files[i].Number)
				}
			}
			continue
		}

		for i := 1; i < len(files); i++ {
			prev, cur := files[i-1], files[i]
			if bytes.Compare(prev.Smallest, cur.Smallest) > 0 {
				return fmt.Errorf("%w: level %d is not sorted by smallest key at index %d",
					ErrCorrupt, level, i)
			}
			// The invariant the whole read path below L0 depends on.
			if bytes.Compare(prev.Largest, cur.Smallest) >= 0 {
				return fmt.Errorf("%w: level %d files %d and %d overlap at key %q",
					ErrCorrupt, level, prev.Number, cur.Number, cur.Smallest)
			}
		}
	}

	return nil
}
