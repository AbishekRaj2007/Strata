package engine

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/AbishekRaj2007/Strata/internal/manifest"
	"github.com/AbishekRaj2007/Strata/internal/memtable"
	"github.com/AbishekRaj2007/Strata/internal/sstable"
	"github.com/AbishekRaj2007/Strata/internal/wal"
)

// Options configures an LSM engine.
type Options struct {
	// Dir is the data directory. It is created if it does not exist.
	Dir string

	// Threshold is the memtable size that triggers a rotation. Zero means
	// DefaultMemtableThreshold.
	Threshold int

	// MaxImmutable bounds the flush queue. Zero means DefaultMaxImmutable.
	MaxImmutable int

	// SyncPolicy selects the WAL durability mode. The zero value is
	// wal.SyncAlways, so the safe choice is the one you get by not choosing.
	SyncPolicy wal.SyncPolicy
}

// LSM is the durable storage engine: memtables in front of a WAL, flushed
// into SSTables that a manifest tracks.
//
// It is the composition of the pieces built in Phases 2 through 4 rather than
// new machinery of its own -- rotation owns the write path, Flusher owns the
// memtable-to-table transition, manifest owns which files are live, and
// lookup owns the read order. What this type adds is startup: deciding what
// on disk is still true after a crash.
type LSM struct {
	dir     string
	vs      *manifest.VersionSet
	log     *manifest.Log
	set     *memtableSet
	flusher *Flusher
	tables  tableReader

	policy wal.SyncPolicy

	mu     sync.Mutex
	closed bool
}

// Compile-time proof that the durable engine satisfies the public interface.
var _ Engine = (*LSM)(nil)

// Open starts an engine on dir, recovering whatever a previous run left
// behind.
//
// Recovery runs in the order the durability argument requires:
//
//  1. Replay the manifest, which names every SSTable that committed.
//  2. Sweep orphaned tables -- files on disk that no committed version names.
//     They are the debris of a crash between an SSTable fsync and the manifest
//     fsync, and no future version can ever name them because file numbers are
//     never reused.
//  3. Replay every WAL still on disk into a fresh memtable. A WAL that
//     survives is by construction one whose data never reached a committed
//     table: the flusher deletes a WAL only after the manifest fsync that
//     references its data.
//
// Step 3 is why step 2 is safe. An orphaned table is never the only copy of
// anything, because the WAL that protected the same writes is still there.
func Open(opts Options) (*LSM, error) {
	if opts.Dir == "" {
		return nil, fmt.Errorf("engine: Dir is required")
	}
	if err := os.MkdirAll(opts.Dir, 0o700); err != nil {
		return nil, fmt.Errorf("engine: create %s: %w", opts.Dir, err)
	}

	vs, log, err := openManifest(opts.Dir)
	if err != nil {
		return nil, err
	}

	if _, err := manifest.SweepOrphans(opts.Dir, vs.Current()); err != nil {
		_ = log.Close()
		return nil, err
	}

	// Replay the surviving WALs before opening the new one, so that the
	// replayed writes keep the sequence numbers they were acknowledged with
	// and the new WAL gets a number above every file replay touched.
	replayed := memtable.NewSkipList()
	highest, err := wal.Recover(opts.Dir, func(seq uint64, rec wal.Record) error {
		return replayed.Insert(memtable.Entry{
			Key:       rec.Key,
			Sequence:  seq,
			Value:     rec.Value,
			Tombstone: rec.Kind == wal.KindDelete,
		})
	})
	if err != nil {
		_ = log.Close()
		return nil, fmt.Errorf("engine: wal recovery: %w", err)
	}
	if s := vs.Current().LastSequence(); s > highest {
		highest = s
	}

	set, err := newMemtableSet(RotationConfig{
		Dir:            opts.Dir,
		Threshold:      opts.Threshold,
		MaxImmutable:   opts.MaxImmutable,
		New:            func() memtable.Memtable { return memtable.NewSkipList() },
		NextFileNumber: vs.NextFileNumber,
		SyncPolicy:     opts.SyncPolicy,
		FirstSequence:  highest,
	})
	if err != nil {
		_ = log.Close()
		return nil, err
	}

	e := &LSM{
		dir:     opts.Dir,
		vs:      vs,
		log:     log,
		set:     set,
		tables:  &dirTables{dir: opts.Dir},
		policy:  opts.SyncPolicy,
		flusher: nil,
	}
	e.flusher = NewFlusher(set, opts.Dir, log, vs)

	// The replayed state is loaded through the normal write path so it lands
	// in the new WAL too. That costs one rewrite of the recovered data and
	// buys a single code path: without it the first memtable would hold
	// entries its own WAL never recorded, and a second crash before the next
	// flush would lose them.
	if err := e.reloadReplayed(replayed); err != nil {
		_ = set.Close()
		_ = log.Close()
		return nil, err
	}

	e.flusher.Start()
	return e, nil
}

// highestFileNumber returns the largest file number in use anywhere in dir,
// across all three kinds of file that share the number space.
//
// The allocator has to be seeded from this rather than from the manifest
// alone. docs/format.md §1 gives the directory a single monotonic sequence
// and forbids reusing a number within a database's lifetime, but a manifest
// only records numbers that reached a committed edit -- the manifest file's
// own number, and any file written by a run that crashed before committing,
// appear nowhere in it. Trusting the manifest alone hands out a number that
// is already on disk, and the create fails or, worse, succeeds against a
// stale file.
func highestFileNumber(dir string) (uint64, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, fmt.Errorf("engine: read dir %s: %w", dir, err)
	}

	var highest uint64
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		name := entry.Name()
		var digits string
		switch {
		case strings.HasSuffix(name, ".sst"), strings.HasSuffix(name, ".wal"):
			digits = strings.TrimSuffix(strings.TrimSuffix(name, ".sst"), ".wal")
		case strings.HasPrefix(name, "MANIFEST-"):
			digits = strings.TrimPrefix(name, "MANIFEST-")
		default:
			continue
		}

		n, err := strconv.ParseUint(digits, 10, 64)
		if err != nil {
			continue // not a numbered file; CURRENT and friends land here
		}
		if n > highest {
			highest = n
		}
	}
	return highest, nil
}

// openManifest recovers the existing manifest, or lays down a fresh one for a
// directory that has never held a database.
func openManifest(dir string) (*manifest.VersionSet, *manifest.Log, error) {
	_, err := manifest.ReadCurrent(dir)
	switch {
	case err == nil:
		vs, err := manifest.Recover(dir)
		if err != nil {
			return nil, nil, err
		}

		// Advance the allocator past everything on disk before handing out
		// the new manifest's number.
		highest, err := highestFileNumber(dir)
		if err != nil {
			return nil, nil, err
		}
		vs.SetNextFileNumber(highest + 1)
		// Append to a new manifest rather than the replayed one. The old file
		// stays as it is, so a failure here leaves the previous CURRENT
		// pointing at a manifest that is still complete and readable.
		log, err := manifest.CreateLog(dir, vs.NextFileNumber())
		if err != nil {
			return nil, nil, err
		}
		if err := seedManifest(log, vs); err != nil {
			return nil, nil, err
		}
		if err := manifest.WriteCurrent(dir, log.Name()); err != nil {
			_ = log.Close()
			return nil, nil, err
		}
		return vs, log, nil

	case errors.Is(err, os.ErrNotExist):
		vs := manifest.NewVersionSet()
		log, err := manifest.CreateLog(dir, vs.NextFileNumber())
		if err != nil {
			return nil, nil, err
		}
		if err := manifest.WriteCurrent(dir, log.Name()); err != nil {
			_ = log.Close()
			return nil, nil, err
		}
		return vs, log, nil

	default:
		return nil, nil, err
	}
}

// seedManifest writes the recovered state into a new manifest as one record,
// so the new file is self-contained and the old one can be discarded.
func seedManifest(log *manifest.Log, vs *manifest.VersionSet) error {
	v := vs.Current()

	var edit manifest.VersionEdit
	for level := 0; level < manifest.NumLevels; level++ {
		for _, fm := range v.Files(level) {
			edit.AddFile(level, fm)
		}
	}
	edit.SetLastSequence(v.LastSequence())
	edit.SetLogNumber(v.LogNumber())
	edit.SetNextFileNumber(vs.NextFileNumber())

	if err := log.Append(&edit); err != nil {
		_ = log.Close()
		return err
	}
	return nil
}

// reloadReplayed pushes recovered entries back through the write path.
func (e *LSM) reloadReplayed(replayed *memtable.SkipList) error {
	it := replayed.NewIterator()
	for it.Next() {
		entry := it.Entry()
		if _, _, err := e.set.Add(entry.Key, entry.Value, entry.Tombstone); err != nil {
			return fmt.Errorf("engine: reloading recovered write: %w", err)
		}
	}
	return nil
}

// Put stores value under key.
func (e *LSM) Put(key, value []byte) error {
	if err := ValidateKey(key); err != nil {
		return err
	}
	if err := ValidateValue(value); err != nil {
		return err
	}
	if err := e.checkOpen(); err != nil {
		return err
	}

	if _, _, err := e.set.Add(key, value, false); err != nil {
		return err
	}
	e.flusher.Trigger()
	return e.flusher.Err()
}

// Get returns the value stored under key, or ErrNotFound.
func (e *LSM) Get(key []byte) ([]byte, error) {
	if err := ValidateKey(key); err != nil {
		return nil, err
	}
	if err := e.checkOpen(); err != nil {
		return nil, err
	}

	// Acquire pins the version for the whole lookup, so a concurrent flush
	// cannot delete a file between choosing it and opening it.
	v := e.vs.Acquire()
	defer e.vs.Release(v)

	entry, found, err := lookup(e.set, v, e.tables, key)
	if err != nil {
		return nil, err
	}
	if !found || entry.Tombstone {
		return nil, ErrNotFound
	}

	// The caller owns the result: a copy, so nothing it holds aliases a
	// memtable a later write could touch.
	return append([]byte(nil), entry.Value...), nil
}

// Delete removes key, reporting whether a value was actually removed.
func (e *LSM) Delete(key []byte) (bool, error) {
	if err := ValidateKey(key); err != nil {
		return false, err
	}
	if err := e.checkOpen(); err != nil {
		return false, err
	}

	// Whether a value existed is part of DEL's contract, so the lookup
	// happens before the tombstone is written.
	_, err := e.Get(key)
	existed := err == nil
	if err != nil && !errors.Is(err, ErrNotFound) {
		return false, err
	}

	if _, _, err := e.set.Add(key, nil, true); err != nil {
		return false, err
	}
	e.flusher.Trigger()
	return existed, e.flusher.Err()
}

// Stats reports engine state for INFO.
func (e *LSM) Stats() (Stats, error) {
	if err := e.checkOpen(); err != nil {
		return Stats{}, err
	}

	rotation := e.set.Stats()
	return Stats{
		SyncPolicy: e.policy.String(),
		Memtable:   &rotation,
	}, nil
}

// Close stops the flusher, flushes what is queued, and releases everything.
// It is idempotent.
func (e *LSM) Close() error {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return nil
	}
	e.closed = true
	e.mu.Unlock()

	// Stop drains the queue before returning, so a clean shutdown leaves
	// tables behind rather than WALs to replay.
	firstErr := e.flusher.Stop()

	if err := e.set.Close(); err != nil && firstErr == nil {
		firstErr = err
	}
	if err := e.log.Close(); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

func (e *LSM) checkOpen() error {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.closed {
		return ErrClosed
	}
	return nil
}

// scanSources assembles every iterator the scan must merge, newest first.
//
// Order matters even though the merge sorts by the comparator: two sources
// can hold the same key at the same sequence only if something has gone
// wrong, and the merge breaks that tie by source index. Listing newest first
// means the tie resolves the same way the Get path would.
func (e *LSM) scanSources(v *manifest.Version) ([]memtable.Iterator, []*sstable.Table, error) {
	sources := e.set.iterators()

	var opened []*sstable.Table
	closeAll := func() {
		for _, t := range opened {
			_ = t.Close()
		}
	}

	for level := 0; level < manifest.NumLevels; level++ {
		for _, fm := range v.Files(level) {
			tbl, err := sstable.Open(filepath.Join(e.dir, fm.Name()))
			if err != nil {
				closeAll()
				return nil, nil, fmt.Errorf("scan: open table %d: %w", fm.Number, err)
			}
			opened = append(opened, tbl)
			sources = append(sources, tbl.NewIterator())
		}
	}
	return sources, opened, nil
}

// Scan returns up to count keys strictly after cursor, in key order.
//
// The cursor is the last key of the previous page rather than a position, so
// it stays meaningful across a flush or a compaction that rewrites the files
// underneath it -- which is the whole reason plan.md §7.5 specifies it that
// way.
//
// Tombstones are suppressed by the merge, and suppressing a delete also
// suppresses the older versions it shadows, so a deleted key cannot reappear
// in a page from a lower level.
func (e *LSM) Scan(cursor []byte, count int) (ScanResult, error) {
	if err := e.checkOpen(); err != nil {
		return ScanResult{}, err
	}
	if count <= 0 {
		count = 10
	}

	v := e.vs.Acquire()
	defer e.vs.Release(v)

	sources, opened, err := e.scanSources(v)
	if err != nil {
		return ScanResult{}, err
	}
	defer func() {
		for _, t := range opened {
			_ = t.Close()
		}
	}()

	m := sstable.NewMergeIterator(sources, true)

	// Advancing to the cursor by walking rather than by seeking each source.
	// It is the obviously-correct formulation and Phase 4 is establishing
	// correctness; pushing the seek down into the sources is a Phase 5
	// optimisation, and the merge already exposes what it would need.
	page := make([][]byte, 0, count)
	for m.Next() {
		key := m.Entry().Key
		if cursor != nil && bytes.Compare(key, cursor) <= 0 {
			continue
		}
		page = append(page, append([]byte(nil), key...))
		if len(page) == count {
			break
		}
	}
	if err := m.Err(); err != nil {
		return ScanResult{}, err
	}

	// A short page means the iteration reached the end. Only a full page can
	// have more behind it, and its last key is the next cursor.
	var next []byte
	if len(page) == count {
		next = append([]byte(nil), page[len(page)-1]...)
	}
	return ScanResult{Keys: page, Cursor: next}, nil
}
