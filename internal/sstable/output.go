package sstable

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"

	"github.com/AbishekRaj2007/Strata/internal/memtable"
	"github.com/AbishekRaj2007/Strata/internal/vfs"
)

// Size is how many bytes the table would occupy if it were finished now,
// counting the block still being built.
//
// Compaction rolls output files on this. It is an estimate in one direction
// only: the bloom filter, index and footer are not yet written, so the real
// file is larger than the figure reported here, never smaller. A roll
// threshold is a target rather than a limit, so undercounting the trailer is
// acceptable in a way that overcounting live data would not be.
func (w *Writer) Size() int64 { return w.offset + int64(w.block.Size()) }

// EntryCount is how many entries have been added.
func (w *Writer) EntryCount() int { return w.entryCount }

// FileWriter builds one table into a file it owns, for callers that produce
// entries incrementally rather than from a single iterator.
//
// WriteTable covers the flush path, where the whole memtable is known in
// advance. Compaction is the other shape: it merges an open-ended stream and
// has to decide mid-stream when to close one output and start the next, which
// needs the file's current size and the ability to finish it on demand.
type FileWriter struct {
	w      *Writer
	f      vfs.File
	fsys   vfs.FS
	dir    string
	path   string
	number uint64
	done   bool
}

// Create opens dir/%06d.sst for a new table. The file must not already
// exist: docs/format.md §1 never reuses a file number, so a collision means
// the caller's allocator is wrong and silently truncating the existing file
// would destroy live data.
func Create(dir string, number uint64, opts WriterOptions) (*FileWriter, error) {
	fsys := vfs.Or(opts.FS)
	path := filepath.Join(dir, fmt.Sprintf("%06d.sst", number))

	f, err := fsys.Create(path)
	if err != nil {
		return nil, fmt.Errorf("sstable: create %s: %w", path, err)
	}

	return &FileWriter{w: newWriter(f, opts), f: f, fsys: fsys, dir: dir, path: path, number: number}, nil
}

// Number is the file number the table is being written as.
func (fw *FileWriter) Number() uint64 { return fw.number }

// Path is the file being written.
func (fw *FileWriter) Path() string { return fw.path }

// Add appends one entry. Entries must arrive in comparator order.
func (fw *FileWriter) Add(e memtable.Entry) error { return fw.w.Add(e) }

// Size reports the bytes written so far, which is what a caller rolling
// output files at a target size compares against.
func (fw *FileWriter) Size() int64 { return fw.w.Size() }

// EntryCount is how many entries the table holds so far.
func (fw *FileWriter) EntryCount() int { return fw.w.EntryCount() }

// Finish completes the table and returns once the file and its directory
// entry are both durable.
//
// The directory fsync is not optional and not deferrable to the end of a
// compaction: a file is not durable until the entry naming it is, and the
// manifest record that commits the compaction claims every output is already
// on disk.
func (fw *FileWriter) Finish() (Info, error) {
	info, err := fw.w.Finish()
	if err != nil {
		_ = fw.f.Close()
		return Info{}, err
	}
	if err := fw.f.Close(); err != nil {
		return Info{}, fmt.Errorf("sstable: close %s: %w", fw.path, err)
	}
	fw.done = true

	if err := syncDir(fw.fsys, fw.dir); err != nil {
		return Info{}, err
	}

	info.Path = fw.path
	return info, nil
}

// Abort closes and removes a table that will never be finished.
//
// Leaving the partial file behind would not corrupt anything -- startup
// sweeps any .sst the manifest does not name -- but it would leave debris
// until the next restart, and a half-written table sitting in the data
// directory is exactly the thing an operator should never have to wonder
// about. Abort after Finish is a no-op, so it is safe to defer.
func (fw *FileWriter) Abort() error {
	if fw.done {
		return nil
	}
	fw.done = true

	_ = fw.f.Close()
	if err := fw.fsys.Remove(fw.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("sstable: remove partial %s: %w", fw.path, err)
	}
	return nil
}
