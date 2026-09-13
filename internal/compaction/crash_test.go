package compaction

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/AbishekRaj2007/Strata/internal/manifest"
	"github.com/AbishekRaj2007/Strata/internal/memtable"
)

// This file is T6.3's done-when condition: 50 kills aimed precisely at the
// windows either side of the manifest fsync, each leaving the database
// correct, with orphans cleaned on restart.
//
// The kill is simulated by abandoning the commit at an exact step and then
// reopening the directory from scratch. That is not an approximation of
// kill -9, it is the same thing: SIGKILL destroys the process, not the page
// cache, so every byte already written is still readable after restart.
// Aiming a real signal at a window microseconds wide would need this hook
// anyway, and would only add a subprocess between the test and the bytes it
// is checking.

// snapshot reads every visible key in a version, walking levels top-down the
// way the read path does. It is the observable state of the database, and it
// is what must not change across a crash.
func snapshot(t *testing.T, dir string, v *manifest.Version) map[string]string {
	t.Helper()

	var levels [][]*manifest.FileMetadata
	keys := map[string]bool{}
	for level := 0; level < manifest.NumLevels; level++ {
		files := v.Files(level)
		levels = append(levels, files)
		for _, f := range files {
			for _, e := range tableEntries(t, dir, f) {
				keys[string(e.Key)] = true
			}
		}
	}

	out := map[string]string{}
	for key := range keys {
		if value, visible := readLevels(t, dir, levels, key); visible {
			out[key] = value
		}
	}
	return out
}

// tables lists the .sst files present in the directory.
func tables(t *testing.T, dir string) []string {
	t.Helper()

	names, err := filepath.Glob(filepath.Join(dir, "*.sst"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	for i, n := range names {
		names[i] = filepath.Base(n)
	}
	sort.Strings(names)
	return names
}

// restart reopens the directory the way engine.Open does: replay the
// manifest, then sweep every table the recovered version does not name.
func restart(t *testing.T, dir string) (*manifest.VersionSet, int) {
	t.Helper()

	vs, err := manifest.Recover(nil, dir)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	swept, err := manifest.SweepOrphans(nil, dir, vs.Current())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	return vs, swept
}

// TestCommitSurvivesAKillAtEitherSideOfTheFsync is the done-when condition.
func TestCommitSurvivesAKillAtEitherSideOfTheFsync(t *testing.T) {
	const kills = 50

	for i := 0; i < kills; i++ {
		// Alternate the two windows so each gets 25 kills, and vary the
		// shape of the compaction so they are not 25 copies of one case.
		killAt := StepBeforeManifestSync
		window := "before the fsync"
		if i%2 == 1 {
			killAt, window = StepAfterManifestSync, "after the fsync"
		}

		t.Run(fmt.Sprintf("kill_%02d_%s", i, strings.ReplaceAll(window, " ", "_")), func(t *testing.T) {
			f := newFixture(t)

			// Two overlapping L1 files and one L2 file beneath them, so the
			// compaction deletes from two levels and the merge has real
			// versions to resolve. The value length varies with i so output
			// files roll differently between runs.
			value := strings.Repeat("v", 16+i)
			a := f.install(1, []memtable.Entry{
				entry("k1", 10, value+"-new"),
				entry("k3", 11, value),
			})
			b := f.install(2, []memtable.Entry{
				entry("k1", 1, value+"-old"),
				entry("k2", 2, value),
			})

			before := snapshot(t, f.dir, f.vs.Current())
			if len(before) != 3 {
				t.Fatalf("the scenario is wrong: %d visible keys, want 3", len(before))
			}

			c := &Compaction{
				Level:      1,
				Base:       []*manifest.FileMetadata{a},
				Parent:     []*manifest.FileMetadata{b},
				BottomMost: true,
			}
			res, err := f.executor(Options{TargetFileBytes: uint64(64 + i)}).Run(c)
			if err != nil {
				t.Fatalf("run: %v", err)
			}

			f.cm.OnStep = func(s CommitStep) error {
				if s == killAt {
					return ErrCommitAborted
				}
				return nil
			}
			if _, err := f.cm.Commit(c, res); !errors.Is(err, ErrCommitAborted) {
				t.Fatalf("commit: %v, want the injected abort", err)
			}

			// The kill lands here. Everything in memory is gone; only what
			// was written survives.
			_ = f.log.Close()

			vs, swept := restart(t, f.dir)
			v := vs.Current()

			// The state must be one of exactly two, never a mixture.
			committed := v.NumFiles(1) == 0
			switch killAt {
			case StepBeforeManifestSync:
				if committed {
					t.Fatal("the compaction committed despite being killed before the fsync")
				}
				if swept != len(res.Outputs) {
					t.Errorf("swept %d files, want the %d orphaned outputs", swept, len(res.Outputs))
				}
			case StepAfterManifestSync:
				if !committed {
					t.Fatal("the compaction did not commit despite the fsync having returned")
				}
				// The inputs were still on disk when the kill landed; the
				// restart is what collects them.
				if swept != 2 {
					t.Errorf("swept %d files, want the 2 superseded inputs", swept)
				}
			}

			// Whichever state it landed in, the data must be identical.
			after := snapshot(t, f.dir, v)
			if len(after) != len(before) {
				t.Fatalf("visible keys = %d, want %d", len(after), len(before))
			}
			for key, want := range before {
				if got := after[key]; got != want {
					t.Errorf("key %q = %q after the kill, want %q", key, got, want)
				}
			}

			// And no debris: every file on disk is named by the version, and
			// every file the version names is on disk.
			named := map[string]bool{}
			for level := 0; level < manifest.NumLevels; level++ {
				for _, fm := range v.Files(level) {
					named[fm.Name()] = true
					if _, err := os.Stat(filepath.Join(f.dir, fm.Name())); err != nil {
						t.Errorf("the version names %s, which is not on disk: %v", fm.Name(), err)
					}
				}
			}
			for _, name := range tables(t, f.dir) {
				if !named[name] {
					t.Errorf("%s survived the restart unnamed by any version", name)
				}
			}
		})
	}
}
