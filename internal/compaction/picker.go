package compaction

import (
	"bytes"
	"sort"
	"sync"

	"github.com/AbishekRaj2007/Strata/internal/manifest"
)

// Compaction is one unit of work the executor can run: the files to merge,
// the level their output goes to, and whether tombstones may be dropped.
//
// It is a plain description with no behaviour of its own. The picker decides
// it against a version, the executor merges it, and the committer installs
// the result -- three steps that can each be tested without the other two.
type Compaction struct {
	// Level is the level the compaction reads from.
	Level int

	// Base is the input files from Level, and Parent the overlapping files
	// from Level+1. They are separate because the commit path needs to know
	// which level each input is being deleted from, and because Parent being
	// empty is the common, cheap case worth recognising.
	Base   []*manifest.FileMetadata
	Parent []*manifest.FileMetadata

	// Score is what the level scored when this was picked, recorded for
	// logging and for the amplification study rather than used in the merge.
	Score float64

	// BottomMost reports that no level below the output level holds a file
	// overlapping this compaction's key range, which is the precondition for
	// dropping a tombstone. It is computed against the version the
	// compaction was picked from; see Pick for why that is safe.
	BottomMost bool
}

// OutputLevel is the level the merged output is written to.
func (c *Compaction) OutputLevel() int { return c.Level + 1 }

// Inputs returns every file the compaction reads, base level first.
func (c *Compaction) Inputs() []*manifest.FileMetadata {
	all := make([]*manifest.FileMetadata, 0, len(c.Base)+len(c.Parent))
	all = append(all, c.Base...)
	return append(all, c.Parent...)
}

// TotalBytes is the size of every input, which is what MaxInputBytes bounds.
func (c *Compaction) TotalBytes() uint64 {
	var total uint64
	for _, f := range c.Inputs() {
		total += f.Size
	}
	return total
}

// Range returns the key range the compaction covers, across both levels.
func (c *Compaction) Range() (smallest, largest []byte) {
	return keyRange(c.Inputs())
}

// Picker chooses what to compact next.
//
// It holds one piece of state, the per-level round-robin pointer, which is
// what stops compaction from hammering a single key range. A picker that
// always chose the first file of a level would rewrite the bottom of the
// keyspace repeatedly while the top went uncompacted -- and under a skewed
// workload that is not a theoretical concern, it is the behaviour you get.
type Picker struct {
	opts Options

	// mu guards the pointers. Pick is called from the compactor goroutine
	// today, but the scheduler in T6.4 also calls it to decide whether to
	// stall, so it must be safe from more than one goroutine.
	mu sync.Mutex

	// pointers[level] is the largest key the last compaction of that level
	// covered. The next one starts above it, wrapping at the end of the
	// level.
	pointers [manifest.NumLevels][]byte
}

// NewPicker returns a picker using opts, with every field defaulted.
func NewPicker(opts Options) *Picker {
	return &Picker{opts: opts.withDefaults()}
}

// Options returns the picker's effective options, defaults applied.
func (p *Picker) Options() Options { return p.opts }

// Pick returns the compaction to run against v, or nil if no level is over
// budget.
//
// The version is a snapshot, and the compaction describes files in it. That
// is safe because the caller runs the compaction against the same version it
// picked from and the commit path re-checks the inputs are still live: a
// version installed in between cannot invalidate the merge, only the edit
// that commits it, which is where the conflict is caught.
func (p *Picker) Pick(v *manifest.Version) *Compaction {
	level, score := p.worstLevel(v)
	if level < 0 {
		return nil
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	seed := p.seedLocked(v, level)
	if seed == nil {
		return nil
	}

	c := &Compaction{Level: level, Score: score}
	if level == 0 {
		c.Base = expandL0(v, seed)
	} else {
		c.Base = []*manifest.FileMetadata{seed}
	}

	smallest, largest := keyRange(c.Base)
	c.Parent = v.Overlapping(level+1, smallest, largest)

	// Growing the base set is free work when it pulls in no extra parent
	// files: those parents are being rewritten regardless, so merging another
	// base file into them costs only that file's own bytes. This is the one
	// discretionary part of input selection, and the only part MaxInputBytes
	// can bound -- see growBaseLocked.
	if level > 0 {
		c.Base = p.growBase(v, c)
		smallest, largest = keyRange(c.Base)
	}

	full, fullLargest := keyRange(c.Inputs())
	c.BottomMost = isBottomMost(v, c.OutputLevel(), full, fullLargest)

	// Advance the pointer past what was just chosen, so the next compaction
	// of this level starts further along the keyspace. It moves at pick time
	// rather than at commit: a compaction that fails should not be retried
	// forever on the same file while the rest of the level goes untouched.
	p.pointers[level] = append([]byte(nil), largest...)

	return c
}

// worstLevel returns the level with the highest score at or above 1.0, and
// that score. It returns -1 when nothing is over budget.
//
// The bottom level is excluded because there is nowhere below it to compact
// into. Its files are rewritten only as the output of the level above.
func (p *Picker) worstLevel(v *manifest.Version) (int, float64) {
	best, bestScore := -1, 0.0

	for level := 0; level < manifest.NumLevels-1; level++ {
		score := p.opts.Score(v, level)
		if score < 1.0 {
			continue
		}
		if best < 0 || score > bestScore {
			best, bestScore = level, score
		}
	}
	return best, bestScore
}

// seedLocked chooses the file a compaction of level starts from, sweeping the
// keyspace rather than returning to the same place. The caller holds mu.
func (p *Picker) seedLocked(v *manifest.Version, level int) *manifest.FileMetadata {
	files := v.Files(level)
	if len(files) == 0 {
		return nil
	}

	// L0 is stored newest-first by file number, which says nothing about key
	// position, so the sweep needs a key-ordered view of it. The copy is
	// bounded by the L0 stall threshold -- a dozen pointers at most.
	if level == 0 {
		sorted := append([]*manifest.FileMetadata(nil), files...)
		sort.Slice(sorted, func(i, j int) bool {
			return bytes.Compare(sorted[i].Smallest, sorted[j].Smallest) < 0
		})
		files = sorted
	}

	pointer := p.pointers[level]
	for _, f := range files {
		if pointer == nil || bytes.Compare(f.Largest, pointer) > 0 {
			return f
		}
	}

	// The sweep reached the end of the level. Wrap, which is also what
	// happens the first time a level is compacted after its keys shift down.
	return files[0]
}

// growBase extends a compaction's base inputs with adjacent files from the
// same level, for as long as doing so pulls in no additional parent files and
// keeps the total under MaxInputBytes.
//
// Both conditions matter. Dropping the first turns a bounded merge into a
// cascade down the level. Dropping the second lets one compaction run for as
// long as the level is large, which is exactly the latency spike the cap
// exists to prevent.
func (p *Picker) growBase(v *manifest.Version, c *Compaction) []*manifest.FileMetadata {
	files := v.Files(c.Level)
	if len(files) <= 1 {
		return c.Base
	}

	// The base is a contiguous run of a sorted level, so growing it means
	// walking forward from the seed's index. Only forward: the round-robin
	// pointer sweeps upward through the keyspace, and growing backwards
	// would re-compact the range the previous pick just finished.
	first := indexOf(files, c.Base[0])
	if first < 0 {
		return c.Base
	}

	last := first
	_, parentLargest := keyRange(c.Parent)
	total := c.TotalBytes()

	for last+1 < len(files) {
		next := files[last+1]
		if total+next.Size > p.opts.MaxInputBytes {
			break
		}
		// A candidate that reaches past the parents already being rewritten
		// would widen the output range and pull in more of the level below.
		if len(c.Parent) > 0 && bytes.Compare(next.Largest, parentLargest) > 0 {
			break
		}
		if len(c.Parent) == 0 && len(v.Overlapping(c.OutputLevel(), next.Smallest, next.Largest)) > 0 {
			break
		}
		total += next.Size
		last++
	}

	return append([]*manifest.FileMetadata(nil), files[first:last+1]...)
}

// expandL0 returns every L0 file overlapping the seed, iterated to a fixed
// point.
//
// The iteration is the point. Adding an L0 file widens the key range under
// consideration, which may bring a further L0 file into range, which widens
// it again. Stopping after one pass leaves a file behind, and a left-behind
// L0 file is not merely inefficient: if it is older than one that was
// compacted, its stale version of a shared key stays in L0, which the read
// path consults before the level the fresh version was just written to. The
// old value wins and a deleted or overwritten key comes back.
//
// This set is mandatory, so it is not subject to MaxInputBytes. Its size is
// bounded instead by the write path: L0 cannot exceed L0StallThreshold files,
// because writes stop before it does.
func expandL0(v *manifest.Version, seed *manifest.FileMetadata) []*manifest.FileMetadata {
	smallest, largest := seed.Smallest, seed.Largest

	var out []*manifest.FileMetadata
	for {
		grown := v.Overlapping(0, smallest, largest)
		if len(grown) == len(out) {
			break
		}
		out = grown
		smallest, largest = keyRange(out)
	}

	// Overlapping preserves the version's newest-first L0 order, which the
	// merge iterator does not depend on but a reader of the compaction log
	// will expect to match the level.
	return out
}

// isBottomMost reports whether no level below output holds a file overlapping
// [smallest, largest].
//
// This is the tombstone-dropping precondition, and it is deliberately coarse:
// it asks about the compaction's whole key range rather than about each key.
// A coarse answer can only be wrong in the safe direction -- it keeps a
// tombstone that could have been dropped, costing space, never drops one that
// was still shadowing something, which would cost data.
func isBottomMost(v *manifest.Version, output int, smallest, largest []byte) bool {
	for level := output + 1; level < manifest.NumLevels; level++ {
		if len(v.Overlapping(level, smallest, largest)) > 0 {
			return false
		}
	}
	return true
}

// keyRange returns the union of the files' key ranges.
func keyRange(files []*manifest.FileMetadata) (smallest, largest []byte) {
	for i, f := range files {
		if i == 0 {
			smallest, largest = f.Smallest, f.Largest
			continue
		}
		if bytes.Compare(f.Smallest, smallest) < 0 {
			smallest = f.Smallest
		}
		if bytes.Compare(f.Largest, largest) > 0 {
			largest = f.Largest
		}
	}
	return smallest, largest
}

// indexOf finds a file in a level by number.
func indexOf(files []*manifest.FileMetadata, target *manifest.FileMetadata) int {
	for i, f := range files {
		if f.Number == target.Number {
			return i
		}
	}
	return -1
}
