package manifest

import (
	"errors"
	"testing"
)

func meta(number uint64, smallest, largest string) *FileMetadata {
	return &FileMetadata{
		Number:      number,
		Size:        1024,
		Smallest:    []byte(smallest),
		Largest:     []byte(largest),
		SmallestSeq: 1,
		LargestSeq:  100,
	}
}

func TestFileName(t *testing.T) {
	tests := []struct {
		number uint64
		want   string
	}{
		{1, "000001.sst"},
		{999999, "999999.sst"},
		// docs/format.md §1: numbers widen beyond six digits rather than wrap.
		{1000000, "1000000.sst"},
	}
	for _, tc := range tests {
		if got := (&FileMetadata{Number: tc.number}).Name(); got != tc.want {
			t.Errorf("Name(%d) = %q, want %q", tc.number, got, tc.want)
		}
	}
}

func TestContains(t *testing.T) {
	f := meta(1, "d", "m")

	tests := []struct {
		key  string
		want bool
	}{
		{"d", true},  // inclusive lower bound
		{"m", true},  // inclusive upper bound
		{"g", true},  // interior
		{"c", false}, // below
		{"n", false}, // above
		{"", false},  // empty key sorts below any non-empty bound
		{"da", true},
		{"ma", false},
	}
	for _, tc := range tests {
		if got := f.Contains([]byte(tc.key)); got != tc.want {
			t.Errorf("Contains(%q) = %v, want %v", tc.key, got, tc.want)
		}
	}
}

// TestOverlaps covers the boundaries compaction depends on. Touching at a
// single key counts as overlap: two files sharing one key at a level that
// guarantees non-overlap is exactly the corruption this predicate exists to
// prevent.
func TestOverlaps(t *testing.T) {
	f := meta(1, "d", "m")

	tests := []struct {
		name             string
		smallest, larges string
		want             bool
	}{
		{"strictly below", "a", "c", false},
		{"strictly above", "n", "z", false},
		{"touching at lower bound", "a", "d", true},
		{"touching at upper bound", "m", "z", true},
		{"one key below the lower bound", "a", "c", false},
		{"contained entirely", "e", "f", true},
		{"containing entirely", "a", "z", true},
		{"identical range", "d", "m", true},
		{"single key inside", "g", "g", true},
		{"single key outside", "c", "c", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := f.Overlaps([]byte(tc.smallest), []byte(tc.larges)); got != tc.want {
				t.Errorf("Overlaps(%q,%q) = %v, want %v", tc.smallest, tc.larges, got, tc.want)
			}
		})
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		f       FileMetadata
		wantErr bool
	}{
		{"well formed", FileMetadata{Number: 1, Smallest: []byte("a"), Largest: []byte("b"), SmallestSeq: 1, LargestSeq: 2}, false},
		{"equal bounds", FileMetadata{Number: 1, Smallest: []byte("a"), Largest: []byte("a"), SmallestSeq: 1, LargestSeq: 1}, false},
		{"empty key bounds", FileMetadata{Number: 1, Smallest: nil, Largest: nil}, false},
		{"keys inverted", FileMetadata{Number: 1, Smallest: []byte("b"), Largest: []byte("a")}, true},
		{"sequences inverted", FileMetadata{Number: 1, Smallest: []byte("a"), Largest: []byte("b"), SmallestSeq: 9, LargestSeq: 2}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.f.Validate()
			if tc.wantErr {
				if !errors.Is(err, ErrCorrupt) {
					t.Errorf("Validate() = %v, want an ErrCorrupt", err)
				}
				return
			}
			if err != nil {
				t.Errorf("Validate() = %v, want nil", err)
			}
		})
	}
}
