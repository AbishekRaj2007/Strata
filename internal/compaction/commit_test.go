package compaction

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/AbishekRaj2007/Strata/internal/manifest"
	"github.com/AbishekRaj2007/Strata/internal/memtable"
)

// fixture is a data directory with a manifest, a version set, and a
// committer over them -- the minimum a commit needs.
type fixture struct {
	t   *testing.T
	dir string
	log *manifest.Log
	vs  *manifest.VersionSet
	cm  *Committer
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()

	vs := manifest.NewVersionSet()
	log, err := manifest.CreateLog(nil, dir, vs.NextFileNumber())
	if err != nil {
		t.Fatalf("create manifest: %v", err)
	}
	t.Cleanup(func() { _ = log.Close() })

	if err := manifest.WriteCurrent(nil, dir, log.Name()); err != nil {
		t.Fatalf("write CURRENT: %v", err)
	}

	f := &fixture{t: t, dir: dir, log: log, vs: vs}
	f.cm = &Committer{Dir: dir, Log: log, Versions: vs}
	return f
}

// install adds a table at a level through the manifest, the way a flush
// would, so the fixture starts from a state the engine could really be in.
func (f *fixture) install(level int, entries []memtable.Entry) *manifest.FileMetadata {
	f.t.Helper()

	meta := writeTable(f.t, f.dir, f.vs.NextFileNumber(), entries)

	var e manifest.VersionEdit
	e.AddFile(level, meta)
	if err := f.log.Append(&e); err != nil {
		f.t.Fatalf("append: %v", err)
	}
	if _, err := f.vs.Apply(&e); err != nil {
		f.t.Fatalf("apply: %v", err)
	}
	return meta
}

func (f *fixture) executor(opts Options) *Executor {
	return &Executor{Dir: f.dir, Opts: opts, NextFileNumber: f.vs.NextFileNumber}
}

func (f *fixture) exists(number uint64) bool {
	f.t.Helper()
	_, err := os.Stat(filepath.Join(f.dir, fmt.Sprintf("%06d.sst", number)))
	return err == nil
}

func TestEditReplacesEveryInputWithEveryOutput(t *testing.T) {
	c := &Compaction{
		Level:  1,
		Base:   []*manifest.FileMetadata{file(1, "a", "b", 10), file(2, "c", "d", 10)},
		Parent: []*manifest.FileMetadata{file(3, "a", "d", 10)},
	}
	res := &Result{Outputs: []*manifest.FileMetadata{file(10, "a", "c", 10), file(11, "c", "d", 10)}}

	e := Edit(c, res)

	if len(e.Deleted) != 3 {
		t.Errorf("deletions = %d, want one per input", len(e.Deleted))
	}
	for _, d := range e.Deleted {
		want := 1
		if d.Number == 3 {
			want = 2
		}
		if d.Level != want {
			t.Errorf("file %d deleted from level %d, want %d", d.Number, d.Level, want)
		}
	}

	if len(e.Added) != 2 {
		t.Fatalf("additions = %d, want one per output", len(e.Added))
	}
	for _, a := range e.Added {
		if a.Level != 2 {
			t.Errorf("output %d added at level %d, want the output level 2", a.Meta.Number, a.Level)
		}
	}

	if e.NextFileNumber == nil || *e.NextFileNumber != 12 {
		t.Errorf("next file number = %v, want 12: a replay must not reissue an output's number", e.NextFileNumber)
	}
}

func TestCommitInstallsTheOutputsAndRetiresTheInputs(t *testing.T) {
	f := newFixture(t)

	in := f.install(1, []memtable.Entry{entry("a", 1, "1"), entry("b", 2, "2")})

	c := &Compaction{Level: 1, Base: []*manifest.FileMetadata{in}, BottomMost: true}
	res, err := f.executor(Options{}).Run(c)
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	next, err := f.cm.Commit(c, res)
	if err != nil {
		t.Fatalf("commit: %v", err)
	}

	if next.NumFiles(1) != 0 {
		t.Errorf("L1 still holds %d files", next.NumFiles(1))
	}
	if next.NumFiles(2) != len(res.Outputs) {
		t.Errorf("L2 holds %d files, want %d", next.NumFiles(2), len(res.Outputs))
	}
	if f.exists(in.Number) {
		t.Error("the input file is still on disk after a committed compaction")
	}
	for _, out := range res.Outputs {
		if !f.exists(out.Number) {
			t.Errorf("output %d is missing from disk", out.Number)
		}
	}

	// And the commit is durable: a replay of the manifest sees the same
	// thing, which is what makes the crash-after case "it fully happened".
	replayed, err := manifest.Recover(nil, f.dir)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if got := replayed.Current().NumFiles(2); got != len(res.Outputs) {
		t.Errorf("after replay L2 holds %d files, want %d", got, len(res.Outputs))
	}
	if got := replayed.Current().NumFiles(1); got != 0 {
		t.Errorf("after replay L1 holds %d files, want none", got)
	}
}

// The file-lifetime rule: an input a reader is still holding must survive the
// commit, however thoroughly it has been superseded.
func TestCommitKeepsInputsAliveForAHeldVersion(t *testing.T) {
	f := newFixture(t)

	in := f.install(1, []memtable.Entry{entry("a", 1, "1")})

	// A reader that started before the compaction.
	held := f.vs.Acquire()

	c := &Compaction{Level: 1, Base: []*manifest.FileMetadata{in}, BottomMost: true}
	res, err := f.executor(Options{}).Run(c)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if _, err := f.cm.Commit(c, res); err != nil {
		t.Fatalf("commit: %v", err)
	}

	if !f.exists(in.Number) {
		t.Fatal("the input was deleted while a held version still named it; the reader would fault")
	}
	if got := held.NumFiles(1); got != 1 {
		t.Errorf("the held version changed under the reader: L1 has %d files", got)
	}

	// Once the reader is done, the next commit's sweep collects it.
	f.vs.Release(held)
	if err := f.cm.DropObsolete(); err != nil {
		t.Fatalf("drop obsolete: %v", err)
	}
	if f.exists(in.Number) {
		t.Error("the input survived its last reference being released")
	}
}

// An edit that cannot be applied must be rejected before it reaches the
// manifest. Once the fsync returns, the durable state says the compaction
// happened, and an in-memory refusal afterwards is unrecoverable.
func TestCommitRejectsAnInapplicableEditBeforeWriting(t *testing.T) {
	f := newFixture(t)

	in := f.install(1, []memtable.Entry{entry("a", 1, "1")})

	c := &Compaction{Level: 1, Base: []*manifest.FileMetadata{in}, BottomMost: true}
	res, err := f.executor(Options{}).Run(c)
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	// Commit once, legitimately.
	if _, err := f.cm.Commit(c, res); err != nil {
		t.Fatalf("first commit: %v", err)
	}
	before := f.vs.Current()

	// Committing the same compaction again names an input that is no longer
	// there.
	if _, err := f.cm.Commit(c, res); err == nil {
		t.Fatal("committed an edit deleting a file that is already gone")
	}
	if f.vs.Current() != before {
		t.Error("the version changed despite the commit failing")
	}

	// The rejected edit must not be in the manifest either: a replay that
	// hit it would fail to recover the database at all.
	if _, err := manifest.Recover(nil, f.dir); err != nil {
		t.Errorf("the rejected edit reached the manifest and broke replay: %v", err)
	}
}

// Startup sweeps outputs that a crash left behind before the commit point --
// the "it never happened" state.
func TestOrphanedOutputsAreSweptAtStartup(t *testing.T) {
	f := newFixture(t)

	in := f.install(1, []memtable.Entry{entry("a", 1, "1")})

	c := &Compaction{Level: 1, Base: []*manifest.FileMetadata{in}, BottomMost: true}
	res, err := f.executor(Options{}).Run(c)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(res.Outputs) == 0 {
		t.Fatal("the compaction wrote nothing")
	}

	// The crash lands here: outputs on disk, no manifest record.
	replayed, err := manifest.Recover(nil, f.dir)
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	removed, err := manifest.SweepOrphans(nil, f.dir, replayed.Current())
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if removed != len(res.Outputs) {
		t.Errorf("swept %d files, want the %d orphaned outputs", removed, len(res.Outputs))
	}

	if !f.exists(in.Number) {
		t.Error("the sweep removed the input, which the manifest still names")
	}
	for _, out := range res.Outputs {
		if f.exists(out.Number) {
			t.Errorf("orphaned output %d survived the sweep", out.Number)
		}
	}
}
