package manifest

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

func touch(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
		t.Fatalf("touch %s: %v", name, err)
	}
}

func tableName(number uint64) string { return fmt.Sprintf("%06d.sst", number) }

func names(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read dir: %v", err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out
}

func TestDeleteObsolete(t *testing.T) {
	dir := t.TempDir()
	vs := NewVersionSet()

	var add VersionEdit
	add.AddFile(0, meta(1, "a", "c"))
	if _, err := vs.Apply(&add); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	touch(t, dir, tableName(1))

	// While the file is live, nothing is deleted.
	n, err := vs.DeleteObsolete(dir)
	if err != nil {
		t.Fatalf("DeleteObsolete: %v", err)
	}
	if n != 0 {
		t.Errorf("deleted %d files, want 0 while file 1 is live", n)
	}
	if _, err := os.Stat(filepath.Join(dir, tableName(1))); err != nil {
		t.Errorf("live file was deleted: %v", err)
	}

	var del VersionEdit
	del.DeleteFile(0, 1)
	if _, err := vs.Apply(&del); err != nil {
		t.Fatalf("Apply delete: %v", err)
	}

	n, err = vs.DeleteObsolete(dir)
	if err != nil {
		t.Fatalf("DeleteObsolete: %v", err)
	}
	if n != 1 {
		t.Errorf("deleted %d files, want 1", n)
	}
	if _, err := os.Stat(filepath.Join(dir, tableName(1))); !os.IsNotExist(err) {
		t.Error("obsolete file still on disk")
	}
}

// TestDeleteObsoleteWaitsForReaders is the disk-level statement of T4.2's
// invariant: the file survives on disk for as long as a reader holds a version
// naming it.
func TestDeleteObsoleteWaitsForReaders(t *testing.T) {
	dir := t.TempDir()
	vs := NewVersionSet()

	var add VersionEdit
	add.AddFile(0, meta(1, "a", "c"))
	if _, err := vs.Apply(&add); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	touch(t, dir, tableName(1))

	held := vs.Acquire()

	var del VersionEdit
	del.DeleteFile(0, 1)
	if _, err := vs.Apply(&del); err != nil {
		t.Fatalf("Apply delete: %v", err)
	}

	if n, err := vs.DeleteObsolete(dir); err != nil || n != 0 {
		t.Fatalf("DeleteObsolete = %d, %v; want 0, nil while a reader holds the version", n, err)
	}
	if _, err := os.Stat(filepath.Join(dir, tableName(1))); err != nil {
		t.Fatalf("file deleted while a reader held a version naming it: %v", err)
	}

	vs.Release(held)

	if n, err := vs.DeleteObsolete(dir); err != nil || n != 1 {
		t.Fatalf("DeleteObsolete = %d, %v; want 1, nil after release", n, err)
	}
}

func TestDeleteObsoleteToleratesMissingFiles(t *testing.T) {
	dir := t.TempDir()
	vs := NewVersionSet()

	var add VersionEdit
	add.AddFile(0, meta(1, "a", "c"))
	if _, err := vs.Apply(&add); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	// Deliberately do not create the file on disk.

	var del VersionEdit
	del.DeleteFile(0, 1)
	if _, err := vs.Apply(&del); err != nil {
		t.Fatalf("Apply delete: %v", err)
	}

	if _, err := vs.DeleteObsolete(dir); err != nil {
		t.Errorf("DeleteObsolete on an already-absent file = %v, want nil", err)
	}
}

func TestSweepOrphans(t *testing.T) {
	dir := t.TempDir()

	var e VersionEdit
	e.AddFile(0, meta(1, "a", "c"))
	e.AddFile(1, meta(2, "d", "f"))
	v, err := NewVersion().Apply(&e)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}

	touch(t, dir, tableName(1)) // live
	touch(t, dir, tableName(2)) // live
	touch(t, dir, tableName(3)) // orphaned compaction output
	touch(t, dir, tableName(4)) // orphaned compaction output

	// Files the sweep must never touch.
	touch(t, dir, "000005.wal")
	touch(t, dir, "MANIFEST-000001")
	touch(t, dir, CurrentFile)
	touch(t, dir, "LOCK")

	removed, err := SweepOrphans(dir, v)
	if err != nil {
		t.Fatalf("SweepOrphans: %v", err)
	}
	if removed != 2 {
		t.Errorf("removed %d files, want 2", removed)
	}

	want := []string{"000001.sst", "000002.sst", "000005.wal", "CURRENT", "LOCK", "MANIFEST-000001"}
	got := names(t, dir)
	if len(got) != len(want) {
		t.Fatalf("directory holds %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestSweepOrphansOnEmptyVersion(t *testing.T) {
	dir := t.TempDir()
	touch(t, dir, tableName(1))
	touch(t, dir, tableName(2))

	removed, err := SweepOrphans(dir, NewVersion())
	if err != nil {
		t.Fatalf("SweepOrphans: %v", err)
	}
	if removed != 2 {
		t.Errorf("removed %d files, want 2; an empty version names nothing", removed)
	}
}

// TestParseTableNameIsStrict matters because a loose match would have the
// startup sweep delete the WAL, the manifest, or CURRENT.
func TestParseTableNameIsStrict(t *testing.T) {
	tests := []struct {
		name   string
		want   uint64
		wantOk bool
	}{
		{"000001.sst", 1, true},
		{"123456.sst", 123456, true},
		{"1234567.sst", 1234567, true},
		{"1.sst", 0, false},       // not zero-padded; would alias 000001.sst
		{"0000001.sst", 0, false}, // over-padded; same aliasing risk
		{"000001.wal", 0, false},
		{"MANIFEST-000001", 0, false},
		{"CURRENT", 0, false},
		{"LOCK", 0, false},
		{".sst", 0, false},
		{"abcdef.sst", 0, false},
		{"-00001.sst", 0, false},
		{"000001.sst.tmp", 0, false},
	}

	for _, tc := range tests {
		got, ok := parseTableName(tc.name)
		if ok != tc.wantOk || got != tc.want {
			t.Errorf("parseTableName(%q) = %d,%v; want %d,%v", tc.name, got, ok, tc.want, tc.wantOk)
		}
	}
}
