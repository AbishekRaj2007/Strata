package manifest

import (
	"errors"
	"fmt"
	"testing"
)

func mustApply(t *testing.T, v *Version, e *VersionEdit) *Version {
	t.Helper()
	next, err := v.Apply(e)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	return next
}

func TestApplyAddsFiles(t *testing.T) {
	var e VersionEdit
	e.AddFile(0, meta(1, "a", "c"))
	e.AddFile(1, meta(2, "d", "f"))
	e.SetLastSequence(42)
	e.SetLogNumber(7)
	e.SetNextFileNumber(9)

	v := mustApply(t, NewVersion(), &e)

	if v.NumFiles(0) != 1 || v.NumFiles(1) != 1 {
		t.Errorf("levels = %d/%d, want 1/1", v.NumFiles(0), v.NumFiles(1))
	}
	if v.LastSequence() != 42 || v.LogNumber() != 7 || v.NextFileNumber() != 9 {
		t.Errorf("counters = %d/%d/%d, want 42/7/9", v.LastSequence(), v.LogNumber(), v.NextFileNumber())
	}
}

// TestApplyDoesNotMutateTheReceiver is the property the whole concurrency
// story rests on. A reader holding a version across a compaction must see
// exactly what it saw when it took the pointer.
func TestApplyDoesNotMutateTheReceiver(t *testing.T) {
	var add VersionEdit
	add.AddFile(1, meta(1, "a", "c"))
	base := mustApply(t, NewVersion(), &add)

	before := base.Files(1)
	beforeLen := len(before)

	var more VersionEdit
	more.AddFile(1, meta(2, "m", "p"))
	next := mustApply(t, base, &more)

	if len(base.Files(1)) != beforeLen {
		t.Errorf("base level 1 grew to %d files, want %d", len(base.Files(1)), beforeLen)
	}
	if len(next.Files(1)) != beforeLen+1 {
		t.Errorf("new version level 1 = %d files, want %d", len(next.Files(1)), beforeLen+1)
	}
	if &before[0] != &base.Files(1)[0] {
		t.Error("base version's slice was reallocated underneath a holder")
	}

	var del VersionEdit
	del.DeleteFile(1, 1)
	if _, err := next.Apply(&del); err != nil {
		t.Fatalf("Apply delete: %v", err)
	}
	if len(base.Files(1)) != beforeLen {
		t.Errorf("deleting from a derived version shrank the base to %d files", len(base.Files(1)))
	}
}

// TestAddThenDeleteLeavesFileAbsent is T4.1's trap stated directly. Replay
// must be order-sensitive: whatever way state is accumulated, an ADD followed
// by a DELETE of the same file leaves it absent.
func TestAddThenDeleteLeavesFileAbsent(t *testing.T) {
	var add VersionEdit
	add.AddFile(1, meta(1, "a", "c"))
	v := mustApply(t, NewVersion(), &add)

	var del VersionEdit
	del.DeleteFile(1, 1)
	v = mustApply(t, v, &del)

	if v.NumFiles(1) != 0 {
		t.Errorf("level 1 holds %d files, want 0 after ADD then DELETE", v.NumFiles(1))
	}
}

// TestDeleteThenAddInOneEditKeepsTheFile is the same trap from the other side:
// within a single edit, deletions apply first, so a compaction rewriting a
// file into the level it came from keeps the new file rather than deleting it.
func TestDeleteThenAddInOneEditKeepsTheFile(t *testing.T) {
	var add VersionEdit
	add.AddFile(1, meta(1, "a", "c"))
	v := mustApply(t, NewVersion(), &add)

	var rewrite VersionEdit
	rewrite.AddFile(1, meta(1, "a", "d"))
	rewrite.DeleteFile(1, 1)

	v = mustApply(t, v, &rewrite)

	if v.NumFiles(1) != 1 {
		t.Fatalf("level 1 holds %d files, want the rewritten one", v.NumFiles(1))
	}
	if got := string(v.Files(1)[0].Largest); got != "d" {
		t.Errorf("largest key = %q, want %q: the addition was lost to the deletion", got, "d")
	}
}

func TestDeleteOfAbsentFileIsCorruption(t *testing.T) {
	var del VersionEdit
	del.DeleteFile(1, 99)

	if _, err := NewVersion().Apply(&del); !errors.Is(err, ErrCorrupt) {
		t.Errorf("Apply = %v, want an ErrCorrupt (§4.2)", err)
	}
}

func TestDeleteFromWrongLevelIsCorruption(t *testing.T) {
	var add VersionEdit
	add.AddFile(1, meta(1, "a", "c"))
	v := mustApply(t, NewVersion(), &add)

	var del VersionEdit
	del.DeleteFile(2, 1)

	if _, err := v.Apply(&del); !errors.Is(err, ErrCorrupt) {
		t.Errorf("Apply = %v, want an ErrCorrupt", err)
	}
}

func TestApplyRejectsInvariantViolations(t *testing.T) {
	var overlapping VersionEdit
	overlapping.AddFile(1, meta(1, "a", "f"))
	overlapping.AddFile(1, meta(2, "e", "g"))

	if _, err := NewVersion().Apply(&overlapping); !errors.Is(err, ErrCorrupt) {
		t.Errorf("Apply of overlapping L1 files = %v, want an ErrCorrupt", err)
	}

	var outOfRange VersionEdit
	outOfRange.AddFile(NumLevels, meta(1, "a", "b"))
	if _, err := NewVersion().Apply(&outOfRange); !errors.Is(err, ErrCorrupt) {
		t.Errorf("Apply of an out-of-range level = %v, want an ErrCorrupt", err)
	}
}

// TestL0OrderedByFileNumberDescending covers T4.3's trap: L0 ordering is by
// file number, never by key range. Newest wins, always.
func TestL0OrderedByFileNumberDescending(t *testing.T) {
	var e VersionEdit
	e.AddFile(0, meta(3, "m", "z"))
	e.AddFile(0, meta(7, "a", "b"))
	e.AddFile(0, meta(5, "c", "d"))

	v := mustApply(t, NewVersion(), &e)

	want := []uint64{7, 5, 3}
	for i, f := range v.Files(0) {
		if f.Number != want[i] {
			t.Errorf("L0[%d] = file %d, want %d", i, f.Number, want[i])
		}
	}
}

func TestLevelsBelowL0SortedBySmallestKey(t *testing.T) {
	var e VersionEdit
	e.AddFile(2, meta(1, "m", "p"))
	e.AddFile(2, meta(2, "a", "c"))
	e.AddFile(2, meta(3, "f", "h"))

	v := mustApply(t, NewVersion(), &e)

	want := []string{"a", "f", "m"}
	for i, f := range v.Files(2) {
		if string(f.Smallest) != want[i] {
			t.Errorf("L2[%d] smallest = %q, want %q", i, f.Smallest, want[i])
		}
	}
}

func TestCountersCarryForwardWhenUnset(t *testing.T) {
	var first VersionEdit
	first.SetLastSequence(10)
	first.SetLogNumber(3)
	v := mustApply(t, NewVersion(), &first)

	var second VersionEdit
	second.AddFile(0, meta(1, "a", "b"))
	v = mustApply(t, v, &second)

	if v.LastSequence() != 10 || v.LogNumber() != 3 {
		t.Errorf("counters = %d/%d, want them carried forward as 10/3", v.LastSequence(), v.LogNumber())
	}
}

// TestReplayOf200FilesAcrossFourLevels is T4.1's done-when reconstruction,
// exercised through Apply. The manifest log that would carry these edits needs
// the framing reader from T2.3; this covers everything above that line.
func TestReplayOf200FilesAcrossFourLevels(t *testing.T) {
	const (
		levels       = 4
		filesPerEdit = 5
		total        = 200
	)

	v := NewVersion()
	number := uint64(1)

	// Level 0 tolerates overlap; levels below must not, so each file at a
	// level gets a disjoint key range.
	perLevel := total / levels
	for level := 0; level < levels; level++ {
		for i := 0; i < perLevel; i += filesPerEdit {
			var e VersionEdit
			for j := 0; j < filesPerEdit; j++ {
				idx := i + j
				m := &FileMetadata{
					Number:      number,
					Size:        4096,
					Smallest:    []byte(fmt.Sprintf("L%d-%05d-a", level, idx)),
					Largest:     []byte(fmt.Sprintf("L%d-%05d-z", level, idx)),
					SmallestSeq: number,
					LargestSeq:  number + 1,
				}
				e.AddFile(level, m)
				number++
			}
			e.SetNextFileNumber(number)
			v = mustApply(t, v, &e)
		}
	}

	if v.TotalFiles() != total {
		t.Fatalf("TotalFiles = %d, want %d", v.TotalFiles(), total)
	}
	for level := 0; level < levels; level++ {
		if v.NumFiles(level) != perLevel {
			t.Errorf("level %d holds %d files, want %d", level, v.NumFiles(level), perLevel)
		}
	}
	if err := v.CheckInvariants(); err != nil {
		t.Fatalf("reconstructed version violates invariants: %v", err)
	}

	// Every file must still be findable by the read path's own search.
	for level := 1; level < levels; level++ {
		for i := 0; i < perLevel; i++ {
			key := []byte(fmt.Sprintf("L%d-%05d-m", level, i))
			if _, ok := v.Candidate(level, key); !ok {
				t.Fatalf("level %d lost the file covering %q", level, key)
			}
		}
	}

	// Now delete half of them, one edit at a time, and confirm the version
	// tracks exactly.
	for level := 0; level < levels; level++ {
		files := append([]*FileMetadata(nil), v.Files(level)...)
		for i := 0; i < len(files)/2; i++ {
			var e VersionEdit
			e.DeleteFile(level, files[i].Number)
			v = mustApply(t, v, &e)
		}
	}

	if got, want := v.TotalFiles(), total/2; got != want {
		t.Errorf("TotalFiles after deleting half = %d, want %d", got, want)
	}
	if err := v.CheckInvariants(); err != nil {
		t.Errorf("version violates invariants after deletions: %v", err)
	}
}
