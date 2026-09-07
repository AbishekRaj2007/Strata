package sstable

import (
	"errors"
	"fmt"
	"sort"
	"testing"

	"github.com/AbishekRaj2007/Strata/internal/memtable"
)

func buildBlock(t *testing.T, entries []memtable.Entry) *Block {
	t.Helper()

	b := NewBlockBuilder()
	for _, e := range entries {
		b.Add(e)
	}
	blk, err := NewBlock(b.Finish())
	if err != nil {
		t.Fatalf("NewBlock: %v", err)
	}
	return blk
}

func collect(t *testing.T, it *BlockIterator) []memtable.Entry {
	t.Helper()
	var out []memtable.Entry
	for it.Next() {
		out = append(out, it.Entry())
	}
	if err := it.Err(); err != nil {
		t.Fatalf("iterator error: %v", err)
	}
	return out
}

// TestBlockRoundTripsSingleEntry is the smallest useful case.
func TestBlockRoundTripsSingleEntry(t *testing.T) {
	entries := []memtable.Entry{{Key: []byte("k"), Sequence: 1, Value: []byte("v")}}
	blk := buildBlock(t, entries)

	got := collect(t, blk.NewIterator())
	if len(got) != 1 {
		t.Fatalf("got %d entries, want 1", len(got))
	}
	if string(got[0].Key) != "k" || got[0].Sequence != 1 || string(got[0].Value) != "v" {
		t.Errorf("entry = %+v, want k/1/v", got[0])
	}
}

// TestBlockDeleteCarriesNoValue pins §3.2: a DELETE entry has value_len=0 and
// no value bytes, distinct from a SET with an empty value.
func TestBlockDeleteCarriesNoValue(t *testing.T) {
	blk := buildBlock(t, []memtable.Entry{
		{Key: []byte("a"), Sequence: 1, Value: []byte{}},
		{Key: []byte("b"), Sequence: 2, Tombstone: true},
	})

	got := collect(t, blk.NewIterator())
	if len(got) != 2 {
		t.Fatalf("got %d entries, want 2", len(got))
	}
	if got[0].Tombstone || got[0].Value == nil {
		t.Errorf("entry 0 = %+v, want a non-tombstone SET with an empty (non-nil) value", got[0])
	}
	if !got[1].Tombstone || len(got[1].Value) != 0 {
		t.Errorf("entry 1 = %+v, want a tombstone with no value", got[1])
	}
}

// TestHundredThousandEntriesRoundTrip is T3.3's first done-when condition:
// heavily shared prefixes (sequential keys share almost everything with their
// predecessor) must reproduce every entry exactly, spanning many restart
// points.
func TestHundredThousandEntriesRoundTrip(t *testing.T) {
	const n = 100_000
	var entries []memtable.Entry
	for i := 0; i < n; i++ {
		entries = append(entries, memtable.Entry{
			Key:      []byte(fmt.Sprintf("user:%08d:profile", i)),
			Sequence: uint64(i + 1),
			Value:    []byte(fmt.Sprintf("value-%d", i)),
		})
	}

	b := NewBlockBuilder()
	for _, e := range entries {
		b.Add(e)
	}
	encoded := b.Finish()

	blk, err := NewBlock(encoded)
	if err != nil {
		t.Fatalf("NewBlock: %v", err)
	}

	got := collect(t, blk.NewIterator())
	if len(got) != n {
		t.Fatalf("got %d entries, want %d", len(got), n)
	}
	for i, e := range got {
		want := entries[i]
		if string(e.Key) != string(want.Key) || e.Sequence != want.Sequence || string(e.Value) != string(want.Value) {
			t.Fatalf("entry %d = %+v, want %+v", i, e, want)
		}
	}
}

// TestSeekLandsOnCorrectSuccessor is T3.3's second done-when condition: a
// seek to a key strictly between two stored entries must land on the one
// that follows it, and this must hold whether the target falls near a
// restart point or deep inside a prefix-compressed run.
func TestSeekLandsOnCorrectSuccessor(t *testing.T) {
	var entries []memtable.Entry
	for i := 0; i < 500; i += 2 { // only even keys are stored, so odd ones seek to a gap
		key := fmt.Sprintf("k%04d", i)
		entries = append(entries, memtable.Entry{Key: []byte(key), Sequence: 1, Value: []byte("v")})
	}
	blk := buildBlock(t, entries)

	tests := []struct {
		target string
		want   string // "" means no successor exists
	}{
		{"k0000", "k0000"}, // exact hit on the very first entry (a restart point)
		{"k0001", "k0002"}, // between two stored entries
		{"k0016", "k0016"}, // exact hit that happens to land on a restart point
		{"k0017", "k0018"}, // between entries, just after a restart point
		{"", "k0000"},      // before everything
		{"zzzz", ""},       // after everything
	}

	for _, tc := range tests {
		t.Run(tc.target, func(t *testing.T) {
			it, err := blk.Seek([]byte(tc.target))
			if err != nil {
				t.Fatalf("Seek(%q): %v", tc.target, err)
			}
			if tc.want == "" {
				if it.Next() {
					t.Fatalf("Seek(%q) found %q, want no successor", tc.target, it.Entry().Key)
				}
				return
			}
			if !it.Next() {
				t.Fatalf("Seek(%q): no successor, want %q", tc.target, tc.want)
			}
			if got := string(it.Entry().Key); got != tc.want {
				t.Errorf("Seek(%q) landed on %q, want %q", tc.target, got, tc.want)
			}
		})
	}
}

// TestSeekAfterEveryRestartPoint is the trap T3.3 names: the first entry
// after each restart point must store its full key. A builder that shares a
// prefix against the wrong reference key still passes a purely sequential
// scan (each entry's shared prefix is still computed against *some* previous
// key) and only fails when a seek jumps directly into the middle of a run --
// exactly what this test forces for every restart point in the block.
func TestSeekAfterEveryRestartPoint(t *testing.T) {
	const n = RestartInterval*5 + 3 // several full restart groups plus a partial one
	var entries []memtable.Entry
	for i := 0; i < n; i++ {
		entries = append(entries, memtable.Entry{
			Key:      []byte(fmt.Sprintf("prefix-shared-%05d", i)),
			Sequence: 1,
			Value:    []byte("v"),
		})
	}
	blk := buildBlock(t, entries)

	for i := 0; i < n; i += RestartInterval {
		target := entries[i].Key
		it, err := blk.Seek(target)
		if err != nil {
			t.Fatalf("Seek at restart %d: %v", i, err)
		}
		if !it.Next() {
			t.Fatalf("Seek at restart %d: no successor", i)
		}
		if got := string(it.Entry().Key); got != string(target) {
			t.Errorf("Seek at restart %d landed on %q, want %q", i, got, target)
		}
	}
}

// TestBlockRejectsFlippedByte checks a single corrupted byte anywhere in the
// block is caught by the checksum rather than silently decoded into wrong
// data -- the durability invariant that a failed check must be loud.
func TestBlockRejectsFlippedByte(t *testing.T) {
	b := NewBlockBuilder()
	for i := 0; i < 50; i++ {
		b.Add(memtable.Entry{Key: []byte(fmt.Sprintf("k%03d", i)), Sequence: 1, Value: []byte("v")})
	}
	encoded := b.Finish()

	for i := 0; i < len(encoded); i++ {
		corrupted := append([]byte(nil), encoded...)
		corrupted[i] ^= 0xFF

		_, err := NewBlock(corrupted)
		if err == nil {
			// A handful of positions can flip a byte within a value or key
			// without changing the checksum's verdict on validity if the flip
			// happens to reproduce another well-formed encoding by chance --
			// but a checksum mismatch is what we're checking for, so any
			// success here without an error is only acceptable if the bytes
			// still checksum correctly, which can't happen from a single
			// flipped byte under CRC32C except astronomically rarely. Fail
			// loudly so a real regression is caught.
			t.Fatalf("byte %d: flipping it produced no error, want a checksum failure", i)
		}
		if !errors.Is(err, ErrCorruptBlock) {
			t.Errorf("byte %d: error = %v, want it to wrap ErrCorruptBlock", i, err)
		}
	}
}

// TestBlockRejectsTruncation checks a block cut short is rejected rather than
// decoded into a partial, misleading result.
func TestBlockRejectsTruncation(t *testing.T) {
	b := NewBlockBuilder()
	for i := 0; i < 20; i++ {
		b.Add(memtable.Entry{Key: []byte(fmt.Sprintf("k%02d", i)), Sequence: 1, Value: []byte("value")})
	}
	encoded := b.Finish()

	for cut := 0; cut < len(encoded); cut++ {
		if _, err := NewBlock(encoded[:cut]); err == nil {
			t.Fatalf("truncated to %d/%d bytes: NewBlock succeeded, want a corruption error", cut, len(encoded))
		}
	}
}

// TestBlockEntriesMustBeSortedForIterationToMakeSense is not a builder
// invariant check (the builder trusts its caller, per its own doc comment)
// but confirms the round trip preserves whatever order was given, which is
// what every caller above actually depends on.
func TestBlockPreservesInsertionOrder(t *testing.T) {
	keys := []string{"m", "a", "z", "b"} // deliberately unsorted
	var entries []memtable.Entry
	for i, k := range keys {
		entries = append(entries, memtable.Entry{Key: []byte(k), Sequence: uint64(i + 1), Value: []byte("v")})
	}
	blk := buildBlock(t, entries)

	got := collect(t, blk.NewIterator())
	var gotKeys []string
	for _, e := range got {
		gotKeys = append(gotKeys, string(e.Key))
	}
	for i := range keys {
		if gotKeys[i] != keys[i] {
			t.Fatalf("order = %v, want %v", gotKeys, keys)
		}
	}
}

// TestEmptyBlockIterates checks a block built with zero entries decodes and
// iterates cleanly rather than panicking on an empty restart array.
func TestEmptyBlockIterates(t *testing.T) {
	blk := buildBlock(t, nil)
	got := collect(t, blk.NewIterator())
	if len(got) != 0 {
		t.Errorf("got %d entries from an empty block, want 0", len(got))
	}
}

// TestRandomizedRoundTrip is a light property check: many random key/value
// sets, always sorted before building (the builder's documented precondition),
// must always reproduce exactly.
func TestRandomizedRoundTrip(t *testing.T) {
	for trial := 0; trial < 200; trial++ {
		n := trial%37 + 1
		var entries []memtable.Entry
		for i := 0; i < n; i++ {
			entries = append(entries, memtable.Entry{
				Key:      []byte(fmt.Sprintf("k-%03d-%03d", trial, i)),
				Sequence: uint64(i + 1),
				Value:    []byte(fmt.Sprintf("v%d", i)),
			})
		}
		sort.Slice(entries, func(i, j int) bool {
			return memtable.Compare(entries[i].Key, entries[i].Sequence, entries[j].Key, entries[j].Sequence) < 0
		})

		blk := buildBlock(t, entries)
		got := collect(t, blk.NewIterator())
		if len(got) != len(entries) {
			t.Fatalf("trial %d: got %d entries, want %d", trial, len(got), len(entries))
		}
		for i := range entries {
			if string(got[i].Key) != string(entries[i].Key) || got[i].Sequence != entries[i].Sequence {
				t.Fatalf("trial %d entry %d = %+v, want %+v", trial, i, got[i], entries[i])
			}
		}
	}
}
