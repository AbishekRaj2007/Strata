package compaction

import (
	"fmt"
	"math"
	"math/rand"
	"sort"
	"strings"
	"testing"

	"github.com/AbishekRaj2007/Strata/internal/manifest"
)

// This file is T6.1's done-when condition: under a skewed write workload,
// compaction work has to spread across the keyspace instead of concentrating.
//
// It runs the picker against a model of the tree rather than against the real
// engine. What is being tested is policy, and policy is decided entirely from
// a version -- so a model that tracks which keys live in which file exercises
// the same decisions in a millisecond that a real workload would take minutes
// to reach, and lets the naive picker be run side by side for comparison.
// The model is not self-certifying either: every version it builds goes
// through manifest.Apply, which rejects a level whose files overlap, so a
// compaction that produced an illegal layout fails the test rather than
// quietly skewing the chart.

const (
	simKeyspace    = 1000 // keys 0..999
	simBuckets     = 10   // reported in ten equal ranges
	simFlushes     = 120
	simKeysPerCut  = 40 // keys per output file, the model's TargetFileBytes
	simBytesPerKey = 1024
)

func simKey(k int) []byte { return []byte(fmt.Sprintf("%04d", k)) }

// tree is a model of the LSM: a version, plus the key set behind each file.
type tree struct {
	t       *testing.T
	version *manifest.Version
	keys    map[uint64][]int
	next    uint64
}

func newTree(t *testing.T) *tree {
	return &tree{t: t, version: manifest.NewVersion(), keys: map[uint64][]int{}, next: 1}
}

// meta builds the metadata for a file holding keys, which must be sorted.
func (tr *tree) meta(keys []int) *manifest.FileMetadata {
	number := tr.next
	tr.next++
	tr.keys[number] = keys

	return &manifest.FileMetadata{
		Number:      number,
		Size:        uint64(len(keys) * simBytesPerKey),
		Smallest:    simKey(keys[0]),
		Largest:     simKey(keys[len(keys)-1]),
		SmallestSeq: number,
		LargestSeq:  number,
	}
}

func (tr *tree) apply(e *manifest.VersionEdit) {
	tr.t.Helper()

	next, err := tr.version.Apply(e)
	if err != nil {
		tr.t.Fatalf("the model produced an illegal version: %v", err)
	}
	tr.version = next
}

// flush adds one L0 file, the model of a memtable reaching its threshold.
func (tr *tree) flush(keys []int) {
	var e manifest.VersionEdit
	e.AddFile(0, tr.meta(keys))
	tr.apply(&e)
}

// run executes a compaction the way the real executor will: merge the inputs'
// keys, keep one copy of each, and roll an output file every simKeysPerCut
// keys.
func (tr *tree) run(c *Compaction) {
	merged := map[int]bool{}
	for _, f := range c.Inputs() {
		for _, k := range tr.keys[f.Number] {
			merged[k] = true
		}
	}

	all := make([]int, 0, len(merged))
	for k := range merged {
		all = append(all, k)
	}
	sort.Ints(all)

	var e manifest.VersionEdit
	for _, f := range c.Base {
		e.DeleteFile(c.Level, f.Number)
	}
	for _, f := range c.Parent {
		e.DeleteFile(c.OutputLevel(), f.Number)
	}
	for start := 0; start < len(all); start += simKeysPerCut {
		end := min(start+simKeysPerCut, len(all))
		e.AddFile(c.OutputLevel(), tr.meta(all[start:end]))
	}
	tr.apply(&e)
}

// picker is the interface the sweep compares two implementations across.
type picker interface {
	Pick(*manifest.Version) *Compaction
}

// naivePicker is the "try the obvious thing first" picker T6.1 asks for: same
// scoring, but it always seeds from the first file of the level. It exists to
// be beaten.
type naivePicker struct{ opts Options }

func (n naivePicker) Pick(v *manifest.Version) *Compaction {
	// Reuse the real picker with its pointer pinned at the start of every
	// level, which is exactly "always take the first file".
	p := NewPicker(n.opts)
	return p.Pick(v)
}

// sweep runs the workload against a picker and returns the number of
// compactions whose key range covered each bucket, alongside the number of
// keys written into each -- the chart is only readable next to the workload
// that produced it.
func sweep(t *testing.T, p picker) (touched, written []int) {
	t.Helper()

	// Fixed seed: a distribution chart that moves between runs cannot be
	// compared against the one in the docs.
	rng := rand.New(rand.NewSource(20260911))
	tr := newTree(t)
	touched = make([]int, simBuckets)
	written = make([]int, simBuckets)

	for f := 0; f < simFlushes; f++ {
		keys := skewedKeys(rng, 60)
		for _, k := range keys {
			written[bucketOf(simKey(k))]++
		}
		tr.flush(keys)

		// Compact to quiescence after each flush, the way the scheduler in
		// T6.4 will. The bound is a safety net: a picker that never reduced
		// a level's score would otherwise spin here forever.
		for i := 0; i < 200; i++ {
			c := p.Pick(tr.version)
			if c == nil {
				break
			}
			smallest, largest := c.Range()
			for b := bucketOf(smallest); b <= bucketOf(largest); b++ {
				touched[b]++
			}
			tr.run(c)
		}
	}
	return touched, written
}

// skewedKeys draws n keys with 80% of them from the bottom tenth of the
// keyspace -- the hot-range workload that breaks a naive picker.
func skewedKeys(rng *rand.Rand, n int) []int {
	set := map[int]bool{}
	for len(set) < n {
		if rng.Float64() < 0.8 {
			set[rng.Intn(simKeyspace/10)] = true
			continue
		}
		set[rng.Intn(simKeyspace)] = true
	}

	keys := make([]int, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	return keys
}

func bucketOf(key []byte) int {
	var k int
	if _, err := fmt.Sscanf(string(key), "%04d", &k); err != nil {
		panic("sweep: unparseable model key " + string(key))
	}
	return min(k*simBuckets/simKeyspace, simBuckets-1)
}

// chart renders the per-key-range compaction counts, which is the artifact
// T6.1 asks for. It goes to the test log so `go test -v -run Sweep` prints it
// and docs/benchmarks.md can quote it.
func chart(title string, counts []int) string {
	peak := 1
	for _, c := range counts {
		if c > peak {
			peak = c
		}
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "\n%s\n", title)
	for b, c := range counts {
		lo := b * simKeyspace / simBuckets
		hi := (b+1)*simKeyspace/simBuckets - 1
		fmt.Fprintf(&sb, "  %04d-%04d %-40s %d\n", lo, hi, strings.Repeat("#", c*40/peak), c)
	}
	return sb.String()
}

// spread is the ratio of most- to least-compacted bucket over a range of
// them. One means perfectly even; larger means some ranges get more
// compaction attention than others.
func spread(counts []int) float64 {
	lo, hi := counts[0], counts[0]
	for _, c := range counts {
		lo = min(lo, c)
		hi = max(hi, c)
	}
	if lo == 0 {
		return math.Inf(1)
	}
	return float64(hi) / float64(lo)
}

// TestSweepDistributesCompactionAcrossTheKeyspace is the done-when condition.
//
// The measure needs care, because evenness across the whole keyspace is the
// wrong target: a skewed workload *should* compact the hot range most, since
// that is where the data is, and a picker that ignored that would be ignoring
// the workload. The question is whether compaction attention tracks write
// volume or merely key position.
//
// The workload makes that separable. Buckets 1 through 9 each receive the
// same share of writes -- only bucket 0 is hot -- so a picker whose choices
// follow the data must treat those nine alike. The naive picker does not: it
// restarts at the bottom of every level each time, so attention decays
// monotonically with key position across nine ranges that were written
// identically. That decay is pure position bias, and it is what the
// round-robin pointer removes.
func TestSweepDistributesCompactionAcrossTheKeyspace(t *testing.T) {
	opts := Options{
		L0Trigger:       4,
		BaseLevelBytes:  simKeysPerCut * simBytesPerKey * 4,
		LevelMultiplier: 10,
		MaxInputBytes:   simKeysPerCut * simBytesPerKey * 8,
	}

	roundRobin, written := sweep(t, NewPicker(opts))
	naive, _ := sweep(t, naivePicker{opts: opts})

	t.Log(chart("keys written into each range (the workload)", written))
	t.Log(chart("round-robin picker: compactions covering each key range", roundRobin))
	t.Log(chart("naive picker (always the first file): compactions covering each key range", naive))

	// Across the nine equally-written ranges.
	rrSpread, naiveSpread := spread(roundRobin[1:]), spread(naive[1:])
	t.Logf("spread over the equally-written ranges: round-robin %.2fx, naive %.2fx", rrSpread, naiveSpread)

	if rrSpread > 1.6 {
		t.Errorf("round-robin spread is %.2fx over ranges that received the same writes; "+
			"the pointer is not sweeping evenly", rrSpread)
	}

	// The before-and-after is the point of the exercise, so it is asserted
	// rather than left to the reader of the chart. If the naive picker ever
	// stops concentrating, this test should fail and be re-derived rather
	// than silently keep passing.
	if naiveSpread < 2.0 {
		t.Errorf("the naive picker's spread is only %.2fx; the pathology this task rests on is gone",
			naiveSpread)
	}
	if naiveSpread <= rrSpread {
		t.Errorf("round-robin (%.2fx) did not beat naive (%.2fx)", rrSpread, naiveSpread)
	}
}
