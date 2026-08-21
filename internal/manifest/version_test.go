package manifest

import (
	"errors"
	"strings"
	"testing"
)

// withFiles builds a version directly, bypassing Apply, so invariant tests can
// construct the violations Apply would refuse to produce.
func withFiles(levels map[int][]*FileMetadata) *Version {
	v := NewVersion()
	for level, files := range levels {
		v.levels[level] = files
	}
	return v
}

func TestEmptyVersion(t *testing.T) {
	v := NewVersion()
	if v.TotalFiles() != 0 {
		t.Errorf("TotalFiles = %d, want 0", v.TotalFiles())
	}
	if err := v.CheckInvariants(); err != nil {
		t.Errorf("empty version violates invariants: %v", err)
	}
	if _, ok := v.Candidate(1, []byte("k")); ok {
		t.Error("empty version returned a candidate")
	}
}

func TestLevelBytesAndCounts(t *testing.T) {
	a, b := meta(1, "a", "c"), meta(2, "e", "g")
	a.Size, b.Size = 100, 250
	v := withFiles(map[int][]*FileMetadata{1: {a, b}})

	if got := v.LevelBytes(1); got != 350 {
		t.Errorf("LevelBytes(1) = %d, want 350", got)
	}
	if got := v.NumFiles(1); got != 2 {
		t.Errorf("NumFiles(1) = %d, want 2", got)
	}
	if got := v.TotalFiles(); got != 2 {
		t.Errorf("TotalFiles = %d, want 2", got)
	}
	if got := v.LevelBytes(2); got != 0 {
		t.Errorf("LevelBytes(2) = %d, want 0", got)
	}
}

// TestCandidateFindsTheSingleFile covers the property that makes reads scale:
// below L0 at most one file per level can hold a key, and binary search finds
// it without touching the others.
func TestCandidateFindsTheSingleFile(t *testing.T) {
	v := withFiles(map[int][]*FileMetadata{
		2: {meta(1, "a", "c"), meta(2, "f", "h"), meta(3, "m", "p")},
	})

	tests := []struct {
		key    string
		want   uint64
		wantOk bool
	}{
		{"a", 1, true},  // first key of the first file
		{"c", 1, true},  // last key of the first file
		{"b", 1, true},  // interior
		{"d", 0, false}, // in the gap between files
		{"f", 2, true},  // start of the middle file
		{"h", 2, true},  // end of the middle file
		{"i", 0, false}, // gap
		{"p", 3, true},  // end of the last file
		{"q", 0, false}, // past every file
		{"", 0, false},  // below every file
	}

	for _, tc := range tests {
		got, ok := v.Candidate(2, []byte(tc.key))
		if ok != tc.wantOk {
			t.Errorf("Candidate(%q) ok = %v, want %v", tc.key, ok, tc.wantOk)
			continue
		}
		if ok && got.Number != tc.want {
			t.Errorf("Candidate(%q) = file %d, want %d", tc.key, got.Number, tc.want)
		}
	}
}

func TestCandidatePanicsOnL0(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("Candidate(0, ...) did not panic; L0 files overlap and have no single candidate")
		}
	}()
	NewVersion().Candidate(0, []byte("k"))
}

func TestOverlappingBelowL0(t *testing.T) {
	v := withFiles(map[int][]*FileMetadata{
		1: {meta(1, "a", "c"), meta(2, "f", "h"), meta(3, "m", "p")},
	})

	tests := []struct {
		name             string
		smallest, larges string
		want             []uint64
	}{
		{"spanning two files", "b", "g", []uint64{1, 2}},
		{"spanning all", "a", "z", []uint64{1, 2, 3}},
		{"inside a gap", "d", "e", nil},
		{"touching a lower bound", "c", "c", []uint64{1}},
		{"touching an upper bound", "h", "m", []uint64{2, 3}},
		{"entirely below", "", "", nil},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := v.Overlapping(1, []byte(tc.smallest), []byte(tc.larges))
			if len(got) != len(tc.want) {
				t.Fatalf("Overlapping = %d files, want %d", len(got), len(tc.want))
			}
			for i := range got {
				if got[i].Number != tc.want[i] {
					t.Errorf("file %d = %d, want %d", i, got[i].Number, tc.want[i])
				}
			}
		})
	}
}

// TestOverlappingAtL0ScansEverything checks the asymmetry: L0 files come from
// independent memtable dumps, so no ordering rules any of them out.
func TestOverlappingAtL0ScansEverything(t *testing.T) {
	v := withFiles(map[int][]*FileMetadata{
		0: {meta(3, "a", "z"), meta(2, "b", "c"), meta(1, "y", "z")},
	})

	got := v.Overlapping(0, []byte("b"), []byte("b"))
	if len(got) != 2 {
		t.Fatalf("Overlapping = %d files, want 2 (the wide file and the narrow one)", len(got))
	}
	if got[0].Number != 3 || got[1].Number != 2 {
		t.Errorf("got files %d,%d; want 3,2 in newest-first order", got[0].Number, got[1].Number)
	}
}

func TestCheckInvariants(t *testing.T) {
	tests := []struct {
		name    string
		version *Version
		wantErr string
	}{
		{
			name:    "well formed",
			version: withFiles(map[int][]*FileMetadata{0: {meta(9, "a", "z"), meta(8, "b", "c")}, 1: {meta(1, "a", "c"), meta(2, "e", "g")}}),
		},
		{
			name:    "L0 not newest first",
			version: withFiles(map[int][]*FileMetadata{0: {meta(1, "a", "b"), meta(2, "c", "d")}}),
			wantErr: "not ordered by file number descending",
		},
		{
			name:    "level 1 unsorted",
			version: withFiles(map[int][]*FileMetadata{1: {meta(1, "m", "p"), meta(2, "a", "c")}}),
			wantErr: "not sorted by smallest key",
		},
		{
			name:    "level 1 overlapping",
			version: withFiles(map[int][]*FileMetadata{1: {meta(1, "a", "f"), meta(2, "e", "g")}}),
			wantErr: "overlap",
		},
		{
			name:    "level 1 touching at one key",
			version: withFiles(map[int][]*FileMetadata{1: {meta(1, "a", "f"), meta(2, "f", "g")}}),
			wantErr: "overlap",
		},
		{
			name:    "same file at two levels",
			version: withFiles(map[int][]*FileMetadata{1: {meta(5, "a", "c")}, 2: {meta(5, "m", "p")}}),
			wantErr: "appears at both level",
		},
		{
			name:    "inconsistent file metadata",
			version: withFiles(map[int][]*FileMetadata{1: {{Number: 1, Smallest: []byte("z"), Largest: []byte("a")}}}),
			wantErr: "above largest",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.version.CheckInvariants()
			if tc.wantErr == "" {
				if err != nil {
					t.Errorf("CheckInvariants = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("CheckInvariants = nil, want an error containing %q", tc.wantErr)
			}
			if !errors.Is(err, ErrCorrupt) {
				t.Errorf("error %v does not wrap ErrCorrupt", err)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err, tc.wantErr)
			}
		})
	}
}
