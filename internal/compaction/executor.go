package compaction

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/AbishekRaj2007/Strata/internal/cache"
	"github.com/AbishekRaj2007/Strata/internal/manifest"
	"github.com/AbishekRaj2007/Strata/internal/memtable"
	"github.com/AbishekRaj2007/Strata/internal/sstable"
)

// Executor merges a compaction's inputs into new output tables.
//
// It does not commit anything. Running a compaction and committing it are
// separate steps because they have different failure semantics: a merge that
// fails leaves files nothing references, which startup sweeps, while a commit
// that fails is the one place in the system where the two-state rule has to
// hold exactly. Keeping them apart means the merge can be tested without a
// manifest and the commit can be tested without merging anything.
type Executor struct {
	// Dir is the data directory the inputs live in and the outputs go to.
	Dir string

	// Opts supplies TargetFileBytes, the size an output rolls at.
	Opts Options

	// TableOpts are the bloom and block parameters for the output tables. It
	// must match what the flusher writes, or a table's read cost would
	// depend on which level produced it.
	TableOpts sstable.WriterOptions

	// Cache is the shared block cache, used for reading inputs. Nil is
	// usable.
	Cache *cache.Cache

	// NextFileNumber allocates output file numbers from the same monotonic
	// sequence everything else in the directory uses.
	NextFileNumber func() uint64

	// Cancel, when non-nil, aborts a merge in flight once it is closed. A
	// canceled merge removes its outputs and commits nothing, so the tree is
	// left exactly as it was -- which is what lets shutdown interrupt a long
	// compaction instead of waiting it out. Nil never cancels.
	Cancel <-chan struct{}
}

// Result is what a completed merge produced, before anything references it.
type Result struct {
	// Outputs are the new tables, in key order, ready to be added to the
	// output level.
	Outputs []*manifest.FileMetadata

	// BytesRead is the total size of the input files, and BytesWritten the
	// total size of the outputs. Their ratio over a run is write
	// amplification, which T6.6 reports.
	BytesRead    uint64
	BytesWritten uint64

	// Keys is the number of distinct user keys the merge surfaced, and
	// EntriesWritten how many of them reached an output. They differ by
	// TombstonesDropped.
	//
	// Neither counts the superseded versions the merge discarded: the merge
	// iterator collapses them internally and never surfaces them, so the
	// count is not available here. Reclaimed garbage is measured in bytes --
	// BytesRead against BytesWritten -- which is what amplification is
	// expressed in anyway.
	Keys              uint64
	EntriesWritten    uint64
	TombstonesDropped uint64
}

// Run merges c's inputs and returns the outputs it wrote.
//
// Every output is fully durable on return -- file and directory entry both
// fsynced -- and referenced by nothing. That is the intended state: until the
// manifest record lands, a crash here leaves orphans that startup deletes,
// and every input is still live and still correct.
//
// On any failure the partial outputs are removed rather than left for the
// sweep. Both are safe; removing them means an operator never finds a
// half-written table in the data directory and has to reason about it.
func (e *Executor) Run(c *Compaction) (res *Result, err error) {
	opts := e.Opts.withDefaults()

	inputs, err := e.openInputs(c)
	if err != nil {
		return nil, err
	}
	defer func() {
		for _, t := range inputs {
			_ = t.Close()
		}
	}()

	res = &Result{}
	for _, f := range c.Inputs() {
		res.BytesRead += f.Size
	}

	var (
		sources []memtable.Iterator
		out     *sstable.FileWriter
		written []*sstable.FileWriter
		lastKey []byte
	)
	for _, t := range inputs {
		sources = append(sources, t.NewIterator())
	}

	// Any failure past this point abandons every output, finished or not.
	// A compaction is all-or-nothing: half its outputs installed alongside
	// all of its inputs would double-count every key they share.
	defer func() {
		if err == nil {
			return
		}
		if out != nil {
			_ = out.Abort()
		}
		for _, w := range written {
			_ = removeTable(e.Dir, w.Number())
		}
		res = nil
	}()

	// skipTombstones stays false here even when the compaction is
	// bottom-most: the merge would happily drop them, but dropping them in
	// this loop keeps the rule at the one place a reader looks for it, and
	// makes the count reportable.
	merge := sstable.NewMergeIterator(sources, false)

	for merge.Next() {
		entry := merge.Entry()
		res.Keys++

		// Checked per key rather than per block: the deferred cleanup above
		// turns a cancel into "nothing happened" whatever point it lands on,
		// so the only cost of checking often is the check itself.
		if canceled(e.Cancel) {
			err = ErrCanceled
			return nil, err
		}

		// The tombstone rule, and the whole of T6.2's trap. A tombstone may
		// go only when nothing below the output level could still hold the
		// key it deletes -- which is what BottomMost reports. Drop it any
		// earlier and the older version underneath becomes visible again:
		// the key the user deleted comes back, silently, possibly long after
		// the compaction that did it.
		//
		// There is no snapshot clause because there are no snapshots: the
		// engine exposes no read that observes a sequence older than the
		// current one, so the oldest sequence any reader can require is the
		// newest one written. If snapshots are ever added, this condition
		// gains "and no live snapshot predates entry.Sequence".
		if entry.Tombstone && c.BottomMost {
			res.TombstonesDropped++
			continue
		}

		// Roll before writing, never after, so the decision is made against
		// a file that is already at its target rather than one about to
		// exceed it. Rolling only on a key change is belt and braces: the
		// merge yields one version per user key, so consecutive entries
		// never share one -- but if that ever changed, splitting a key
		// across two files at the same level would break the non-overlap
		// invariant the read path depends on below L0.
		if out != nil && out.Size() >= int64(opts.TargetFileBytes) && !bytes.Equal(entry.Key, lastKey) {
			meta, err := finishOutput(out)
			if err != nil {
				return nil, err
			}
			res.Outputs = append(res.Outputs, meta)
			res.BytesWritten += meta.Size
			written = append(written, out)
			out = nil
		}

		if out == nil {
			if out, err = sstable.Create(e.Dir, e.NextFileNumber(), e.TableOpts); err != nil {
				return nil, err
			}
		}

		if err = out.Add(entry); err != nil {
			return nil, fmt.Errorf("compaction: write entry %q: %w", entry.Key, err)
		}
		lastKey = append(lastKey[:0], entry.Key...)
		res.EntriesWritten++
	}

	if err = merge.Err(); err != nil {
		return nil, fmt.Errorf("compaction: merge inputs: %w", err)
	}

	if out != nil {
		// A compaction whose every entry was a dropped tombstone produces an
		// empty table, which there is nothing to record and no point keeping.
		if out.EntryCount() == 0 {
			err = out.Abort()
			return res, err
		}

		meta, ferr := finishOutput(out)
		if ferr != nil {
			err = ferr
			return nil, err
		}
		res.Outputs = append(res.Outputs, meta)
		res.BytesWritten += meta.Size
		out = nil
	}

	return res, nil
}

// openInputs opens every table the compaction reads, in the order the merge
// should see them. Order does not affect correctness -- the comparator
// resolves versions by sequence, not by source -- but keeping base before
// parent makes a trace of the merge readable.
func (e *Executor) openInputs(c *Compaction) ([]*sstable.Table, error) {
	var opened []*sstable.Table

	for _, f := range c.Inputs() {
		t, err := sstable.OpenWith(filepath.Join(e.Dir, f.Name()), sstable.OpenOptions{
			Number: f.Number,
			Cache:  e.Cache,
		})
		if err != nil {
			for _, prev := range opened {
				_ = prev.Close()
			}
			return nil, fmt.Errorf("compaction: open input %d: %w", f.Number, err)
		}
		opened = append(opened, t)
	}
	return opened, nil
}

// canceled reports whether a cancel channel has been closed, without
// blocking on one that has not.
func canceled(ch <-chan struct{}) bool {
	if ch == nil {
		return false
	}
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// removeTable deletes an output file that a failed compaction abandoned. A
// file already gone is not an error: nothing references it either way, and
// the only thing that matters is that it is not there afterwards.
func removeTable(dir string, number uint64) error {
	path := filepath.Join(dir, fmt.Sprintf("%06d.sst", number))
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("compaction: remove abandoned output %d: %w", number, err)
	}
	return nil
}

// finishOutput closes a table and returns the metadata the manifest will
// record for it.
func finishOutput(w *sstable.FileWriter) (*manifest.FileMetadata, error) {
	number := w.Number()

	info, err := w.Finish()
	if err != nil {
		return nil, fmt.Errorf("compaction: finish output %d: %w", number, err)
	}

	return &manifest.FileMetadata{
		Number:      number,
		Size:        uint64(info.Size),
		Smallest:    info.SmallestKey,
		Largest:     info.LargestKey,
		SmallestSeq: info.SmallestSeq,
		LargestSeq:  info.LargestSeq,
	}, nil
}
