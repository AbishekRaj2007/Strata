package sstable

import (
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/AbishekRaj2007/Strata/internal/memtable"
)

// buildMergeSources splits a fixed total number of entries evenly across k
// sources. Holding the total constant is the whole point: if the merge is
// O(n log k) then growing k with n fixed costs only the log factor, while an
// O(nk) merge grows linearly in k and separates from it immediately.
func buildMergeSources(total, k int) []memtable.Iterator {
	perSource := total / k

	sources := make([]memtable.Iterator, 0, k)
	for s := 0; s < k; s++ {
		entries := make([]memtable.Entry, 0, perSource)
		for i := 0; i < perSource; i++ {
			// Interleaving keys across sources so that every source stays live
			// for the whole merge; giving each a disjoint contiguous range
			// would drain them one at a time and flatter the heap.
			key := i*k + s
			entries = append(entries, memtable.Entry{
				Key:      []byte(fmt.Sprintf("key-%08d", key)),
				Sequence: uint64(key + 1),
				Value:    []byte("value"),
			})
		}
		sources = append(sources, newSliceIterator(entries))
	}
	return sources
}

// BenchmarkMergeScaling is T4.4's complexity claim. Total entries are fixed at
// 65536 and only the source count varies, so the per-entry cost isolates the
// heap: O(n log k) predicts cost proportional to log2(k), meaning a doubling
// of k adds a constant, while O(nk) predicts a doubling.
//
// Read the ns/op column across k and compare ratios, not absolutes:
//
//	go test ./internal/sstable/ -run '^$' -bench BenchmarkMergeScaling -benchmem
func BenchmarkMergeScaling(b *testing.B) {
	const total = 65536

	for _, k := range []int{2, 4, 8, 16, 32, 64, 128} {
		b.Run(fmt.Sprintf("k=%d", k), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				sources := buildMergeSources(total, k)
				b.StartTimer()

				m := NewMergeIterator(sources, false)
				n := 0
				for m.Next() {
					n++
				}
				if err := m.Err(); err != nil {
					b.Fatalf("merge error: %v", err)
				}
				if n == 0 {
					b.Fatal("merged nothing")
				}
			}
		})
	}
}

// TestMergeCostGrowsLogarithmicallyInSources turns the benchmark's claim into
// an assertion, so a regression to a linear scan fails the suite rather than
// waiting to be noticed in a benchmark run nobody reads.
//
// With the entry count fixed, going from k=2 to k=128 is a 64x increase in
// sources. O(nk) would cost roughly 64x more work; O(n log k) costs the ratio
// of the logs, log2(128)/log2(2) = 7. The assertion is deliberately loose --
// it only has to separate 7 from 64 -- because wall-clock timing in a test is
// noisy and a tight bound would flake.
func TestMergeCostGrowsLogarithmicallyInSources(t *testing.T) {
	if testing.Short() {
		t.Skip("timing comparison is slow under -short")
	}

	const total = 65536

	measure := func(k int) float64 {
		// A few repetitions, keeping the best, to blunt scheduler noise.
		best := math.Inf(1)
		for rep := 0; rep < 5; rep++ {
			sources := buildMergeSources(total, k)
			m := NewMergeIterator(sources, false)

			start := time.Now()
			n := 0
			for m.Next() {
				n++
			}
			elapsed := float64(time.Since(start).Nanoseconds())

			if err := m.Err(); err != nil {
				t.Fatalf("merge error: %v", err)
			}
			if n == 0 {
				t.Fatal("merged nothing")
			}
			if elapsed < best {
				best = elapsed
			}
		}
		return best
	}

	low, high := measure(2), measure(128)
	ratio := high / low

	// log2(128)/log2(2) = 7 is the O(n log k) prediction; 64 is the O(nk)
	// one. Anything under 20 is comfortably the former.
	const linearWouldBe = 64.0
	if ratio > linearWouldBe/3 {
		t.Errorf("merging 128 sources cost %.1fx merging 2 (same total entries); "+
			"O(n log k) predicts ~7x, O(nk) would predict ~%.0fx", ratio, linearWouldBe)
	}
	t.Logf("k=2: %.0f ns, k=128: %.0f ns, ratio %.1fx (O(n log k) predicts ~7x)", low, high, ratio)
}
