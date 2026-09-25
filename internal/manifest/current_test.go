package manifest

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// writeManifestStub creates the file CURRENT will name, since ReadCurrent
// rejects a CURRENT pointing at something absent.
func writeManifestStub(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("stub"), 0o600); err != nil {
		t.Fatalf("write manifest stub: %v", err)
	}
}

func TestManifestName(t *testing.T) {
	if got := Name(1); got != "MANIFEST-000001" {
		t.Errorf("Name(1) = %q", got)
	}
	if got := Name(123456); got != "MANIFEST-123456" {
		t.Errorf("Name(123456) = %q", got)
	}
}

func TestWriteAndReadCurrent(t *testing.T) {
	dir := t.TempDir()
	name := Name(4)
	writeManifestStub(t, dir, name)

	if err := WriteCurrent(nil, dir, name); err != nil {
		t.Fatalf("WriteCurrent: %v", err)
	}

	got, err := ReadCurrent(nil, dir)
	if err != nil {
		t.Fatalf("ReadCurrent: %v", err)
	}
	if got != name {
		t.Errorf("ReadCurrent = %q, want %q", got, name)
	}

	// docs/format.md §1: the name plus exactly one newline, nothing else.
	raw, err := os.ReadFile(filepath.Join(dir, CurrentFile))
	if err != nil {
		t.Fatalf("read CURRENT: %v", err)
	}
	if string(raw) != name+"\n" {
		t.Errorf("CURRENT contents = %q, want %q", raw, name+"\n")
	}
}

// TestWriteCurrentLeavesNoTemporary matters because a leftover CURRENT.tmp
// would be mistaken for a stray file by the startup sweep that deletes
// anything the manifest does not reference.
func TestWriteCurrentLeavesNoTemporary(t *testing.T) {
	dir := t.TempDir()
	name := Name(1)
	writeManifestStub(t, dir, name)

	if err := WriteCurrent(nil, dir, name); err != nil {
		t.Fatalf("WriteCurrent: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, CurrentFile+".tmp")); !os.IsNotExist(err) {
		t.Errorf("CURRENT.tmp still exists after a successful write")
	}
}

// TestWriteCurrentReplacesAtomically checks the rename actually replaces
// rather than appending or leaving the old contents behind.
func TestWriteCurrentReplacesAtomically(t *testing.T) {
	dir := t.TempDir()
	first, second := Name(1), Name(2)
	writeManifestStub(t, dir, first)
	writeManifestStub(t, dir, second)

	if err := WriteCurrent(nil, dir, first); err != nil {
		t.Fatalf("first WriteCurrent: %v", err)
	}
	if err := WriteCurrent(nil, dir, second); err != nil {
		t.Fatalf("second WriteCurrent: %v", err)
	}

	got, err := ReadCurrent(nil, dir)
	if err != nil {
		t.Fatalf("ReadCurrent: %v", err)
	}
	if got != second {
		t.Errorf("ReadCurrent = %q, want %q", got, second)
	}
}

func TestReadCurrentRejectsCorruption(t *testing.T) {
	tests := []struct {
		name     string
		contents string
		// stub names a manifest file to create alongside, or "" for none.
		stub string
	}{
		{name: "empty", contents: ""},
		{name: "no trailing newline", contents: "MANIFEST-000001", stub: "MANIFEST-000001"},
		{name: "only a newline", contents: "\n"},
		{name: "names a missing manifest", contents: "MANIFEST-000009\n"},
		{name: "two lines", contents: "MANIFEST-000001\nMANIFEST-000002\n", stub: "MANIFEST-000001"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if tc.stub != "" {
				writeManifestStub(t, dir, tc.stub)
			}
			if err := os.WriteFile(filepath.Join(dir, CurrentFile), []byte(tc.contents), 0o600); err != nil {
				t.Fatalf("write CURRENT: %v", err)
			}

			if _, err := ReadCurrent(nil, dir); !errors.Is(err, ErrCorrupt) {
				t.Errorf("ReadCurrent = %v, want an ErrCorrupt", err)
			}
		})
	}
}

func TestReadCurrentMissingFile(t *testing.T) {
	// An absent CURRENT is not corruption: it is a directory that has never
	// been opened as a database, which the caller distinguishes with
	// os.IsNotExist to decide whether to initialise one.
	_, err := ReadCurrent(nil, t.TempDir())
	if err == nil {
		t.Fatal("ReadCurrent on an empty directory = nil, want an error")
	}
	if errors.Is(err, ErrCorrupt) {
		t.Errorf("ReadCurrent = %v, want a not-exist error rather than corruption", err)
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("ReadCurrent = %v, want it to wrap os.ErrNotExist", err)
	}
}

func TestSyncDirRejectsMissingDirectory(t *testing.T) {
	if err := SyncDir(nil, filepath.Join(t.TempDir(), "nope")); err == nil {
		t.Error("SyncDir on a missing directory = nil, want an error")
	}
}
