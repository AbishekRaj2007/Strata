package cache

import (
	"math/rand"
	"testing"
)

// TestTuningCacheSizeAgainstHitRate is the cache-size arm of the T5.3 study.
//
// Reproduce with:
//
//	go test ./internal/cache/ -run TestTuningCacheSizeAgainstHitRate -v -count 3
//
// docs/benchmarks.md records the medians. The workload is fixed and the
// capacity is the only variable, which is the point: a sweep that also changed
// the access pattern would produce a curve nobody could attribute.
func TestTuningCacheSizeAgainstHitRate(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping the tuning study measurement in -short")
	}

	const (
		blocks    = 20_000
		blockSize = 4096
		accesses  = 500_000
		working   = int64(blocks) * blockSize // 80 MiB of distinct blocks
	)

	// Two skews, because the answer depends entirely on the shape of the
	// access pattern and a single curve would read as a property of the
	// cache rather than of the workload it was measured on.
	for _, skew := range []float64{1.05, 1.2} {
		t.Logf("zipf s=%.2f over %d blocks (%d MiB of distinct data)", skew, blocks, working>>20)
		t.Log("  capacity | resident blocks | hit rate | bytes per point of hit rate")

		for _, capacity := range []int64{1 << 20, 4 << 20, 16 << 20, 64 << 20, 256 << 20} {
			c := New(capacity)
			rng := rand.New(rand.NewSource(20250908))
			zipf := rand.NewZipf(rng, skew, 1, blocks-1)
			payload := block(blockSize, 0x5A)

			for i := 0; i < accesses; i++ {
				k := Key{FileNumber: 1, BlockOffset: zipf.Uint64() * blockSize}
				if _, ok := c.Get(k); !ok {
					c.Put(k, payload)
				}
			}

			st := c.Stats()
			perPoint := float64(st.Bytes) / (st.HitRate() * 100)
			t.Logf("  %6d MiB | %15d | %7.2f%% | %26.0f",
				capacity>>20, st.Entries, st.HitRate()*100, perPoint)
		}
	}
}
