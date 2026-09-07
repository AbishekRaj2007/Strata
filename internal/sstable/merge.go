package sstable

import (
	"bytes"
	"container/heap"

	"github.com/AbishekRaj2007/Strata/internal/memtable"
)

// MergeIterator merges any number of sorted sources into one sorted stream,
// keeping only the newest version of each user key.
//
// The comparator is the project's canonical one -- user key ascending,
// sequence descending -- and using it here is what makes deduplication a
// single comparison rather than a search. Because every version of a key
// arrives newest-first, the first entry seen for a key is the one that wins
// and every later entry for that key is a strictly older version to discard.
// Comparing only user keys, and leaving sequence order to chance, yields a
// merge that returns an arbitrary version; that is the trap this type exists
// to close.
//
// Cost is O(n log k) for n entries across k sources: each entry enters and
// leaves a k-element heap exactly once. The naive alternative -- rescanning
// every source for the minimum on each step -- is O(nk), which is the
// difference between compaction being cheap and compaction being the reason
// the database falls over.
//
// Compaction reuses this verbatim in Phase 6, which is why tombstone handling
// is a flag rather than a hardcoded policy: a read path wants tombstones
// hidden, and a compaction that is not merging into the bottom level must
// keep them, because dropping one there resurrects deleted data.
type MergeIterator struct {
	h              mergeHeap
	skipTombstones bool

	cur   memtable.Entry
	valid bool

	// lastKey is the last user key the merge has consumed, whether it was
	// emitted or skipped. Recording skipped keys matters: when a tombstone is
	// suppressed, the older versions it shadows must be suppressed too.
	lastKey []byte
	hasLast bool

	err error
}

// mergeCursor is one source and the entry it is currently positioned on.
type mergeCursor struct {
	it    memtable.Iterator
	entry memtable.Entry

	// index breaks ties between sources holding the same key at the same
	// sequence. That should not happen -- sequences are unique per write --
	// but the merge must still be deterministic if it does, rather than
	// depending on heap internals.
	index int
}

type mergeHeap []*mergeCursor

func (h mergeHeap) Len() int { return len(h) }

func (h mergeHeap) Less(i, j int) bool {
	if c := memtable.Compare(h[i].entry.Key, h[i].entry.Sequence, h[j].entry.Key, h[j].entry.Sequence); c != 0 {
		return c < 0
	}
	return h[i].index < h[j].index
}

func (h mergeHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }

func (h *mergeHeap) Push(x any) { *h = append(*h, x.(*mergeCursor)) }

func (h *mergeHeap) Pop() any {
	old := *h
	n := len(old)
	c := old[n-1]
	old[n-1] = nil
	*h = old[:n-1]
	return c
}

// NewMergeIterator merges sources into one stream. When skipTombstones is
// set, a key whose newest version is a delete is omitted entirely rather than
// surfaced as a tombstone.
//
// Sources must each be in comparator order; the merge preserves that order
// but cannot repair a source that violates it. Nil sources are ignored, which
// lets a caller assemble a source list with holes rather than compacting it
// first.
func NewMergeIterator(sources []memtable.Iterator, skipTombstones bool) *MergeIterator {
	m := &MergeIterator{skipTombstones: skipTombstones}

	for i, it := range sources {
		if it == nil {
			continue
		}
		if it.Next() {
			m.h = append(m.h, &mergeCursor{it: it, entry: it.Entry(), index: i})
			continue
		}
		m.captureErr(it)
	}
	heap.Init(&m.h)

	return m
}

// Next advances to the next distinct user key and reports whether one exists.
func (m *MergeIterator) Next() bool {
	for {
		if m.err != nil || len(m.h) == 0 {
			m.valid = false
			return false
		}

		top := m.h[0]
		e := top.entry
		m.advanceTop(top)

		// Every entry after the first for a key is an older version of it.
		if m.hasLast && bytes.Equal(e.Key, m.lastKey) {
			continue
		}
		m.lastKey = append(m.lastKey[:0], e.Key...)
		m.hasLast = true

		if m.skipTombstones && e.Tombstone {
			continue
		}

		m.cur = e
		m.valid = true
		return true
	}
}

// Entry returns the entry at the current position. It is only valid after
// Next has returned true.
func (m *MergeIterator) Entry() memtable.Entry { return m.cur }

// Err reports the first error a source raised. A merge that stops because a
// source was corrupt must not be mistaken for one that reached the end, so
// callers have to consult this after Next returns false.
func (m *MergeIterator) Err() error { return m.err }

// advanceTop moves the cursor at the root of the heap forward, restoring the
// heap in place when it still has entries and removing it when it does not.
func (m *MergeIterator) advanceTop(c *mergeCursor) {
	if c.it.Next() {
		c.entry = c.it.Entry()
		heap.Fix(&m.h, 0)
		return
	}

	heap.Pop(&m.h)
	m.captureErr(c.it)
}

// captureErr records a source's error if it reports one. Memtable iterators
// cannot fail, so the interface does not require Err; table iterators can,
// because they decode bytes that may be corrupt.
func (m *MergeIterator) captureErr(it memtable.Iterator) {
	e, ok := it.(interface{ Err() error })
	if !ok {
		return
	}
	if err := e.Err(); err != nil && m.err == nil {
		m.err = err
	}
}
