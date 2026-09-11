package sstable

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/AbishekRaj2007/Strata/internal/memtable"
)

func outEntry(key string, seq uint64, value string) memtable.Entry {
	return memtable.Entry{Key: []byte(key), Sequence: seq, Value: []byte(value)}
}

func TestFileWriterRoundTrips(t *testing.T) {
	dir := t.TempDir()

	w, err := Create(dir, 7, WriterOptions{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if got, want := w.Path(), filepath.Join(dir, "000007.sst"); got != want {
		t.Errorf("path = %q, want %q", got, want)
	}

	want := []memtable.Entry{outEntry("a", 1, "1"), outEntry("b", 2, "2"), outEntry("c", 3, "3")}
	for _, e := range want {
		if err := w.Add(e); err != nil {
			t.Fatalf("add %q: %v", e.Key, err)
		}
	}
	if got := w.EntryCount(); got != 3 {
		t.Errorf("entry count = %d, want 3", got)
	}

	info, err := w.Finish()
	if err != nil {
		t.Fatalf("finish: %v", err)
	}
	if info.EntryCount != 3 {
		t.Errorf("info entry count = %d, want 3", info.EntryCount)
	}

	tbl, err := Open(info.Path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = tbl.Close() }()

	for _, e := range want {
		got, found, err := tbl.Get(e.Key)
		if err != nil || !found {
			t.Fatalf("get %q: found=%v err=%v", e.Key, found, err)
		}
		if string(got.Value) != string(e.Value) {
			t.Errorf("get %q = %q, want %q", e.Key, got.Value, e.Value)
		}
	}
}

// Size is what compaction rolls output files on, so it has to grow with the
// data rather than only at block boundaries -- a writer reporting zero until
// its first block flushed would overshoot the target by a whole block.
func TestFileWriterSizeTracksUnflushedData(t *testing.T) {
	dir := t.TempDir()

	w, err := Create(dir, 1, WriterOptions{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer func() { _ = w.Abort() }()

	if got := w.Size(); got != 0 {
		t.Errorf("empty writer size = %d, want 0", got)
	}

	value := string(make([]byte, 512))
	if err := w.Add(outEntry("a", 1, value)); err != nil {
		t.Fatalf("add: %v", err)
	}
	if got := w.Size(); got < 512 {
		t.Errorf("size = %d after a 512-byte value, want at least that: the open block is not counted", got)
	}
}

// A finished file must be at least as large as the size the writer reported,
// never smaller: rolling decisions are made against that figure, and a writer
// that overcounted would cut files short of the target.
func TestFileWriterSizeIsALowerBound(t *testing.T) {
	dir := t.TempDir()

	w, err := Create(dir, 1, WriterOptions{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	for i := 0; i < 200; i++ {
		if err := w.Add(outEntry(string(rune('a'+i/26))+string(rune('a'+i%26)), uint64(i+1), "v")); err != nil {
			t.Fatalf("add: %v", err)
		}
	}

	reported := w.Size()
	info, err := w.Finish()
	if err != nil {
		t.Fatalf("finish: %v", err)
	}
	if info.Size < reported {
		t.Errorf("finished size %d is below the reported %d", info.Size, reported)
	}
}

func TestFileWriterAbortRemovesThePartialFile(t *testing.T) {
	dir := t.TempDir()

	w, err := Create(dir, 3, WriterOptions{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := w.Add(outEntry("a", 1, "v")); err != nil {
		t.Fatalf("add: %v", err)
	}

	if err := w.Abort(); err != nil {
		t.Fatalf("abort: %v", err)
	}
	if _, err := os.Stat(w.Path()); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("stat after abort: %v, want the file to be gone", err)
	}
}

// Abort is deferred alongside Finish on every write path, so it has to be
// harmless once the table is committed -- removing a finished table would
// delete data the manifest is about to reference.
func TestFileWriterAbortAfterFinishIsANoOp(t *testing.T) {
	dir := t.TempDir()

	w, err := Create(dir, 4, WriterOptions{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := w.Add(outEntry("a", 1, "v")); err != nil {
		t.Fatalf("add: %v", err)
	}
	info, err := w.Finish()
	if err != nil {
		t.Fatalf("finish: %v", err)
	}

	if err := w.Abort(); err != nil {
		t.Fatalf("abort after finish: %v", err)
	}
	if _, err := os.Stat(info.Path); err != nil {
		t.Errorf("the finished table was removed: %v", err)
	}
}

// docs/format.md §1 never reuses a file number, so a collision is an
// allocator bug. Truncating the existing file would destroy live data.
func TestCreateRefusesAnExistingNumber(t *testing.T) {
	dir := t.TempDir()

	w, err := Create(dir, 5, WriterOptions{})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	defer func() { _ = w.Abort() }()

	if _, err := Create(dir, 5, WriterOptions{}); err == nil {
		t.Error("created a second writer for file 5; the first one's data would be lost")
	}
}
