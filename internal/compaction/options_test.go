package compaction

import (
	"fmt"
	"math"
	"testing"

	"github.com/AbishekRaj2007/Strata/internal/manifest"
)

// buildVersion assembles a version from a per-level description, going
// through a VersionEdit so the result is one Apply could actually have
// produced -- including its sorting and its invariant check.
func buildVersion(t *testing.T, levels map[int][]*manifest.FileMetadata) *manifest.Version {
	t.Helper()

	var edit manifest.VersionEdit
	for level, files := range levels {
		for _, f := range files {
			edit.AddFile(level, f)
		}
	}

	v, err := manifest.NewVersion().Apply(&edit)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if err := v.CheckInvariants(); err != nil {
		t.Fatalf("the test built an impossible version: %v", err)
	}
	return v
}

// file is shorthand for one table covering [smallest, largest].
func file(number uint64, smallest, largest string, size uint64) *manifest.FileMetadata {
	return &manifest.FileMetadata{
		Number:      number,
		Size:        size,
		Smallest:    []byte(smallest),
		Largest:     []byte(largest),
		SmallestSeq: number,
		LargestSeq:  number,
	}
}

func TestTargetBytesGrowsGeometrically(t *testing.T) {
	o := Options{BaseLevelBytes: 1000, LevelMultiplier: 10}

	for level, want := range map[int]uint64{0: 0, 1: 1000, 2: 10000, 3: 100000} {
		if got := o.TargetBytes(level); got != want {
			t.Errorf("TargetBytes(%d) = %d, want %d", level, got, want)
		}
	}
}

func TestTargetBytesUsesDefaults(t *testing.T) {
	if got := (Options{}).TargetBytes(1); got != DefaultBaseLevelBytes {
		t.Errorf("TargetBytes(1) = %d, want the default %d", got, DefaultBaseLevelBytes)
	}
	want := uint64(DefaultBaseLevelBytes) * DefaultLevelMultiplier
	if got := (Options{}).TargetBytes(2); got != want {
		t.Errorf("TargetBytes(2) = %d, want %d", got, want)
	}
}

func TestScore(t *testing.T) {
	o := Options{L0Trigger: 4, BaseLevelBytes: 1000, LevelMultiplier: 10}

	tests := []struct {
		name   string
		levels map[int][]*manifest.FileMetadata
		level  int
		want   float64
	}{
		{
			name:   "empty level scores zero",
			levels: nil,
			level:  0,
			want:   0,
		},
		{
			name: "L0 counts files, not bytes",
			levels: map[int][]*manifest.FileMetadata{
				0: {file(1, "a", "z", 1), file(2, "a", "z", 1)},
			},
			level: 0,
			want:  0.5,
		},
		{
			name: "L0 at the trigger scores exactly one",
			levels: map[int][]*manifest.FileMetadata{
				0: {file(1, "a", "z", 1), file(2, "a", "z", 1), file(3, "a", "z", 1), file(4, "a", "z", 1)},
			},
			level: 0,
			want:  1,
		},
		{
			name: "a tiny L0 file still counts, because a reader still reads it",
			levels: map[int][]*manifest.FileMetadata{
				0: {file(1, "a", "z", 1<<30), file(2, "a", "z", 1)},
			},
			level: 0,
			want:  0.5,
		},
		{
			name: "L1 counts bytes, not files",
			levels: map[int][]*manifest.FileMetadata{
				1: {file(1, "a", "b", 400), file(2, "c", "d", 400)},
			},
			level: 1,
			want:  0.8,
		},
		{
			name: "L2 is scored against its own larger target",
			levels: map[int][]*manifest.FileMetadata{
				2: {file(1, "a", "b", 5000)},
			},
			level: 2,
			want:  0.5,
		},
		{
			name: "an over-budget level scores above one",
			levels: map[int][]*manifest.FileMetadata{
				1: {file(1, "a", "b", 3000)},
			},
			level: 1,
			want:  3,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v := buildVersion(t, tc.levels)
			if got := o.Score(v, tc.level); math.Abs(got-tc.want) > 1e-9 {
				t.Errorf("Score(level %d) = %v, want %v", tc.level, got, tc.want)
			}
		})
	}
}

// A level's score must not depend on how its bytes are distributed across
// files, or the picker would chase file counts at levels where file count is
// not what costs anything.
func TestScoreBelowL0IgnoresFileCount(t *testing.T) {
	o := Options{BaseLevelBytes: 1000, LevelMultiplier: 10}

	oneFile := buildVersion(t, map[int][]*manifest.FileMetadata{
		1: {file(1, "a", "z", 900)},
	})

	var many []*manifest.FileMetadata
	for i := 0; i < 9; i++ {
		many = append(many, file(uint64(i+1), fmt.Sprintf("%02d", i*2), fmt.Sprintf("%02d", i*2+1), 100))
	}
	manyFiles := buildVersion(t, map[int][]*manifest.FileMetadata{1: many})

	if a, b := o.Score(oneFile, 1), o.Score(manyFiles, 1); math.Abs(a-b) > 1e-9 {
		t.Errorf("score changed with file count: %v then %v", a, b)
	}
}
