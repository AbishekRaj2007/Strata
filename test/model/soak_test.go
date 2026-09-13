package model

import (
	"flag"
	"math/rand"
	"testing"
	"time"
)

// The soak is one test with a knob rather than two tests, so that the nightly
// run and the per-commit run exercise identical code and a nightly-only
// failure is never explained by "the long test does something different".
var (
	soakOps  = flag.Int("model.ops", 100_000, "operations for TestModelSoak; the nightly run passes 10000000")
	soakSeed = flag.Int64("model.seed", 1, "seed for TestModelSoak")
	soakDist = flag.String("model.dist", "zipfian", "key distribution: uniform, zipfian, sequential, prefix")
)

// TestModelSoak is T7.1's done-when: a long randomised run against the
// reference with zero divergence.
//
// Per commit it runs 100,000 operations, which takes seconds. The nightly run
// passes -model.ops=10000000. Both use the same generator, the same weights
// and the same comparison, so the only difference is how long the engine is
// given to get something wrong.
func TestModelSoak(t *testing.T) {
	ops := *soakOps
	if testing.Short() {
		ops = 5_000
	}

	sys := newLSMSystem(t, t.TempDir(), 32<<10)
	ref := NewReference()

	start := time.Now()
	last := start
	cfg := SoakConfig{
		GenConfig: GenConfig{
			Ops:          ops,
			KeySpace:     2_048,
			Weights:      SoakWeights,
			Distribution: Distribution(*soakDist),
		},
		Progress: func(done int) {
			// Progress at most once a minute: a ten-million-operation run
			// that prints nothing for an hour cannot be told from a hung
			// one, and one that prints every chunk buries the failure.
			if time.Since(last) < time.Minute && done != ops {
				return
			}
			last = time.Now()
			elapsed := time.Since(start)
			t.Logf("%d/%d operations in %s (%.0f ops/s)",
				done, ops, elapsed.Truncate(time.Second), float64(done)/elapsed.Seconds())
		},
	}

	if err := Soak(sys, ref, rand.New(rand.NewSource(*soakSeed)), cfg); err != nil {
		t.Fatalf("divergence after %d operations (seed %d, %s): %v",
			ops, *soakSeed, *soakDist, err)
	}
}
