package model

import (
	"fmt"
	"math/rand"
)

// Weights control how often each operation is generated. They are weights
// rather than probabilities so a caller can express "twice as many reads as
// writes" without normalising.
type Weights struct {
	Put    int
	Get    int
	Delete int
	Scan   int
	Flush  int
	Reopen int
}

// LongRunWeights is DefaultWeights with SCAN and REOPEN made rare, for
// sequences long enough that their cost matters.
//
// Both are O(database) rather than O(operation): a full scan walks every live
// key in every table, and a reopen replays the manifest. Until compaction
// lands in Phase 6 nothing merges L0, so the table count grows with the
// sequence and a scan grows with it -- keeping SCAN at its default rate would
// make a 50,000-operation run quadratic and tell us nothing a shorter one
// does not. TestModelManySeeds exercises SCAN densely at a size where it is
// cheap; this mix is for depth instead.
var LongRunWeights = Weights{
	Put:    45,
	Get:    35,
	Delete: 15,
	Scan:   1,
	Flush:  3,
	Reopen: 1,
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

// GenConfig parameterises a generated sequence.
type GenConfig struct {
	Ops      int
	KeySpace int
	Weights  Weights
}

// Generate builds a random operation sequence.
//
// The key space is deliberately much smaller than the operation count. Keys
// have to collide for the test to mean anything: overwrites, deletes of live
// keys, and reads of keys that were deleted three flushes ago are where the
// bugs are, and a wide key space would make almost every operation touch a
// key nothing else ever touched.
func Generate(rng *rand.Rand, cfg GenConfig) []Op {
	if cfg.KeySpace <= 0 {
		cfg.KeySpace = 64
	}
	w := cfg.Weights
	total := w.Put + w.Get + w.Delete + w.Scan + w.Flush + w.Reopen
	if total <= 0 {
		w = DefaultWeights
		total = w.Put + w.Get + w.Delete + w.Scan + w.Flush + w.Reopen
	}

	ops := make([]Op, 0, cfg.Ops)
	for i := 0; i < cfg.Ops; i++ {
		key := fmt.Sprintf("key-%04d", rng.Intn(cfg.KeySpace))

		switch pick := rng.Intn(total); {
		case pick < w.Put:
			ops = append(ops, Op{
				Kind:  OpPut,
				Key:   key,
				Value: fmt.Sprintf("v%d", rng.Intn(1_000_000)),
			})
		case pick < w.Put+w.Get:
			ops = append(ops, Op{Kind: OpGet, Key: key})
		case pick < w.Put+w.Get+w.Delete:
			ops = append(ops, Op{Kind: OpDelete, Key: key})
		case pick < w.Put+w.Get+w.Delete+w.Scan:
			ops = append(ops, Op{Kind: OpScan, Count: 1 + rng.Intn(16)})
		case pick < w.Put+w.Get+w.Delete+w.Scan+w.Flush:
			ops = append(ops, Op{Kind: OpFlush})
		default:
			ops = append(ops, Op{Kind: OpReopen})
		}
	}
	return ops
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
