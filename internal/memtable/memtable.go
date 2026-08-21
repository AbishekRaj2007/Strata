package memtable

import "bytes"

// Entry is one version of one key. A tombstone records that the key was
// deleted at Sequence; it is a version like any other, because a delete has to
// out-rank an older write of the same key at every level of the tree.
type Entry struct {
	Key       []byte
	Sequence  uint64
	Value     []byte
	Tombstone bool
}

// Compare orders two versions by the comparator docs/format.md §3.1 fixes for
// the whole system: user key ascending, sequence descending.
//
// Sequence descending is what makes the newest version of a key sort first, so
// an iterator meeting a key for the first time has found its current value and
// can skip the rest. The same comparator is used by the memtable, the block
// builder, the merge iterator and compaction; §3.1 calls a second, subtly
// different one anywhere a bug, so callers order through this function rather
// than reimplementing it.
func Compare(aKey []byte, aSeq uint64, bKey []byte, bSeq uint64) int {
	if c := bytes.Compare(aKey, bKey); c != 0 {
		return c
	}
	switch {
	case aSeq > bSeq:
		return -1
	case aSeq < bSeq:
		return 1
	default:
		return 0
	}
}

// Iterator walks entries in comparator order.
//
// The zero value is not usable; obtain one from a Memtable. An iterator
// observes the memtable as of its creation: memtables are only mutated while
// active, and a memtable is made immutable before anything iterates it for a
// flush.
type Iterator interface {
	// Next advances to the next entry and reports whether one exists.
	Next() bool

	// Entry returns the entry at the current position. It is only valid after
	// Next has returned true.
	Entry() Entry
}

// Memtable buffers recent writes in comparator order.
//
// The interface is the surface plan.md T3.1 specifies -- insert, point lookup,
// forward iteration, and an approximate size -- so that the concurrent skip
// list satisfies it without changing any call site here. Implementations must
// support concurrent readers alongside a single writer; the rotation machinery
// in internal/engine guarantees the single writer.
type Memtable interface {
	// Insert adds a version. Callers assign sequence numbers monotonically, so
	// an implementation may assume a key/sequence pair is never inserted twice.
	Insert(e Entry) error

	// Get returns the newest version of key, and whether any version exists.
	// A tombstone is a version: a found tombstone reports (entry, true) with
	// Tombstone set, which is how a delete shadows an older value in a lower
	// level rather than being mistaken for an absent key.
	Get(key []byte) (Entry, bool)

	// NewIterator returns an iterator positioned before the first entry.
	NewIterator() Iterator

	// ApproxSize is the estimated heap cost in bytes. It drives the rotation
	// threshold, so it must account for keys, values and per-entry overhead --
	// an implementation reporting only payload bytes rotates late and
	// overshoots the memory budget it exists to enforce.
	ApproxSize() int
}
