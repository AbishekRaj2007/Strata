package manifest

import (
	"bytes"
	"fmt"
)

// NumLevels is the number of levels in the tree: L0 for freshly flushed
// tables, and L1 through L6 for the non-overlapping levels below it. Seven is
// enough for a database far larger than this one at the 10x per-level
// multiplier ADR-002 fixes, and a fixed count keeps a Version's per-level
// slices a plain array rather than a growing structure that compaction would
// have to reason about.
const NumLevels = 7

// FileMetadata describes one SSTable. It is the manifest's whole view of a
// table: everything the read path needs to decide whether a key could be in a
// file, without opening the file.
//
// A FileMetadata is immutable once constructed. Versions share pointers to the
// same value rather than copying it, so mutating one would change the contents
// of versions that other readers are holding.
type FileMetadata struct {
	// Number is the file's number in the directory's single monotonic
	// sequence (docs/format.md §1), which names it as %06d.sst.
	Number uint64

	// Size is the file's size in bytes, used by the compaction picker to
	// compare a level's bytes against its target.
	Size uint64

	// Smallest and Largest are the user keys bounding the file, inclusive at
	// both ends. They are user keys, not internal keys: the sequence range is
	// carried separately, because a file's key bounds and its sequence bounds
	// answer different questions.
	Smallest []byte
	Largest  []byte

	// SmallestSeq and LargestSeq bound the sequence numbers in the file. They
	// are what lets a snapshot read skip a file entirely, and what tells
	// compaction whether a tombstone in this file could still be shadowing an
	// older value somewhere below.
	SmallestSeq uint64
	LargestSeq  uint64
}

// Name returns the file's name within the data directory.
func (f *FileMetadata) Name() string {
	return fmt.Sprintf("%06d.sst", f.Number)
}

// Contains reports whether key falls within the file's key range. A true
// result does not mean the key is present -- only that the file cannot be
// ruled out and must be searched.
func (f *FileMetadata) Contains(key []byte) bool {
	return bytes.Compare(key, f.Smallest) >= 0 && bytes.Compare(key, f.Largest) <= 0
}

// Overlaps reports whether the file's key range intersects [smallest, largest],
// inclusive at both ends.
//
// Compaction is built on this: picking a file at level i and finding every
// file at level i+1 that overlaps it is the whole input set. An off-by-one
// here silently drops a file from a compaction and leaves two files with
// overlapping ranges at a level that guarantees none.
func (f *FileMetadata) Overlaps(smallest, largest []byte) bool {
	return bytes.Compare(f.Largest, smallest) >= 0 && bytes.Compare(f.Smallest, largest) <= 0
}

// Validate reports whether the metadata is internally consistent. It is
// checked on decode, because a manifest that says a file's smallest key is
// above its largest describes a file that cannot exist, and carrying that
// forward produces a version whose invariants cannot hold.
func (f *FileMetadata) Validate() error {
	if bytes.Compare(f.Smallest, f.Largest) > 0 {
		return fmt.Errorf("%w: file %d has smallest key %q above largest %q",
			ErrCorrupt, f.Number, f.Smallest, f.Largest)
	}
	if f.SmallestSeq > f.LargestSeq {
		return fmt.Errorf("%w: file %d has smallest sequence %d above largest %d",
			ErrCorrupt, f.Number, f.SmallestSeq, f.LargestSeq)
	}
	return nil
}
