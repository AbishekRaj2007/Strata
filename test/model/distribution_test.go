package model

import (
	"math/rand"
	"strings"
	"testing"
)

// TestDistributionsAreDistinct guards the generators themselves. A
// distribution that silently degenerates to uniform would leave the whole
// property-testing suite testing one thing while claiming to test four, and
// nothing downstream would notice.
func TestDistributionsAreDistinct(t *testing.T) {
	const keySpace = 256

	counts := func(d Distribution) map[string]int {
		src := newKeySource(d, keySpace)
		rng := rand.New(rand.NewSource(1))
		got := make(map[string]int)
		for i := 0; i < 20_000; i++ {
			got[src.next(rng)]++
		}
		return got
	}

	t.Run("uniform spreads across the key space", func(t *testing.T) {
		c := counts(DistUniform)
		if len(c) != keySpace {
			t.Errorf("uniform touched %d of %d keys", len(c), keySpace)
		}
	})

	t.Run("zipfian concentrates on a few keys", func(t *testing.T) {
		c := counts(DistZipfian)
		// The head of a Zipfian is the whole point: if the ten hottest keys
		// do not carry a large share of the load, the skew is not there.
		top := topShare(c, 10)
		if top < 0.30 {
			t.Errorf("ten hottest keys carry %.2f of operations, want >= 0.30", top)
		}
	})

	t.Run("sequential walks in order", func(t *testing.T) {
		src := newKeySource(DistSequential, keySpace)
		rng := rand.New(rand.NewSource(1))
		prev := src.next(rng)
		for i := 1; i < keySpace; i++ {
			cur := src.next(rng)
			if cur <= prev {
				t.Fatalf("sequential went backwards at %d: %q then %q", i, prev, cur)
			}
			prev = cur
		}
		// And wraps, so a long run revisits rather than grows without bound.
		if got := src.next(rng); got >= prev {
			t.Errorf("sequential did not wrap after the key space: %q then %q", prev, got)
		}
	})

	t.Run("prefix keys share a long prefix", func(t *testing.T) {
		src := newKeySource(DistPrefix, keySpace)
		rng := rand.New(rand.NewSource(1))
		a, b := src.next(rng), src.next(rng)
		if a == b {
			b = src.next(rng)
		}
		if n := len(commonPrefixLen(a, b)); n < 40 {
			t.Errorf("%q and %q share only %d bytes, want a long shared prefix", a, b, n)
		}
		if !strings.HasPrefix(a, commonPrefix) {
			t.Errorf("%q does not carry the shared prefix", a)
		}
	})
}

// TestGenerateEmitsStructuralOperations checks that COMPACT is reachable,
// because a weight that is never drawn is a test that never runs.
func TestGenerateEmitsStructuralOperations(t *testing.T) {
	ops := Generate(rand.New(rand.NewSource(4)), GenConfig{
		Ops:          2_000,
		KeySpace:     64,
		Weights:      StructuralWeights,
		Distribution: DistZipfian,
	})

	seen := make(map[OpKind]int)
	for _, op := range ops {
		seen[op.Kind]++
	}
	for _, kind := range []OpKind{OpPut, OpGet, OpDelete, OpScan, OpFlush, OpCompact, OpReopen} {
		if seen[kind] == 0 {
			t.Errorf("StructuralWeights generated no %s in 2,000 operations", kind)
		}
	}
}

// TestGenerateHonoursValueSize keeps the large-value knob real.
func TestGenerateHonoursValueSize(t *testing.T) {
	ops := Generate(rand.New(rand.NewSource(5)), GenConfig{
		Ops:       200,
		KeySpace:  8,
		Weights:   Weights{Put: 1},
		ValueSize: 512,
	})
	for i, op := range ops {
		if len(op.Value) != 512 {
			t.Fatalf("op %d has a %d-byte value, want 512", i, len(op.Value))
		}
	}
}

// TestModelAcrossDistributions is the breadth half of T7.1: every
// distribution, run against the real engine with compaction in the mix.
func TestModelAcrossDistributions(t *testing.T) {
	ops := 4_000
	if testing.Short() {
		ops = 500
	}

	for _, d := range []Distribution{DistUniform, DistZipfian, DistSequential, DistPrefix} {
		t.Run(string(d), func(t *testing.T) {
			seq := Generate(rand.New(rand.NewSource(11)), GenConfig{
				Ops:          ops,
				KeySpace:     128,
				Weights:      StructuralWeights,
				Distribution: d,
			})
			if err := runSequence(t, seq, 4<<10); err != nil {
				t.Fatalf("%s diverged: %v", d, err)
			}
		})
	}
}

func topShare(counts map[string]int, n int) float64 {
	var total int
	ordered := make([]int, 0, len(counts))
	for _, c := range counts {
		ordered = append(ordered, c)
		total += c
	}
	// A partial selection sort is enough for the handful we need.
	var head int
	for i := 0; i < n && i < len(ordered); i++ {
		max := i
		for j := i + 1; j < len(ordered); j++ {
			if ordered[j] > ordered[max] {
				max = j
			}
		}
		ordered[i], ordered[max] = ordered[max], ordered[i]
		head += ordered[i]
	}
	return float64(head) / float64(total)
}

func commonPrefixLen(a, b string) string {
	i := 0
	for i < len(a) && i < len(b) && a[i] == b[i] {
		i++
	}
	return a[:i]
}
