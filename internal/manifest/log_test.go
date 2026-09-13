package manifest

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// newLog creates a manifest in a fresh directory and points CURRENT at it.
func newLog(t *testing.T) (dir string, l *Log) {
	t.Helper()
	dir = t.TempDir()
	l, err := CreateLog(nil, dir, 1)
	if err != nil {
		t.Fatalf("CreateLog: %v", err)
	}
	if err := WriteCurrent(nil, dir, l.Name()); err != nil {
		t.Fatalf("WriteCurrent: %v", err)
	}
	return dir, l
}

func TestLogAppendAndRecoverRoundTrip(t *testing.T) {
	dir, l := newLog(t)

	var e VersionEdit
	e.AddFile(0, meta(1, "a", "c"))
	e.AddFile(1, meta(2, "d", "f"))
	e.SetLastSequence(42)
	e.SetLogNumber(7)
	e.SetNextFileNumber(9)

	if err := l.Append(&e); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	vs, err := Recover(nil, dir)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	v := vs.Current()

	if v.NumFiles(0) != 1 || v.NumFiles(1) != 1 {
		t.Errorf("levels = %d/%d, want 1/1", v.NumFiles(0), v.NumFiles(1))
	}
	if v.LastSequence() != 42 || v.LogNumber() != 7 || v.NextFileNumber() != 9 {
		t.Errorf("counters = %d/%d/%d, want 42/7/9", v.LastSequence(), v.LogNumber(), v.NextFileNumber())
	}
}

// TestRecoverReconstructs200FilesAcrossFourLevels is T4.1's first done-when
// condition, now driven through the real manifest log rather than through
// Apply alone: 200 files across 4 levels must reconstruct exactly from a
// replay of what was actually written to disk.
func TestRecoverReconstructs200FilesAcrossFourLevels(t *testing.T) {
	dir, l := newLog(t)

	// Files are spread across L0..L3, 50 apiece. Levels below L0 must hold
	// non-overlapping ranges, so each level gets its own disjoint key space.
	type placed struct {
		level int
		m     *FileMetadata
	}
	var want []placed

	number := uint64(1)
	for level := 0; level < 4; level++ {
		for i := 0; i < 50; i++ {
			m := &FileMetadata{
				Number:      number,
				Size:        1024 + number,
				Smallest:    []byte(fmt.Sprintf("L%d-k%04d", level, i*10)),
				Largest:     []byte(fmt.Sprintf("L%d-k%04d", level, i*10+9)),
				SmallestSeq: number,
				LargestSeq:  number + 5,
			}
			want = append(want, placed{level: level, m: m})
			number++
		}
	}

	// Written across several records to prove replay accumulates across the
	// record boundary, not just within one edit.
	for i := 0; i < len(want); i += 25 {
		var e VersionEdit
		for _, p := range want[i:min(i+25, len(want))] {
			e.AddFile(p.level, p.m)
		}
		if err := l.Append(&e); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	vs, err := Recover(nil, dir)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	v := vs.Current()

	if got := v.TotalFiles(); got != 200 {
		t.Fatalf("TotalFiles = %d, want 200", got)
	}

	// Every file must come back with every field intact, at the right level.
	byNumber := map[uint64]*FileMetadata{}
	for level := 0; level < NumLevels; level++ {
		for _, f := range v.Files(level) {
			byNumber[f.Number] = f
		}
	}
	for _, p := range want {
		got, ok := byNumber[p.m.Number]
		if !ok {
			t.Fatalf("file %d missing after replay", p.m.Number)
		}
		if got.Size != p.m.Size ||
			string(got.Smallest) != string(p.m.Smallest) ||
			string(got.Largest) != string(p.m.Largest) ||
			got.SmallestSeq != p.m.SmallestSeq ||
			got.LargestSeq != p.m.LargestSeq {
			t.Fatalf("file %d = %+v, want %+v", p.m.Number, got, p.m)
		}
	}
	for level := 0; level < 4; level++ {
		if got := v.NumFiles(level); got != 50 {
			t.Errorf("level %d has %d files, want 50", level, got)
		}
	}
}

// TestRecoverStopsAtATruncatedEdit is T4.1's second done-when condition: a
// manifest truncated mid-edit recovers to the last complete edit. The
// truncation is swept across every length so it lands mid-header and
// mid-payload as well as on a boundary.
func TestRecoverStopsAtATruncatedEdit(t *testing.T) {
	dir, l := newLog(t)

	var first VersionEdit
	first.AddFile(0, meta(1, "a", "c"))
	first.SetLastSequence(10)
	if err := l.Append(&first); err != nil {
		t.Fatalf("Append first: %v", err)
	}

	var second VersionEdit
	second.AddFile(0, meta(2, "d", "f"))
	second.SetLastSequence(20)
	if err := l.Append(&second); err != nil {
		t.Fatalf("Append second: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	path := filepath.Join(dir, ManifestName(1))
	full, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}

	// The first record's length is fixed by the encoding; find where the
	// second begins by replaying a manifest holding only the first edit.
	firstOnly := len(first.Encode(nil)) + 7 // one FULL fragment header

	// The sweep runs up to and including the untruncated length. That last
	// case is what keeps the rest honest: it is the only cut that must
	// recover both edits, so if Append had silently written nothing the
	// whole sweep would still report one file and pass vacuously.
	sawBothEdits := false

	for cut := firstOnly; cut <= len(full); cut++ {
		truncDir := t.TempDir()
		if err := os.WriteFile(filepath.Join(truncDir, ManifestName(1)), full[:cut], 0o600); err != nil {
			t.Fatalf("write truncated: %v", err)
		}
		if err := WriteCurrent(nil, truncDir, ManifestName(1)); err != nil {
			t.Fatalf("WriteCurrent: %v", err)
		}

		vs, err := Recover(nil, truncDir)
		if err != nil {
			t.Fatalf("cut %d: Recover: %v", cut, err)
		}
		v := vs.Current()

		// Either the second edit committed in full or it did not exist at
		// all. A half-applied second edit -- the file present but the
		// sequence stale, or the reverse -- is the state §4.1 forbids.
		switch v.NumFiles(0) {
		case 1:
			if v.LastSequence() != 10 {
				t.Fatalf("cut %d: 1 file but LastSequence = %d, want 10", cut, v.LastSequence())
			}
		case 2:
			if v.LastSequence() != 20 {
				t.Fatalf("cut %d: 2 files but LastSequence = %d, want 20", cut, v.LastSequence())
			}
			sawBothEdits = true
		default:
			t.Fatalf("cut %d: NumFiles(0) = %d, want 1 or 2", cut, v.NumFiles(0))
		}
	}

	if !sawBothEdits {
		t.Fatal("no cut recovered both edits, so the second edit was never really written")
	}
}

// TestReplayIsOrderSensitive is T4.1's trap: an ADD followed by a DELETE of
// the same file must leave it absent, however replay accumulates state.
func TestReplayIsOrderSensitive(t *testing.T) {
	dir, l := newLog(t)

	var add VersionEdit
	add.AddFile(1, meta(1, "a", "c"))
	add.AddFile(1, meta(2, "d", "f"))
	if err := l.Append(&add); err != nil {
		t.Fatalf("Append add: %v", err)
	}

	var del VersionEdit
	del.DeleteFile(1, 1)
	if err := l.Append(&del); err != nil {
		t.Fatalf("Append delete: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	vs, err := Recover(nil, dir)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	v := vs.Current()

	if v.NumFiles(1) != 1 {
		t.Fatalf("NumFiles(1) = %d, want 1", v.NumFiles(1))
	}
	if got := v.Files(1)[0].Number; got != 2 {
		t.Errorf("surviving file = %d, want 2 (file 1 was deleted)", got)
	}
}

// TestRecoverRejectsCorruptionAfterValidRecords checks the line §4.2 draws:
// a damaged record with valid records following it is corruption, not a
// clean tail, and must not be silently accepted.
func TestRecoverRejectsCorruptionAfterValidRecords(t *testing.T) {
	dir, l := newLog(t)

	for i := 1; i <= 4; i++ {
		var e VersionEdit
		e.AddFile(0, meta(uint64(i), "a", "c"))
		if err := l.Append(&e); err != nil {
			t.Fatalf("Append %d: %v", i, err)
		}
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	path := filepath.Join(dir, ManifestName(1))
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	// Damage the first record's payload. Later records remain intact and
	// checksum-valid, so this cannot be mistaken for a torn tail.
	data[10] ^= 0xFF
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	if _, err := Recover(nil, dir); err == nil {
		t.Fatal("Recover accepted a corrupted record with valid records after it")
	}
}

// TestAppendSkipsEmptyEdits checks an edit that would change nothing does not
// reach the log, since it would cost an fsync to record no change.
func TestAppendSkipsEmptyEdits(t *testing.T) {
	dir, l := newLog(t)

	var empty VersionEdit
	if err := l.Append(&empty); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	info, err := os.Stat(filepath.Join(dir, ManifestName(1)))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Size() != 0 {
		t.Errorf("manifest is %d bytes, want 0 -- an empty edit was written", info.Size())
	}
}

// TestRecoverOnMissingCurrent checks a directory with no CURRENT is reported
// rather than silently recovering an empty version.
func TestRecoverOnMissingCurrent(t *testing.T) {
	if _, err := Recover(nil, t.TempDir()); err == nil {
		t.Fatal("Recover succeeded with no CURRENT file")
	} else if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, ErrCorrupt) {
		t.Logf("Recover error (acceptable, recorded for reference): %v", err)
	}
}
