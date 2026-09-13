package model

import (
	"fmt"
	"math/rand"
)

// Distribution names how a generated sequence chooses its keys.
//
// The choice matters more than the operation count. Uniform random keys over
// a wide space exercise almost nothing interesting: every key is touched once,
// nothing is ever overwritten, no tombstone ever shadows a live value, and no
// two keys share a prefix. A million such operations find fewer bugs than a
// thousand drawn from a distribution that creates collisions and shared
// structure on purpose.
type Distribution string

const (
	// DistUniform draws keys uniformly from the key space. It is the
	// baseline: broad coverage, shallow interactions.
	DistUniform Distribution = "uniform"

	// DistZipfian concentrates most operations on a few keys, which is what
	// real workloads do. A hot key is overwritten constantly, so its
	// versions end up spread across the memtable and every level, and a
	// read of it has to resolve a chain of shadowed entries in the right
	// order. That is where sequence-number ordering bugs surface.
	DistZipfian Distribution = "zipfian"

	// DistSequential walks the key space in order. Sequential insertion is
	// the degenerate case for a leveled store: every flush produces a table
	// whose range abuts rather than overlaps its predecessor, block restart
	// intervals compress unusually well, and the picker's overlap
	// calculation sees an input set it rarely sees under random load.
	DistSequential Distribution = "sequential"

	// DistPrefix draws from keys that all share a long common prefix and
	// differ only in their last few bytes. This is adversarial for prefix
	// compression inside blocks and for every comparison that is tempted to
	// stop early: two keys that agree for 48 bytes and differ in the 49th
	// are exactly the pair a subtly wrong comparator gets wrong.
	DistPrefix Distribution = "prefix"
)

// keySource produces the next key for a generated operation.
type keySource interface {
	next(rng *rand.Rand) string
}

// newKeySource builds the generator for a distribution over a key space of
// the given size.
func newKeySource(d Distribution, keySpace int) keySource {
	switch d {
	case DistZipfian:
		return &zipfianKeys{space: uint64(keySpace)}
	case DistSequential:
		return &sequentialKeys{space: keySpace}
	case DistPrefix:
		return &prefixKeys{space: keySpace}
	default:
		return &uniformKeys{space: keySpace}
	}
}

type uniformKeys struct{ space int }

func (u *uniformKeys) next(rng *rand.Rand) string {
	return fmt.Sprintf("key-%04d", rng.Intn(u.space))
}

// zipfianKeys draws rank r with probability proportional to 1/(r+1)^s.
//
// The Zipf generator is built lazily against the first rng it sees, because
// rand.NewZipf binds to a source and the sequence must stay a pure function
// of the seed the caller passed.
type zipfianKeys struct {
	space uint64
	zipf  *rand.Zipf
}

func (z *zipfianKeys) next(rng *rand.Rand) string {
	if z.zipf == nil {
		// s=1.1 is mildly heavier-tailed than the classic s=1; imax is one
		// below the space so the generated rank indexes a valid key.
		z.zipf = rand.NewZipf(rng, 1.1, 1, z.space-1)
	}
	return fmt.Sprintf("key-%04d", z.zipf.Uint64())
}

// sequentialKeys advances a counter, wrapping at the key space so a long run
// revisits what it already wrote rather than growing without bound.
type sequentialKeys struct {
	space int
	n     int
}

func (s *sequentialKeys) next(*rand.Rand) string {
	k := s.n % s.space
	s.n++
	return fmt.Sprintf("key-%04d", k)
}

// commonPrefix is long enough that a prefix-compressed block stores almost
// nothing but the suffix, which is the point.
const commonPrefix = "user:session:00000000-0000-0000-0000-0000000000"

type prefixKeys struct{ space int }

func (p *prefixKeys) next(rng *rand.Rand) string {
	return fmt.Sprintf("%s%04d", commonPrefix, rng.Intn(p.space))
}
