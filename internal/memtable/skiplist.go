package memtable

import (
	"bytes"
	"math/rand"
	"sync/atomic"
)

const (
	// maxLevel bounds the skip list's height. p=0.25 makes a level's expected
	// population 1/4 of the one below it, so 12 levels comfortably covers
	// millions of entries before a search degrades toward linear.
	maxLevel = 12
	p        = 0.25

	// entryOverhead approximates the node's own bookkeeping cost -- the
	// struct header and its forward-pointer slice -- on top of the key and
	// value bytes it stores. Reporting only payload bytes rotates the
	// memtable late and overshoots the memory budget ApproxSize exists to
	// enforce.
	entryOverhead = 48
)

// node is one version in the skip list. next is sized to the node's
// randomly-chosen level and never resized after construction, so a reader
// walking it never observes a length change.
type node struct {
	key       []byte
	seq       uint64
	value     []byte
	tombstone bool
	next      []atomic.Pointer[node]
}

func (n *node) entry() Entry {
	return Entry{Key: n.key, Sequence: n.seq, Value: n.value, Tombstone: n.tombstone}
}

// SkipList is the concurrent Memtable (T3.1): lock-free reads via atomic
// forward-pointer loads, with Insert serialised to a single writer by the
// caller (internal/engine's rotation machinery guarantees this).
type SkipList struct {
	head *node
	size atomic.Int64
}

// NewSkipList returns an empty skip list ready for inserts.
func NewSkipList() *SkipList {
	return &SkipList{head: &node{next: make([]atomic.Pointer[node], maxLevel)}}
}

// randomLevel picks a node's height with p=0.25 per additional level, capped
// at maxLevel.
func randomLevel() int {
	lvl := 1
	for lvl < maxLevel && rand.Float64() < p {
		lvl++
	}
	return lvl
}

// Insert adds a version. Every level up to maxLevel is searched top-down to
// find the node immediately preceding the insertion point at that level.
func (s *SkipList) Insert(e Entry) error {
	var update [maxLevel]*node
	x := s.head
	for i := maxLevel - 1; i >= 0; i-- {
		for {
			next := x.next[i].Load()
			if next == nil || Compare(next.key, next.seq, e.Key, e.Sequence) >= 0 {
				break
			}
			x = next
		}
		update[i] = x
	}

	lvl := randomLevel()
	n := &node{
		key:       append([]byte(nil), e.Key...),
		seq:       e.Sequence,
		value:     append([]byte(nil), e.Value...),
		tombstone: e.Tombstone,
		next:      make([]atomic.Pointer[node], lvl),
	}

	// Level 0 must be linked first. A concurrent reader that has already
	// followed a higher-level pointer to n must find n's own level-0 pointer
	// already set -- otherwise it could walk off the end of the list into a
	// node that looks unfinished. Linking bottom-up guarantees that by the
	// time any level i >= 1 makes n reachable, n.next[0..i] are already in
	// place.
	for i := 0; i < lvl; i++ {
		n.next[i].Store(update[i].next[i].Load())
		update[i].next[i].Store(n)
	}

	s.size.Add(int64(len(e.Key)) + int64(len(e.Value)) + entryOverhead)
	return nil
}

// Get returns the newest version of key. A found tombstone reports
// (entry, true) with Tombstone set, distinct from the key being absent.
func (s *SkipList) Get(key []byte) (Entry, bool) {
	x := s.head
	for i := maxLevel - 1; i >= 0; i-- {
		for {
			next := x.next[i].Load()
			if next != nil && bytes.Compare(next.key, key) < 0 {
				x = next
				continue
			}
			break
		}
	}

	candidate := x.next[0].Load()
	if candidate == nil || !bytes.Equal(candidate.key, key) {
		return Entry{}, false
	}
	return candidate.entry(), true
}

// ApproxSize is the estimated heap cost of everything inserted so far.
func (s *SkipList) ApproxSize() int {
	return int(s.size.Load())
}

// NewIterator returns an iterator positioned before the first entry.
func (s *SkipList) NewIterator() Iterator {
	return &skipListIterator{cur: s.head}
}

type skipListIterator struct {
	cur *node
}

func (it *skipListIterator) Next() bool {
	next := it.cur.next[0].Load()
	if next == nil {
		return false
	}
	it.cur = next
	return true
}

func (it *skipListIterator) Entry() Entry {
	return it.cur.entry()
}
