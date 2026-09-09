package bloom

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"testing"
)

func build(t *testing.T, bitsPerKey int, keys [][]byte) *Filter {
	t.Helper()

	b := NewBuilder(bitsPerKey)
	for _, k := range keys {
		b.Add(k)
	}
	f, err := Decode(b.Finish())
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	return f
}

func keyRange(n int) [][]byte {
	keys := make([][]byte, n)
	for i := range keys {
		keys[i] = []byte(fmt.Sprintf("key-%08d", i))
	}
	return keys
}

func TestOptimalProbesFollowsTheDerivation(t *testing.T) {
	// k = (m/n)*ln2, rounded. These are the values the derivation gives, not
	// values read back off the implementation.
	cases := []struct {
		bitsPerKey int
		want       int
	}{
		{1, 1},   // 0.69 -> 1
		{2, 1},   // 1.38 -> 1
		{4, 3},   // 2.76 -> 3
		{8, 6},   // 5.52 -> 6
		{10, 7},  // 6.90 -> 7
		{16, 11}, // 11.04 -> 11
		{20, 14}, // 13.80 -> 14
	}
	for _, c := range cases {
		if got := OptimalProbes(c.bitsPerKey); got != c.want {
			t.Errorf("OptimalProbes(%d) = %d, want %d", c.bitsPerKey, got, c.want)
		}
	}

	// k is clamped at both ends: never zero (a filter with no probes tests
	// nothing) and never unbounded (probing costs more than the block read
	// it saves).
	if got := OptimalProbes(0); got != 1 {
		t.Errorf("OptimalProbes(0) = %d, want the floor of 1", got)
	}
	if got := OptimalProbes(1000); got != maxProbes {
		t.Errorf("OptimalProbes(1000) = %d, want the ceiling of %d", got, maxProbes)
	}
}

func TestFilterNeverReportsAnAddedKeyAbsent(t *testing.T) {
	// A false negative is data loss, so this runs across every bits-per-key
	// the tuning study sweeps rather than only the default.
	for _, bits := range []int{1, 4, 8, 10, 16} {
		keys := keyRange(5000)
		f := build(t, bits, keys)
		for _, k := range keys {
			if !f.MayContain(k) {
				t.Fatalf("bitsPerKey=%d: MayContain(%q) = false for a key that was added", bits, k)
			}
		}
	}
}

func TestEmptyFilterReportsEverythingAbsent(t *testing.T) {
	f := build(t, DefaultBitsPerKey, nil)
	if f.SizeBytes() != 0 {
		t.Errorf("SizeBytes = %d, want 0 for a filter with no keys", f.SizeBytes())
	}
	for _, k := range keyRange(100) {
		if f.MayContain(k) {
			t.Errorf("MayContain(%q) = true against an empty filter", k)
		}
	}
}

func TestBuilderCollapsesRepeatsOfTheSameKey(t *testing.T) {
	// The SSTable writer feeds every version of a key, adjacent and in
	// order. Sizing the bit array by version count instead of distinct-key
	// count would waste memory in proportion to the update rate.
	b := NewBuilder(DefaultBitsPerKey)
	for i := 0; i < 100; i++ {
		b.Add([]byte("same"))
	}
	b.Add([]byte("other"))
	for i := 0; i < 100; i++ {
		b.Add([]byte("other"))
	}
	if b.Keys() != 2 {
		t.Fatalf("Keys = %d, want 2 distinct keys from 202 adds", b.Keys())
	}
}

func TestSmallFilterIsFlooredSoItStillFilters(t *testing.T) {
	// Two keys at 10 bits each is 20 bits -- 3 bytes -- which folds seven
	// probes onto so few bits that the filter would say "maybe" to
	// everything. The floor is what makes a tiny table's filter worth
	// consulting.
	f := build(t, DefaultBitsPerKey, [][]byte{[]byte("a"), []byte("b")})
	if f.SizeBytes() < minBitArrayBytes {
		t.Fatalf("SizeBytes = %d, want at least the %d-byte floor", f.SizeBytes(), minBitArrayBytes)
	}

	absent := 0
	for _, k := range keyRange(1000) {
		if !f.MayContain(k) {
			absent++
		}
	}
	if absent < 900 {
		t.Fatalf("only %d/1000 absent keys were rejected; the filter is saturated", absent)
	}
}

func TestDecodeRoundTripsTheHeader(t *testing.T) {
	f := build(t, 16, keyRange(1000))
	if f.BitsPerKey() != 16 {
		t.Errorf("BitsPerKey = %d, want 16", f.BitsPerKey())
	}
	if want := OptimalProbes(16); f.Probes() != want {
		t.Errorf("Probes = %d, want %d", f.Probes(), want)
	}
	if want := (1000*16 + 7) / 8; f.SizeBytes() != want {
		t.Errorf("SizeBytes = %d, want %d", f.SizeBytes(), want)
	}
}

func TestDecodeRejectsCorruption(t *testing.T) {
	b := NewBuilder(DefaultBitsPerKey)
	for _, k := range keyRange(200) {
		b.Add(k)
	}
	encoded := b.Finish()

	t.Run("every byte is covered by the checksum", func(t *testing.T) {
		// Mutation check: this sweep is what proves the crc actually spans
		// the header and the bit array rather than only one of them.
		for i := range encoded {
			damaged := append([]byte(nil), encoded...)
			damaged[i] ^= 0x01
			if _, err := Decode(damaged); !errors.Is(err, ErrCorruptFilter) {
				t.Fatalf("flipping byte %d of %d produced err=%v, want ErrCorruptFilter", i, len(encoded), err)
			}
		}
	})

	t.Run("truncation", func(t *testing.T) {
		for cut := 0; cut < len(encoded); cut++ {
			if _, err := Decode(encoded[:cut]); !errors.Is(err, ErrCorruptFilter) {
				t.Fatalf("truncating to %d bytes produced err=%v, want ErrCorruptFilter", cut, err)
			}
		}
	})

	t.Run("a bit_array_len that disagrees with the block length", func(t *testing.T) {
		// Consistent bytes, inconsistent framing: the length field is
		// rewritten and the checksum recomputed over it, so only the
		// explicit cross-check catches this. Without it the reader would
		// slice past the header on a length it never validated.
		damaged := append([]byte(nil), encoded...)
		binary.LittleEndian.PutUint32(damaged[8:12], 7)
		binary.LittleEndian.PutUint32(damaged[len(damaged)-4:], crc32.Checksum(damaged[:len(damaged)-4], crcTable))
		if _, err := Decode(damaged); !errors.Is(err, ErrCorruptFilter) {
			t.Fatalf("err=%v, want ErrCorruptFilter for a bit_array_len that does not match the block", err)
		}
	})
}

func TestNilFilterAnswersMaybeRatherThanAbsent(t *testing.T) {
	// "No filter loaded" is the absence of evidence, not evidence of
	// absence. If a nil filter answered false the read path would skip the
	// block and report a key it holds as missing -- a false negative, which
	// is data loss.
	var f *Filter
	if !f.MayContain([]byte("anything")) {
		t.Fatal("a nil filter reported a key definitively absent")
	}
}
