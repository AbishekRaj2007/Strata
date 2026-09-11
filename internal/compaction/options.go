package compaction

import "github.com/AbishekRaj2007/Strata/internal/manifest"

// Defaults for the shape of the tree. They are the values plan.md's Phase 6
// preamble fixes, collected here so the tuning sweep in T6.6 can vary one
// without hunting through the package for a constant.
const (
	// DefaultL0Trigger is the number of L0 files that makes L0 compactable.
	// L0 is scored by file count rather than bytes because what L0 costs is
	// paid per file: every L0 file has to be consulted on every read that
	// its key range admits, however small it is.
	DefaultL0Trigger = 4

	// DefaultL0StallThreshold is the L0 file count at which writes stop
	// entirely. It is here rather than in the scheduler because it also
	// bounds the worst-case size of an L0 compaction -- see Pick.
	DefaultL0StallThreshold = 12

	// DefaultBaseLevelBytes is L1's byte target. Every level below it targets
	// LevelMultiplier times the one above.
	DefaultBaseLevelBytes = 8 << 20

	// DefaultLevelMultiplier is the ratio between consecutive level targets,
	// the 10x that ADR-002 accepted as the leveled-compaction trade.
	DefaultLevelMultiplier = 10

	// DefaultTargetFileBytes is the size an output file rolls at.
	DefaultTargetFileBytes = 2 << 20

	// DefaultMaxInputBytes caps the optional expansion of a compaction's
	// inputs, bounding how long one compaction can run.
	DefaultMaxInputBytes = 64 << 20
)

// Options configures the picker and the executor.
//
// A zero Options is usable: every field falls back to its default, so a test
// that does not care about the shape of the tree does not have to describe
// one.
type Options struct {
	// L0Trigger is the L0 file count that scores 1.0.
	L0Trigger int

	// L0StallThreshold is the L0 file count at which the write path stalls.
	L0StallThreshold int

	// BaseLevelBytes is L1's target size in bytes.
	BaseLevelBytes uint64

	// LevelMultiplier is the ratio between one level's target and the next.
	LevelMultiplier uint64

	// TargetFileBytes is the size at which an output file is rolled.
	TargetFileBytes uint64

	// MaxInputBytes bounds the total input of one compaction. It is a cap on
	// discretionary expansion, not a hard limit -- see Pick, where the L0
	// overlap set is mandatory for correctness and is allowed to exceed it.
	MaxInputBytes uint64
}

// withDefaults returns opts with every unset field filled in.
func (o Options) withDefaults() Options {
	if o.L0Trigger <= 0 {
		o.L0Trigger = DefaultL0Trigger
	}
	if o.L0StallThreshold <= 0 {
		o.L0StallThreshold = DefaultL0StallThreshold
	}
	if o.BaseLevelBytes == 0 {
		o.BaseLevelBytes = DefaultBaseLevelBytes
	}
	if o.LevelMultiplier == 0 {
		o.LevelMultiplier = DefaultLevelMultiplier
	}
	if o.TargetFileBytes == 0 {
		o.TargetFileBytes = DefaultTargetFileBytes
	}
	if o.MaxInputBytes == 0 {
		o.MaxInputBytes = DefaultMaxInputBytes
	}
	return o
}

// TargetBytes returns the byte budget for a level.
//
// L0 has no byte target -- it is scored by file count -- and returns zero,
// which callers must not divide by. L1 gets BaseLevelBytes and each level
// below multiplies by LevelMultiplier, which is the geometry that makes the
// number of levels logarithmic in the data size and therefore makes read
// amplification logarithmic too.
func (o Options) TargetBytes(level int) uint64 {
	o = o.withDefaults()
	if level <= 0 {
		return 0
	}

	target := o.BaseLevelBytes
	for i := 1; i < level; i++ {
		target *= o.LevelMultiplier
	}
	return target
}

// Score rates how badly a level needs compacting. A score at or above 1.0
// means the level is over budget.
//
// The two rules are not arbitrary: L0's cost to a reader is the number of
// files it must check, and a level below L0 costs a reader exactly one file
// however many it holds, so what matters there is the bytes it keeps out of
// the level below. Scoring both by the same measure would either let L0 grow
// until reads collapsed or compact deep levels that nothing was waiting on.
func (o Options) Score(v *manifest.Version, level int) float64 {
	o = o.withDefaults()

	if level == 0 {
		return float64(v.NumFiles(0)) / float64(o.L0Trigger)
	}
	return float64(v.LevelBytes(level)) / float64(o.TargetBytes(level))
}
