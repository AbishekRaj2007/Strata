package compaction

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/AbishekRaj2007/Strata/internal/manifest"
	"github.com/AbishekRaj2007/Strata/internal/memtable"
)

// violated reports whether the report names an invariant.
func violated(r *Report, invariant string) bool {
	for _, v := range r.Violations {
		if v.Invariant == invariant {
			return true
		}
	}
	return false
}

func names(r *Report) string {
	var out []string
	for _, v := range r.Violations {
		out = append(out, v.Invariant)
	}
	return strings.Join(out, ",")
}

func TestCheckPassesAHealthyTree(t *testing.T) {
	f := newFixture(t)

	f.install(1, []memtable.Entry{entry("a", 1, "1"), entry("b", 2, "2")})
	f.install(2, []memtable.Entry{entry("m", 3, "3")})

	r, err := Check(f.dir, f.vs.Current())
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if !r.OK() {
		t.Errorf("a healthy tree reported %s", names(r))
	}
	if r.FilesChecked != 2 || r.EntriesChecked != 3 {
		t.Errorf("checked %d files and %d entries, want 2 and 3", r.FilesChecked, r.EntriesChecked)
	}
}

func TestCheckPassesAfterARealCompaction(t *testing.T) {
	f := newFixture(t)
	opts := Options{L0Trigger: 2, BaseLevelBytes: 1 << 12, TargetFileBytes: 1 << 12}

	for i := 0; i < 6; i++ {
		f.flushL0(i)
	}
	if err := f.scheduler(opts).Drain(); err != nil {
		t.Fatalf("drain: %v", err)
	}

	r, err := Check(f.dir, f.vs.Current())
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if !r.OK() {
		t.Errorf("a compacted tree reported %s:\n%v", names(r), r.Err())
	}
	if r.FilesChecked == 0 {
		t.Error("the check examined nothing")
	}
}

// The done-when condition's second half: a deliberately corrupted version is
// caught immediately.
func TestCheckCatchesADeliberatelyCorruptedTree(t *testing.T) {
	tests := []struct {
		name    string
		want    string
		corrupt func(t *testing.T, f *fixture) *manifest.Version
	}{
		{
			name: "two files at one level holding the same key",
			want: "one-version-per-key-per-level",
			corrupt: func(t *testing.T, f *fixture) *manifest.Version {
				// Built by hand, because a correct compaction cannot produce
				// it: two L2 files whose ranges are disjoint by metadata but
				// whose contents are not.
				one := writeTable(t, f.dir, f.vs.NextFileNumber(), []memtable.Entry{entry("a", 1, "1")})
				two := writeTable(t, f.dir, f.vs.NextFileNumber(), []memtable.Entry{entry("a", 2, "2")})
				two.Smallest, two.Largest = []byte("b"), []byte("b")

				var e manifest.VersionEdit
				e.AddFile(2, one)
				e.AddFile(2, two)
				v, err := f.vs.Current().Apply(&e)
				if err != nil {
					t.Fatalf("apply: %v", err)
				}
				return v
			},
		},
		{
			name: "a file the manifest names is gone from disk",
			want: "manifest-file-exists",
			corrupt: func(t *testing.T, f *fixture) *manifest.Version {
				meta := f.install(1, []memtable.Entry{entry("a", 1, "1")})
				if err := os.Remove(filepath.Join(f.dir, meta.Name())); err != nil {
					t.Fatalf("remove: %v", err)
				}
				return f.vs.Current()
			},
		},
		{
			name: "a table on disk that no level names",
			want: "no-orphan-tables",
			corrupt: func(t *testing.T, f *fixture) *manifest.Version {
				f.install(1, []memtable.Entry{entry("a", 1, "1")})
				writeTable(t, f.dir, f.vs.NextFileNumber(), []memtable.Entry{entry("z", 9, "9")})
				return f.vs.Current()
			},
		},
		{
			name: "metadata whose key bounds exclude the file's own keys",
			want: "metadata-key-bounds",
			corrupt: func(t *testing.T, f *fixture) *manifest.Version {
				meta := writeTable(t, f.dir, f.vs.NextFileNumber(), []memtable.Entry{entry("m", 1, "1")})
				meta.Smallest, meta.Largest = []byte("x"), []byte("z")

				var e manifest.VersionEdit
				e.AddFile(1, meta)
				v, err := f.vs.Current().Apply(&e)
				if err != nil {
					t.Fatalf("apply: %v", err)
				}
				return v
			},
		},
		{
			name: "an L0 file numbered above one that holds newer data",
			want: "l0-number-tracks-recency",
			corrupt: func(t *testing.T, f *fixture) *manifest.Version {
				// File 2 is numbered higher, so the read path consults it
				// first -- but it holds the older sequence.
				old := writeTable(t, f.dir, f.vs.NextFileNumber(), []memtable.Entry{entry("a", 9, "new")})
				stale := writeTable(t, f.dir, f.vs.NextFileNumber(), []memtable.Entry{entry("b", 1, "old")})

				var e manifest.VersionEdit
				e.AddFile(0, old)
				e.AddFile(0, stale)
				e.SetLastSequence(9)
				v, err := f.vs.Current().Apply(&e)
				if err != nil {
					t.Fatalf("apply: %v", err)
				}
				return v
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			v := tc.corrupt(t, f)

			r, err := Check(f.dir, v)
			if err != nil {
				t.Fatalf("check: %v", err)
			}
			if !violated(r, tc.want) {
				t.Errorf("violations = [%s], want %s", names(r), tc.want)
			}
			if r.OK() {
				t.Error("Err() is nil for a corrupted tree")
			}
		})
	}
}

// The accounting identity that no structural check can replace: a merge that
// loses a key leaves a perfectly well-formed tree.
func TestCheckResultCatchesALostKey(t *testing.T) {
	if err := CheckResult(&Result{Keys: 10, EntriesWritten: 8, TombstonesDropped: 2}); err != nil {
		t.Errorf("a balanced result was rejected: %v", err)
	}
	if err := CheckResult(&Result{Keys: 10, EntriesWritten: 7, TombstonesDropped: 2}); err == nil {
		t.Error("a merge that lost a key was accepted")
	}
}

// With verification on, the compactor must refuse to keep going once an
// invariant breaks, rather than compacting on top of a broken tree.
func TestVerifyingSchedulerStopsOnAViolation(t *testing.T) {
	f := newFixture(t)
	opts := Options{L0Trigger: 2, BaseLevelBytes: 1 << 12, TargetFileBytes: 1 << 12, Verify: true}

	for i := 0; i < 4; i++ {
		f.flushL0(i)
	}

	// Delete a file the manifest names. It is a violation the checker
	// reports in a live process, and one a correct compaction cannot cause,
	// which makes it a clean way to prove the wiring runs.
	if err := os.Remove(filepath.Join(f.dir, f.vs.Current().Files(0)[0].Name())); err != nil {
		t.Fatalf("remove: %v", err)
	}

	s := f.scheduler(opts)
	if _, err := s.CompactOnce(); err == nil {
		t.Fatal("the verifying compactor accepted a tree with a violation")
	} else if !strings.Contains(err.Error(), "manifest-file-exists") &&
		!strings.Contains(err.Error(), "open input") {
		t.Errorf("error = %v, want it to name the violated invariant", err)
	}
}

// The orphan rule holds at rest and must not be applied to a live tree: both
// write paths create a durable table before the manifest names it.
func TestCheckLiveToleratesAnUncommittedTable(t *testing.T) {
	f := newFixture(t)
	f.install(1, []memtable.Entry{entry("a", 1, "1")})

	// A table on disk that no version names -- a flush or a merge in the
	// window before its commit.
	writeTable(t, f.dir, f.vs.NextFileNumber(), []memtable.Entry{entry("z", 9, "9")})

	live, err := CheckLive(f.dir, f.vs.Current())
	if err != nil {
		t.Fatalf("check live: %v", err)
	}
	if !live.OK() {
		t.Errorf("CheckLive reported %s for a table mid-commit", names(live))
	}

	atRest, err := Check(f.dir, f.vs.Current())
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if !violated(atRest, "no-orphan-tables") {
		t.Errorf("Check reported %s at rest, want the orphan caught", names(atRest))
	}
}

func TestCheckDirReplaysFromDisk(t *testing.T) {
	f := newFixture(t)
	f.install(1, []memtable.Entry{entry("a", 1, "1")})

	r, err := CheckDir(f.dir)
	if err != nil {
		t.Fatalf("check dir: %v", err)
	}
	if !r.OK() {
		t.Errorf("a healthy directory reported %s", names(r))
	}
}
