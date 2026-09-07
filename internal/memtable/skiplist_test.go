package memtable

import (
	"fmt"
	"math/rand"
	"sort"
	"sync"
	"testing"
)

// TestSkipListInsertGetRoundTrips checks the smallest useful case: what goes
// in comes back out, and an absent key reports absence rather than a zero
// value that could be mistaken for one.
func TestSkipListInsertGetRoundTrips(t *testing.T) {
	s := NewSkipList()

	if err := s.Insert(Entry{Key: []byte("k"), Sequence: 1, Value: []byte("v")}); err != nil {
		t.Fatalf("Insert: %v", err)
	}

	got, ok := s.Get([]byte("k"))
	if !ok {
		t.Fatal("Get(k) = not found, want found")
	}
	if string(got.Value) != "v" || got.Sequence != 1 {
		t.Errorf("Get(k) = %+v, want value v, sequence 1", got)
	}

	if _, ok := s.Get([]byte("missing")); ok {
		t.Error("Get(missing) = found, want not found")
	}
}

// TestSkipListGetReturnsNewestVersion covers why the comparator orders
// sequence descending: a point lookup must land on the newest write without
// scanning every version of the key.
func TestSkipListGetReturnsNewestVersion(t *testing.T) {
	s := NewSkipList()
	for _, seq := range []uint64{1, 5, 3, 9, 2} {
		if err := s.Insert(Entry{Key: []byte("k"), Sequence: seq, Value: []byte(fmt.Sprint(seq))}); err != nil {
			t.Fatalf("Insert seq %d: %v", seq, err)
		}
	}

	got, ok := s.Get([]byte("k"))
	if !ok {
		t.Fatal("Get(k) = not found")
	}
	if got.Sequence != 9 {
		t.Errorf("Get(k).Sequence = %d, want 9 (the newest)", got.Sequence)
	}
}

// TestSkipListGetFindsTombstone covers why Get returns (Entry, bool) rather
// than (Entry, bool) collapsing a tombstone into "not found": a delete must
// shadow an older value in a lower level, which requires the caller to see
// that a tombstone exists at all.
func TestSkipListGetFindsTombstone(t *testing.T) {
	s := NewSkipList()
	if err := s.Insert(Entry{Key: []byte("k"), Sequence: 1, Value: []byte("v")}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := s.Insert(Entry{Key: []byte("k"), Sequence: 2, Tombstone: true}); err != nil {
		t.Fatalf("insert tombstone: %v", err)
	}

	got, ok := s.Get([]byte("k"))
	if !ok {
		t.Fatal("Get(k) = not found, want the tombstone")
	}
	if !got.Tombstone || got.Sequence != 2 {
		t.Errorf("Get(k) = %+v, want tombstone at sequence 2", got)
	}
}

// TestSkipListIteratorOrdersByComparator is T3.1's core correctness property:
// inserting 100k random keys, each with a random number of versions, and
// reading them back through the iterator must reproduce the exact order the
// package comparator defines.
func TestSkipListIteratorOrdersByComparator(t *testing.T) {
	const n = 100_000
	rng := rand.New(rand.NewSource(1))

	s := NewSkipList()
	type ver struct {
		key string
		seq uint64
	}
	seen := map[ver]bool{}
	var inserted []ver

	for len(inserted) < n {
		key := fmt.Sprintf("key-%d", rng.Intn(n/4))
		seq := uint64(rng.Intn(1_000_000) + 1)
		v := ver{key, seq}
		if seen[v] {
			continue // Insert may assume a key/sequence pair is never repeated.
		}
		seen[v] = true
		inserted = append(inserted, v)
		if err := s.Insert(Entry{Key: []byte(key), Sequence: seq, Value: []byte("x")}); err != nil {
			t.Fatalf("insert %v: %v", v, err)
		}
	}

	sort.Slice(inserted, func(i, j int) bool {
		return Compare([]byte(inserted[i].key), inserted[i].seq, []byte(inserted[j].key), inserted[j].seq) < 0
	})

	it := s.NewIterator()
	for i, want := range inserted {
		if !it.Next() {
			t.Fatalf("iterator exhausted after %d entries, want %d", i, n)
		}
		got := it.Entry()
		if string(got.Key) != want.key || got.Sequence != want.seq {
			t.Fatalf("entry %d = (%q,%d), want (%q,%d)", i, got.Key, got.Sequence, want.key, want.seq)
		}
	}
	if it.Next() {
		t.Fatal("iterator produced more entries than were inserted")
	}
}

// TestSkipListConcurrentReadersDuringInsert is T3.1's other done-when
// condition: one writer inserting while eight readers hammer Get and iterate
// concurrently must be clean under -race. This is what exercises the ordering
// constraint in the trap -- a reader must never observe a node reachable at a
// high level but not yet linked at level 0.
func TestSkipListConcurrentReadersDuringInsert(t *testing.T) {
	s := NewSkipList()
	const inserts = 20_000
	const readers = 8

	var wg sync.WaitGroup
	stop := make(chan struct{})

	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				it := s.NewIterator()
				count := 0
				for it.Next() {
					e := it.Entry()
					if len(e.Key) == 0 {
						continue
					}
					count++
				}
				s.Get([]byte("key-0"))
			}
		}()
	}

	for i := 0; i < inserts; i++ {
		key := fmt.Sprintf("key-%d", i)
		if err := s.Insert(Entry{Key: []byte(key), Sequence: uint64(i + 1), Value: []byte("v")}); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	close(stop)
	wg.Wait()

	if got := countEntries(t, s); got != inserts {
		t.Errorf("final count = %d, want %d", got, inserts)
	}
}

func countEntries(t *testing.T, s *SkipList) int {
	t.Helper()
	it := s.NewIterator()
	n := 0
	for it.Next() {
		n++
	}
	return n
}

// TestSkipListApproxSizeGrowsWithInserts checks the rotation threshold has
// something real to compare against: a size report of only payload bytes
// rotates late and overshoots the memory budget it exists to enforce.
func TestSkipListApproxSizeGrowsWithInserts(t *testing.T) {
	s := NewSkipList()
	if s.ApproxSize() != 0 {
		t.Fatalf("ApproxSize on empty list = %d, want 0", s.ApproxSize())
	}

	if err := s.Insert(Entry{Key: []byte("k"), Sequence: 1, Value: make([]byte, 1000)}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if got := s.ApproxSize(); got <= 1000 {
		t.Errorf("ApproxSize = %d, want more than the 1000 payload bytes alone", got)
	}
}
