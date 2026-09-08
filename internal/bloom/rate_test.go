package bloom

import (
	"encoding/binary"
	"math"
	"testing"

	"github.com/cespare/xxhash/v2"
)

// presentKey and absentKey generate two disjoint key spaces. They are
// disjoint by construction -- different prefixes -- rather than by a set
// difference, so a mistake in the generator cannot quietly turn a false
// positive measurement into a true positive one.
func presentKey(i int) []byte {
	b := make([]byte, 12)
	copy(b, "present:")
	binary.BigEndian.PutUint32(b[8:], uint32(i))
	return b
}

func absentKey(i int) []byte {
	b := make([]byte, 12)
	copy(b, "absentt:")
	binary.BigEndian.PutUint32(b[8:], uint32(i))
	return b
}

// theoreticalFPRate is (1 - e^(-k/(m/n)))^k, the standard bloom filter false
// positive rate for k probes at m/n bits per key.
func theoreticalFPRate(bitsPerKey, probes int) float64 {
	fillRatio := 1 - math.Exp(-float64(probes)/float64(bitsPerKey))
	return math.Pow(fillRatio, float64(probes))
}

// TestFilterAtScale is T5.1's Done-when condition: 100k keys inserted, 1M
// probes confirm zero false negatives, and the measured false positive rate
// sits within 2x of theoretical.
//
// The two halves are asserted differently on purpose. A false negative is a
// correctness bug -- the filter claims a key is absent, the read path skips
// the block, and a key the database holds is reported missing -- so the bound
// is exactly zero, not "few". A false positive is only wasted work, so it is
// bounded by a ratio.
func TestFilterAtScale(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping the 1M-probe filter measurement in -short")
	}

	const (
		keys       = 100_000
		probes     = 1_000_000
		bitsPerKey = DefaultBitsPerKey
	)

	b := NewBuilder(bitsPerKey)
	for i := 0; i < keys; i++ {
		b.Add(presentKey(i))
	}
	f, err := Decode(b.Finish())
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}

	// 1M probes over the inserted keys, cycling through the set. Zero
	// tolerance.
	falseNegatives := 0
	for i := 0; i < probes; i++ {
		if !f.MayContain(presentKey(i % keys)) {
			falseNegatives++
		}
	}
	if falseNegatives != 0 {
		t.Fatalf("%d false negatives in %d probes; a bloom filter must never report an inserted key absent", falseNegatives, probes)
	}

	// 1M probes over keys that were never inserted, measuring the rate.
	falsePositives := 0
	for i := 0; i < probes; i++ {
		if f.MayContain(absentKey(i)) {
			falsePositives++
		}
	}
	measured := float64(falsePositives) / float64(probes)
	want := theoreticalFPRate(bitsPerKey, f.Probes())

	t.Logf("bitsPerKey=%d k=%d array=%d bytes: measured FP rate %.4f%%, theoretical %.4f%%",
		bitsPerKey, f.Probes(), f.SizeBytes(), measured*100, want*100)

	if measured > 2*want {
		t.Errorf("measured FP rate %.4f%% exceeds 2x the theoretical %.4f%%", measured*100, want*100)
	}
	// The lower bound matters as much as the upper one. A filter measuring
	// far *below* theory is not a better filter; it is a sign the probes are
	// not landing where the analysis assumes -- a stuck hash, a modulo that
	// folds onto a subrange -- and the same defect would show up as poor
	// filtering on a different key distribution.
	if measured < want/2 {
		t.Errorf("measured FP rate %.4f%% is below half the theoretical %.4f%%, which suggests the probes are not distributed as the derivation assumes", measured*100, want*100)
	}
}

// TestFPRateTracksTheoryAcrossBitsPerKey checks the whole curve, not one
// point. This is what makes the T5.3 sweep trustworthy: if the measured rate
// tracked theory only at 10 bits, the tuning study's recommendations at 4 and
// 16 would be extrapolation.
func TestFPRateTracksTheoryAcrossBitsPerKey(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping the multi-configuration filter measurement in -short")
	}

	const (
		keys   = 50_000
		trials = 200_000
	)

	for _, bitsPerKey := range []int{4, 8, 10, 16} {
		b := NewBuilder(bitsPerKey)
		for i := 0; i < keys; i++ {
			b.Add(presentKey(i))
		}
		f, err := Decode(b.Finish())
		if err != nil {
			t.Fatalf("bitsPerKey=%d: Decode: %v", bitsPerKey, err)
		}

		for i := 0; i < keys; i++ {
			if !f.MayContain(presentKey(i)) {
				t.Fatalf("bitsPerKey=%d: false negative on key %d", bitsPerKey, i)
			}
		}

		hits := 0
		for i := 0; i < trials; i++ {
			if f.MayContain(absentKey(i)) {
				hits++
			}
		}
		measured := float64(hits) / float64(trials)
		want := theoreticalFPRate(bitsPerKey, f.Probes())
		t.Logf("bitsPerKey=%2d k=%2d array=%7d bytes: measured %.4f%%, theoretical %.4f%%",
			bitsPerKey, f.Probes(), f.SizeBytes(), measured*100, want*100)

		if measured > 2*want || measured < want/2 {
			t.Errorf("bitsPerKey=%d: measured FP rate %.4f%% is not within 2x of the theoretical %.4f%%", bitsPerKey, measured*100, want*100)
		}
	}
}

// TestExcessProbesMakeTheFilterWorse demonstrates the claim the derivation
// rests on: k past the optimum saturates the bit array and the false positive
// rate climbs again. Without this the choice of k is an assertion; with it, it
// is a measurement.
func TestExcessProbesMakeTheFilterWorse(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping the probe-count sweep in -short")
	}

	const (
		keys       = 50_000
		trials     = 200_000
		bitsPerKey = DefaultBitsPerKey
	)

	rateAt := func(probes int) float64 {
		// Build the array directly at a forced k rather than going through
		// Builder, which always picks the optimum.
		arrayBytes := keys * bitsPerKey / 8
		bits := make([]byte, arrayBytes)
		nbits := uint32(arrayBytes * 8)
		set := func(key []byte) {
			h := xxhash.Sum64(key)
			h1, h2 := uint32(h), uint32(h>>32)
			for i := 0; i < probes; i++ {
				bits[(h1%nbits)/8] |= 1 << ((h1 % nbits) % 8)
				h1 += h2
			}
		}
		for i := 0; i < keys; i++ {
			set(presentKey(i))
		}
		f := &Filter{probes: uint32(probes), bits: bits, nbits: nbits}

		hits := 0
		for i := 0; i < trials; i++ {
			if f.MayContain(absentKey(i)) {
				hits++
			}
		}
		return float64(hits) / float64(trials)
	}

	optimal := OptimalProbes(bitsPerKey)
	best := rateAt(optimal)
	tooFew := rateAt(2)
	tooMany := rateAt(20)

	t.Logf("k=2: %.4f%%  k=%d (optimal): %.4f%%  k=20: %.4f%%", tooFew*100, optimal, best*100, tooMany*100)

	if best >= tooFew {
		t.Errorf("k=%d gave %.4f%%, no better than k=2's %.4f%%", optimal, best*100, tooFew*100)
	}
	if best >= tooMany {
		t.Errorf("k=%d gave %.4f%%, no better than k=20's %.4f%%; the curve does not turn back up", optimal, best*100, tooMany*100)
	}
}
