package model

import (
	"fmt"
	"math/rand"
)

// DefaultChunk is how many operations a soak run generates at a time.
//
// Ten million Op values held at once would be gigabytes of live heap, and a
// harness that dies of its own memory use proves nothing about the engine.
// Generating in chunks keeps the footprint flat while leaving the sequence a
// pure function of the seed: the same seed produces the same ten million
// operations in the same order regardless of chunk size, because one rng
// drives the whole run.
const DefaultChunk = 50_000

// SoakConfig describes a long run.
type SoakConfig struct {
	GenConfig

	// Chunk bounds how many operations are materialised at once. Zero
	// selects DefaultChunk.
	Chunk int

	// Progress, if set, is called after each chunk with the number of
	// operations completed so far. A ten-million-operation run that prints
	// nothing for an hour is indistinguishable from a hung one.
	Progress func(done int)
}

// Soak replays a long generated sequence against both implementations in
// bounded memory, reporting the first divergence.
//
// The returned Divergence index is absolute across the whole run, not
// relative to the chunk it was found in, so it names the operation a reader
// would count to.
func Soak(sys System, ref *Reference, rng *rand.Rand, cfg SoakConfig) error {
	chunk := cfg.Chunk
	if chunk <= 0 {
		chunk = DefaultChunk
	}

	gen := NewGenerator(rng, cfg.GenConfig)

	for done := 0; done < cfg.Ops; {
		n := chunk
		if remaining := cfg.Ops - done; n > remaining {
			n = remaining
		}

		if err := Run(sys, ref, gen.Generate(n)); err != nil {
			var d Divergence
			if as, ok := err.(Divergence); ok {
				d = as
				d.Index += done
				return d
			}
			return fmt.Errorf("chunk starting at %d: %w", done, err)
		}

		done += n
		if cfg.Progress != nil {
			cfg.Progress(done)
		}
	}
	return nil
}
