package memtable

import (
	"math"
	"sort"
	"testing"
)

// TestCompareOrdersByKeyThenSequenceDescending pins the one comparator
// docs/format.md §3.1 fixes for the memtable, the block builder, the merge
// iterator and compaction. §3.1 calls a second, subtly different comparator
// anywhere a bug, so this is the test that keeps them all honest.
func TestCompareOrdersByKeyThenSequenceDescending(t *testing.T) {
	tests := []struct {
		name string
		aKey string
		aSeq uint64
		bKey string
		bSeq uint64
		want int
	}{
		{"lesser key wins regardless of sequence", "a", 1, "b", 999, -1},
		{"greater key loses regardless of sequence", "b", 999, "a", 1, 1},
		{"same key, newer sequence sorts first", "k", 9, "k", 8, -1},
		{"same key, older sequence sorts last", "k", 8, "k", 9, 1},
		{"identical key and sequence", "k", 5, "k", 5, 0},

		// Byte-string ordering, shortest-first on a shared prefix.
		{"prefix sorts before its extension", "ab", 1, "abc", 1, -1},
		{"extension sorts after its prefix", "abc", 1, "ab", 1, 1},
		{"empty key is the smallest", "", 1, "a", 1, -1},
		{"empty key equals itself", "", 3, "", 3, 0},

		// Unsigned byte comparison: 0x80 is above 0x7f, not below it, which is
		// where a signed-byte comparator would diverge.
		{"high bit compares unsigned", "\x80", 1, "\x7f", 1, 1},
		{"nul byte is ordinary", "a\x00", 1, "a", 1, 1},

		{"sequence extremes do not overflow", "k", math.MaxUint64, "k", 0, -1},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Compare([]byte(tc.aKey), tc.aSeq, []byte(tc.bKey), tc.bSeq)
			if sign(got) != tc.want {
				t.Errorf("Compare(%q,%d, %q,%d) = %d, want sign %d",
					tc.aKey, tc.aSeq, tc.bKey, tc.bSeq, got, tc.want)
			}

			// Antisymmetry: swapping the arguments must negate the result, or
			// a sort built on this comparator is undefined.
			rev := Compare([]byte(tc.bKey), tc.bSeq, []byte(tc.aKey), tc.aSeq)
			if sign(rev) != -tc.want {
				t.Errorf("reversed Compare = %d, want sign %d", rev, -tc.want)
			}
		})
	}
}

// TestCompareIsATotalOrder checks transitivity across a shuffled set, which is
// the property sort.Slice relies on and the one a hand-written comparator most
// often breaks at the key/sequence boundary.
func TestCompareIsATotalOrder(t *testing.T) {
	type ver struct {
		key string
		seq uint64
	}
	vers := []ver{
		{"", 1}, {"", 2},
		{"a", 1}, {"a", 2}, {"a", 3},
		{"ab", 1}, {"b", 7}, {"b", 6},
		{"\x7f", 1}, {"\x80", 1},
	}

	sort.Slice(vers, func(i, j int) bool {
		return Compare([]byte(vers[i].key), vers[i].seq, []byte(vers[j].key), vers[j].seq) < 0
	})

	for i := 1; i < len(vers); i++ {
		if Compare([]byte(vers[i-1].key), vers[i-1].seq, []byte(vers[i].key), vers[i].seq) > 0 {
			t.Fatalf("sorted order violates the comparator at %d: %+v then %+v", i, vers[i-1], vers[i])
		}
	}

	// The newest version of a key must be the first one encountered, which is
	// the property the read path depends on to stop at the first match.
	first := map[string]uint64{}
	for _, v := range vers {
		if _, seen := first[v.key]; !seen {
			first[v.key] = v.seq
		}
	}
	if first["a"] != 3 {
		t.Errorf("first version of %q has sequence %d, want the newest (3)", "a", first["a"])
	}
	if first["b"] != 7 {
		t.Errorf("first version of %q has sequence %d, want the newest (7)", "b", first["b"])
	}
}

func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	default:
		return 0
	}
}
