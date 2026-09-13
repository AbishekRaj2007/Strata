package model

import (
	"fmt"
	"math/rand"
)

// Weights control how often each operation is generated. They are weights
// rather than probabilities so a caller can express "twice as many reads as
// writes" without normalising.
type Weights struct {
	Put     int
	Get     int
	Delete  int
	Scan    int
	Flush   int
	Reopen  int
	Compact int
}

func (w Weights) total() int {
	return w.Put + w.Get + w.Delete + w.Scan + w.Flush + w.Reopen + w.Compact
}

// LongRunWeights is DefaultWeights with SCAN and REOPEN made rare, for
// sequences long enough that their cost matters.
//
// Both are O(database) rather than O(operation): a full scan walks every live
// key in every table, and a reopen replays the manifest. Keeping SCAN at its
// default rate would make a long run quadratic in the key space and tell us
// nothing a shorter one does not. TestModelManySeeds exercises SCAN densely
// at a size where it is cheap; this mix is for depth instead.
var LongRunWeights = Weights{
	Put:     45,
	Get:     35,
	Delete:  15,
	Scan:    1,
	Flush:   3,
	Reopen:  1,
	Compact: 0,
}

// DefaultWeights is a write-heavy mix with enough reads to notice a wrong
// answer and enough flushes and reopens to keep the durable paths in play.
//
// Flush and Reopen are deliberately rare. They are expensive, and more
// importantly their value is in landing at unpredictable points relative to
// the reads around them, which a low rate over many operations achieves
// better than a high rate over few.
var DefaultWeights = Weights{
	Put:    40,
	Get:    30,
	Delete: 15,
	Scan:   10,
	Flush:  4,
	Reopen: 1,
}

// SoakWeights is the mix for runs measured in millions of operations.
//
// The structural operations are rare by ratio rather than absent, because at
// ten million operations a rate that looks negligible is not: one reopen per
// ten thousand operations is still a thousand recoveries, and one flush per
// thousand is ten thousand tables through the compactor. The mix that makes a
// fifty-thousand-operation run interesting makes a ten-million-operation run
// spend all its time in restart rather than in the engine.
//
// The weights are large so these ratios can be expressed as integers.
var SoakWeights = Weights{
	Put:     5_000,
	Get:     4_000,
	Delete:  977,
	Scan:    10,
	Flush:   9,
	Compact: 3,
	Reopen:  1,
}

// StructuralWeights deliberately drives the tree's shape rather than its
// contents: rotations and compactions are frequent enough that reads land in
// the middle of them.
//
// The reason to generate COMPACT as an operation rather than to let it happen
// on its own is reproducibility. A compaction that fires because a background
// threshold happened to trip is a different sequence on every run; a
// compaction at operation 4,312 of seed 7 is the same sequence every time,
// and so is shrinkable.
var StructuralWeights = Weights{
	Put:     40,
	Get:     25,
	Delete:  15,
	Scan:    2,
	Flush:   8,
	Reopen:  2,
	Compact: 8,
}

// GenConfig parameterises a generated sequence.
type GenConfig struct {
	Ops      int
	KeySpace int
	Weights  Weights

	// Distribution selects how keys are drawn. The zero value is
	// DistUniform, which is the least interesting of the four and so the
	// one worth naming explicitly when it is what you want.
	Distribution Distribution

	// ValueSize is the length of generated values in bytes. Zero selects a
	// short value; large values matter because they change how many entries
	// fit in a block and how often a memtable rotates.
	ValueSize int
}

// Generate builds a random operation sequence.
//
// The key space is deliberately much smaller than the operation count. Keys
// have to collide for the test to mean anything: overwrites, deletes of live
// keys, and reads of keys that were deleted three flushes ago are where the
// bugs are, and a wide key space would make almost every operation touch a
// key nothing else ever touched.
func Generate(rng *rand.Rand, cfg GenConfig) []Op {
	return NewGenerator(rng, cfg).Generate(cfg.Ops)
}

// Generator produces operations from a fixed configuration, carrying the
// state a distribution needs across calls.
//
// The state is why this type exists rather than a second parameter to
// Generate: a sequential key source is a counter, and restarting it on every
// call would make a run generated in pieces differ from the same run
// generated in one go. A long soak generates in pieces by necessity, and it
// has to be the same sequence.
type Generator struct {
	rng  *rand.Rand
	cfg  GenConfig
	w    Weights
	tot  int
	keys keySource
}

// NewGenerator prepares a generator. Zero values in cfg take their defaults
// here, once, rather than on every call.
func NewGenerator(rng *rand.Rand, cfg GenConfig) *Generator {
	if cfg.KeySpace <= 0 {
		cfg.KeySpace = 64
	}
	w := cfg.Weights
	if w.total() <= 0 {
		w = DefaultWeights
	}
	return &Generator{
		rng:  rng,
		cfg:  cfg,
		w:    w,
		tot:  w.total(),
		keys: newKeySource(cfg.Distribution, cfg.KeySpace),
	}
}

// Generate returns the next n operations.
func (g *Generator) Generate(n int) []Op {
	rng, cfg, w, total := g.rng, g.cfg, g.w, g.tot

	ops := make([]Op, 0, n)
	for i := 0; i < n; i++ {
		key := g.keys.next(rng)

		switch pick := rng.Intn(total); {
		case pick < w.Put:
			ops = append(ops, Op{Kind: OpPut, Key: key, Value: genValue(rng, cfg.ValueSize)})
		case pick < w.Put+w.Get:
			ops = append(ops, Op{Kind: OpGet, Key: key})
		case pick < w.Put+w.Get+w.Delete:
			ops = append(ops, Op{Kind: OpDelete, Key: key})
		case pick < w.Put+w.Get+w.Delete+w.Scan:
			ops = append(ops, Op{Kind: OpScan, Count: 1 + rng.Intn(16)})
		case pick < w.Put+w.Get+w.Delete+w.Scan+w.Flush:
			ops = append(ops, Op{Kind: OpFlush})
		case pick < w.Put+w.Get+w.Delete+w.Scan+w.Flush+w.Reopen:
			ops = append(ops, Op{Kind: OpReopen})
		default:
			ops = append(ops, Op{Kind: OpCompact})
		}
	}
	return ops
}

// genValue builds a value of the requested size. Values are distinct so that
// a read returning a stale version is a visible difference rather than a
// coincidence.
func genValue(rng *rand.Rand, size int) string {
	v := fmt.Sprintf("v%d", rng.Intn(1_000_000))
	if size <= len(v) {
		return v
	}
	pad := make([]byte, size-len(v))
	for i := range pad {
		pad[i] = byte('a' + rng.Intn(26))
	}
	return v + string(pad)
}

// Shrink reduces a failing sequence to a minimal one that still fails.
//
// A 50,000-operation failure is a fact, not a diagnosis. What makes model
// testing worth the trouble is that it hands back the three operations that
// actually matter, so the bug can be read off the sequence rather than
// hunted for.
//
// The strategy is delta debugging: repeatedly try removing a contiguous chunk
// and keep the removal if the sequence still fails, halving the chunk size
// when a full pass achieves nothing. That converges on a local minimum in
// O(n log n) attempts rather than the O(2^n) an exhaustive search would need.
//
// stillFails must be a fresh run each time -- a new directory, a new engine --
// or the shrinker is testing the residue of previous attempts.
func Shrink(ops []Op, stillFails func([]Op) bool) []Op {
	current := ops

	for chunk := len(current) / 2; chunk >= 1; chunk /= 2 {
		changed := true
		for changed {
			changed = false
			for start := 0; start+chunk <= len(current); {
				candidate := make([]Op, 0, len(current)-chunk)
				candidate = append(candidate, current[:start]...)
				candidate = append(candidate, current[start+chunk:]...)

				if len(candidate) > 0 && stillFails(candidate) {
					current = candidate
					changed = true
					continue // the window now holds different operations
				}
				start += chunk
			}
		}
	}
	return current
}
