package compaction

import (
	"fmt"
	"testing"

	"github.com/AbishekRaj2007/Strata/internal/manifest"
)

// numbers extracts file numbers for comparison, since the identity of a file
// is what these tests are about and a *FileMetadata dump is unreadable.
func numbers(files []*manifest.FileMetadata) []uint64 {
	out := make([]uint64, 0, len(files))
	for _, f := range files {
		out = append(out, f.Number)
	}
	return out
}

func sameSet(a, b []uint64) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[uint64]int, len(a))
	for _, n := range a {
		seen[n]++
	}
	for _, n := range b {
		seen[n]--
		if seen[n] < 0 {
			return false
		}
	}
	return true
}

func TestPickNothingUnderBudget(t *testing.T) {
	p := NewPicker(Options{L0Trigger: 4, BaseLevelBytes: 1000})

	v := buildVersion(t, map[int][]*manifest.FileMetadata{
		0: {file(2, "a", "b", 10), file(1, "c", "d", 10)},
		1: {file(3, "a", "z", 900)},
	})

	if c := p.Pick(v); c != nil {
		t.Fatalf("picked a compaction from a tree under budget: level %d, %v", c.Level, numbers(c.Inputs()))
	}
}

func TestPickChoosesTheWorstLevel(t *testing.T) {
	p := NewPicker(Options{L0Trigger: 4, BaseLevelBytes: 1000, LevelMultiplier: 10})

	// L0 is at 5/4 = 1.25. L1 is at 4000/1000 = 4.0 and must win.
	v := buildVersion(t, map[int][]*manifest.FileMetadata{
		0: {file(5, "m", "n", 1), file(4, "m", "n", 1), file(3, "m", "n", 1), file(2, "m", "n", 1), file(1, "m", "n", 1)},
		1: {file(10, "a", "b", 4000)},
	})

	c := p.Pick(v)
	if c == nil {
		t.Fatal("picked nothing from an over-budget tree")
	}
	if c.Level != 1 {
		t.Errorf("compacted level %d (score %v), want the worse-scoring level 1", c.Level, c.Score)
	}
}

func TestPickSkipsTheBottomLevel(t *testing.T) {
	bottom := manifest.NumLevels - 1
	p := NewPicker(Options{BaseLevelBytes: 1, LevelMultiplier: 1})

	v := buildVersion(t, map[int][]*manifest.FileMetadata{
		bottom: {file(1, "a", "z", 1<<30)},
	})

	if c := p.Pick(v); c != nil {
		t.Fatalf("picked level %d: the bottom level has nowhere to compact into", c.Level)
	}
}

func TestPickExpandsIntoOverlappingParents(t *testing.T) {
	p := NewPicker(Options{BaseLevelBytes: 100, LevelMultiplier: 10})

	v := buildVersion(t, map[int][]*manifest.FileMetadata{
		1: {file(1, "c", "f", 500)},
		2: {
			file(10, "a", "b", 10), // below the range
			file(11, "d", "e", 10), // inside it
			file(12, "f", "g", 10), // touching at the boundary
			file(13, "x", "z", 10), // above it
		},
	})

	c := p.Pick(v)
	if c == nil {
		t.Fatal("picked nothing")
	}
	if got, want := numbers(c.Parent), []uint64{11, 12}; !sameSet(got, want) {
		t.Errorf("parents = %v, want %v -- boundary-touching files overlap and must be included", got, want)
	}
}

// The trap in T6.1: each L0 file added widens the range, which can pull in
// another. One pass is not enough.
func TestPickL0ExpandsToAFixedPoint(t *testing.T) {
	p := NewPicker(Options{L0Trigger: 1})

	// Seeded at "d"-"f", the chain reaches 2 then 3 then 4, one link per
	// pass. File 5 is disjoint throughout and must stay behind.
	v := buildVersion(t, map[int][]*manifest.FileMetadata{
		0: {
			file(5, "y", "z", 10),
			file(4, "a", "b", 10),
			file(3, "b", "c", 10),
			file(2, "c", "e", 10),
			file(1, "d", "f", 10),
		},
	})

	c := p.Pick(v)
	if c == nil {
		t.Fatal("picked nothing from an over-budget L0")
	}
	if got, want := numbers(c.Base), []uint64{1, 2, 3, 4}; !sameSet(got, want) {
		t.Errorf("L0 inputs = %v, want %v: expansion stopped before the fixed point", got, want)
	}
}

func TestPickL0IgnoresTheInputCap(t *testing.T) {
	// The overlap set is mandatory for correctness, so a cap far below the
	// files' size must not shrink it.
	p := NewPicker(Options{L0Trigger: 1, MaxInputBytes: 1})

	v := buildVersion(t, map[int][]*manifest.FileMetadata{
		0: {file(2, "a", "z", 1<<20), file(1, "a", "z", 1<<20)},
	})

	c := p.Pick(v)
	if c == nil {
		t.Fatal("picked nothing")
	}
	if len(c.Base) != 2 {
		t.Errorf("L0 inputs = %v, want both files however large they are", numbers(c.Base))
	}
}

func TestPickCapsDiscretionaryExpansion(t *testing.T) {
	// Four disjoint L1 files with no parents below. Without a cap the picker
	// would take all four, since each is free to add.
	p := NewPicker(Options{BaseLevelBytes: 100, MaxInputBytes: 250})

	v := buildVersion(t, map[int][]*manifest.FileMetadata{
		1: {file(1, "a", "b", 100), file(2, "c", "d", 100), file(3, "e", "f", 100), file(4, "g", "h", 100)},
	})

	c := p.Pick(v)
	if c == nil {
		t.Fatal("picked nothing")
	}
	if got := c.TotalBytes(); got > 250 {
		t.Errorf("total input = %d bytes, want no more than the 250 cap", got)
	}
	if len(c.Base) != 2 {
		t.Errorf("base = %v, want exactly the two files that fit", numbers(c.Base))
	}
}

func TestPickStopsExpandingAtNewParents(t *testing.T) {
	p := NewPicker(Options{BaseLevelBytes: 100, LevelMultiplier: 10, MaxInputBytes: 1 << 30})

	v := buildVersion(t, map[int][]*manifest.FileMetadata{
		1: {file(1, "a", "b", 100), file(2, "c", "d", 100)},
		2: {file(10, "a", "b", 10), file(11, "c", "d", 10)},
	})

	c := p.Pick(v)
	if c == nil {
		t.Fatal("picked nothing")
	}
	if len(c.Base) != 1 {
		t.Errorf("base = %v, want just the seed: growing it would pull in another parent", numbers(c.Base))
	}
}

func TestBottomMost(t *testing.T) {
	p := NewPicker(Options{BaseLevelBytes: 100})

	t.Run("nothing below the output level", func(t *testing.T) {
		v := buildVersion(t, map[int][]*manifest.FileMetadata{
			1: {file(1, "c", "d", 500)},
		})
		c := p.Pick(v)
		if c == nil || !c.BottomMost {
			t.Fatalf("want a bottom-most compaction, got %+v", c)
		}
	})

	t.Run("a disjoint file below does not block it", func(t *testing.T) {
		p := NewPicker(Options{BaseLevelBytes: 100})
		v := buildVersion(t, map[int][]*manifest.FileMetadata{
			1: {file(1, "c", "d", 500)},
			3: {file(2, "x", "z", 10)},
		})
		c := p.Pick(v)
		if c == nil || !c.BottomMost {
			t.Fatalf("a file outside the key range cannot hide anything; got %+v", c)
		}
	})

	t.Run("an overlapping file below blocks it", func(t *testing.T) {
		p := NewPicker(Options{BaseLevelBytes: 100})
		v := buildVersion(t, map[int][]*manifest.FileMetadata{
			1: {file(1, "c", "d", 500)},
			3: {file(2, "c", "z", 10)},
		})
		c := p.Pick(v)
		if c == nil {
			t.Fatal("picked nothing")
		}
		if c.BottomMost {
			t.Error("reported bottom-most with an overlapping file at L3: a tombstone dropped here resurrects it")
		}
	})
}

// The round-robin pointer is what makes compaction sweep rather than sit. A
// picker that always returned the first file would return file 1 forever.
func TestPickSweepsTheKeyspace(t *testing.T) {
	p := NewPicker(Options{BaseLevelBytes: 100, MaxInputBytes: 150})

	var files []*manifest.FileMetadata
	for i := 0; i < 5; i++ {
		files = append(files, file(uint64(i+1), fmt.Sprintf("%02da", i), fmt.Sprintf("%02dz", i), 100))
	}
	v := buildVersion(t, map[int][]*manifest.FileMetadata{1: files})

	var seeds []uint64
	for i := 0; i < 5; i++ {
		c := p.Pick(v)
		if c == nil {
			t.Fatalf("pick %d returned nothing", i)
		}
		seeds = append(seeds, c.Base[0].Number)
	}

	if want := []uint64{1, 2, 3, 4, 5}; !equal(seeds, want) {
		t.Errorf("seeds = %v, want %v: the pointer is not advancing", seeds, want)
	}

	// And it wraps rather than stopping at the end of the level.
	if c := p.Pick(v); c == nil || c.Base[0].Number != 1 {
		t.Errorf("after the sweep the picker did not wrap to the start: %v", c)
	}
}

func equal(a, b []uint64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
