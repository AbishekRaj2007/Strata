package manifest

import (
	"testing"
)

func TestAcquireAndRelease(t *testing.T) {
	vs := NewVersionSet()

	var e VersionEdit
	e.AddFile(1, meta(1, "a", "c"))
	if _, err := vs.Apply(&e); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// The set holds one reference on the current version.
	if got := vs.VersionRefs(vs.Current()); got != 1 {
		t.Errorf("current version refs = %d, want 1", got)
	}
	if got := vs.FileRefs(1); got != 1 {
		t.Errorf("file refs = %d, want 1", got)
	}

	v := vs.Acquire()
	if got := vs.VersionRefs(v); got != 2 {
		t.Errorf("refs after Acquire = %d, want 2", got)
	}

	vs.Release(v)
	if got := vs.VersionRefs(v); got != 1 {
		t.Errorf("refs after Release = %d, want 1", got)
	}
}

// TestFileStaysLiveWhileAReaderHoldsAnOldVersion is T4.2's invariant: a file
// is deletable only when no held version names it. A compaction removing the
// file must not make it collectable while a reader is still working from the
// version that names it.
func TestFileStaysLiveWhileAReaderHoldsAnOldVersion(t *testing.T) {
	vs := NewVersionSet()

	var add VersionEdit
	add.AddFile(1, meta(1, "a", "c"))
	if _, err := vs.Apply(&add); err != nil {
		t.Fatalf("Apply add: %v", err)
	}

	// A reader starts an operation against the version naming file 1.
	held := vs.Acquire()

	// A compaction replaces it.
	var compact VersionEdit
	compact.DeleteFile(1, 1)
	compact.AddFile(1, meta(2, "a", "c"))
	if _, err := vs.Apply(&compact); err != nil {
		t.Fatalf("Apply compaction: %v", err)
	}

	if !vs.IsLive(1) {
		t.Error("file 1 became collectable while a reader still held a version naming it")
	}
	if got := vs.Obsolete(); len(got) != 0 {
		t.Errorf("obsolete = %v, want nothing while the old version is held", got)
	}

	// The reader finishes.
	vs.Release(held)

	if vs.IsLive(1) {
		t.Error("file 1 is still live after its last holder released")
	}
	got := vs.Obsolete()
	if len(got) != 1 || got[0] != 1 {
		t.Errorf("obsolete = %v, want [1]", got)
	}
}

// TestFileCarriedAcrossCompactionNeverGoesObsolete covers the ordering inside
// Apply: the new version references its files before the old one gives its
// references up, so a file present in both never momentarily hits zero.
func TestFileCarriedAcrossCompactionNeverGoesObsolete(t *testing.T) {
	vs := NewVersionSet()

	kept := meta(1, "a", "c")
	var add VersionEdit
	add.AddFile(1, kept)
	add.AddFile(1, meta(2, "m", "p"))
	if _, err := vs.Apply(&add); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// Compact only file 2, leaving file 1 in place.
	var compact VersionEdit
	compact.DeleteFile(1, 2)
	compact.AddFile(1, meta(3, "m", "p"))
	if _, err := vs.Apply(&compact); err != nil {
		t.Fatalf("Apply compaction: %v", err)
	}

	if !vs.IsLive(1) {
		t.Error("a file carried across the compaction lost its last reference")
	}
	for _, n := range vs.Obsolete() {
		if n == 1 {
			t.Error("a file present in both versions was marked obsolete")
		}
	}
}

func TestReleaseBelowZeroPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("releasing an unreferenced version did not panic")
		}
	}()

	vs := NewVersionSet()
	v := vs.Acquire()
	vs.Release(v)
	vs.Release(v) // the set's own reference is not the caller's to drop
	vs.Release(v)
}

func TestSupersededVersionDropsItsFiles(t *testing.T) {
	vs := NewVersionSet()

	var add VersionEdit
	add.AddFile(0, meta(1, "a", "c"))
	if _, err := vs.Apply(&add); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	// No reader holds the old version, so replacing it retires the file
	// immediately.
	var compact VersionEdit
	compact.DeleteFile(0, 1)
	if _, err := vs.Apply(&compact); err != nil {
		t.Fatalf("Apply compaction: %v", err)
	}

	if vs.IsLive(1) {
		t.Error("file 1 is live with no version naming it")
	}
	if got := vs.Obsolete(); len(got) != 1 || got[0] != 1 {
		t.Errorf("obsolete = %v, want [1]", got)
	}
}
