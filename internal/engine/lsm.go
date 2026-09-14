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
	"sync/atomic"

	"github.com/AbishekRaj2007/Strata/internal/cache"
	"github.com/AbishekRaj2007/Strata/internal/compaction"
	"github.com/AbishekRaj2007/Strata/internal/log"
	"github.com/AbishekRaj2007/Strata/internal/manifest"
	"github.com/AbishekRaj2007/Strata/internal/memtable"
	"github.com/AbishekRaj2007/Strata/internal/sstable"
	"github.com/AbishekRaj2007/Strata/internal/vfs"
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

	// BlockCacheBytes bounds the shared SSTable block cache. Zero selects
	// cache.DefaultCapacityBytes; a negative value disables the cache
	// entirely, which is what the tuning study's baseline run needs.
	BlockCacheBytes int64

	// BitsPerKey sizes the bloom filter on every table this engine writes.
	// Zero selects bloom.DefaultBitsPerKey.
	BitsPerKey int

	// BlockSize is the target SSTable data block size. Zero selects the
	// docs/format.md §3.2 default.
	BlockSize int

	// Compaction shapes the tree and the backpressure thresholds. The zero
	// value is the default geometry; T6.6's amplification sweep is the
	// reason the multiplier is a parameter rather than a constant.
	Compaction compaction.Options

	// Logger receives one line per committed compaction at debug level.
	// Nil selects a discarding logger.
	Logger log.Logger

	// FS is the filesystem every durable operation goes through. Nil is the
	// real one. It is an option rather than a global so that T7.2's sweep
	// can fail one engine's I/O without touching another's, which is what
	// lets the sweep run its cases in the same process.
	FS vfs.FS
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
	dir       string
	fs        vfs.FS
	vs        *manifest.VersionSet
	log       *manifest.Log
	set       *memtableSet
	flusher   *Flusher
	compactor *compaction.Scheduler

	// compactionOpts is kept so Stats can report each level's target
	// alongside its actual size.
	compactionOpts compaction.Options

	// userBytes and gets are the two denominators of the amplification
	// figures: what the workload asked for, against what the disk did.
	userBytes atomic.Uint64
	gets      atomic.Uint64

	// logicalLive is the last measurement of live logical bytes, taken by
	// Compact over a settled tree. measured guards it against being read as
	// a real zero.
	amplMu      sync.Mutex
	logicalLive uint64
	measured    bool
	tables      tableReader
	cache       *cache.Cache

	policy wal.SyncPolicy

	mu     sync.Mutex
	closed bool

	// fatal latches the first unrecoverable failure. Once set, every
	// operation refuses rather than pretending the engine is healthy.
	fatal error
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
	fsys := vfs.Or(opts.FS)
	if err := fsys.MkdirAll(opts.Dir, 0o700); err != nil {
		return nil, fmt.Errorf("engine: create %s: %w", opts.Dir, err)
	}

	// A negative capacity means "no cache", which the tuning study needs as
	// its baseline. A nil *cache.Cache is usable, so nothing downstream has
	// to branch on it.
	var blocks *cache.Cache
	if opts.BlockCacheBytes >= 0 {
		blocks = cache.New(opts.BlockCacheBytes)
	}

	vs, log, err := openManifest(fsys, opts.Dir)
	if err != nil {
		return nil, err
	}

	if _, err := manifest.SweepOrphans(fsys, opts.Dir, vs.Current()); err != nil {
		_ = log.Close()
		return nil, err
	}

	// Replay the surviving WALs before opening the new one, so that the
	// replayed writes keep the sequence numbers they were acknowledged with
	// and the new WAL gets a number above every file replay touched.
	replayed := memtable.NewSkipList()
	highest, err := wal.Recover(fsys, opts.Dir, func(seq uint64, rec wal.Record) error {
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

	// The WALs that existed before this run opened its own. They are the
	// ones replay just consumed, and they must be deleted afterwards -- see
	// retireReplayedWALs.
	consumed, err := existingWALs(fsys, opts.Dir)
	if err != nil {
		_ = log.Close()
		return nil, err
	}

	set, err := newMemtableSet(RotationConfig{
		Dir:            opts.Dir,
		Threshold:      opts.Threshold,
		MaxImmutable:   opts.MaxImmutable,
		New:            func() memtable.Memtable { return memtable.NewSkipList() },
		NextFileNumber: vs.NextFileNumber,
		SyncPolicy:     opts.SyncPolicy,
		FirstSequence:  highest,
		FS:             fsys,
	})
	if err != nil {
		_ = log.Close()
		return nil, err
	}

	e := &LSM{
		dir:     opts.Dir,
		fs:      fsys,
		vs:      vs,
		log:     log,
		set:     set,
		tables:  &dirTables{dir: opts.Dir, cache: blocks, fs: fsys},
		cache:   blocks,
		policy:  opts.SyncPolicy,
		flusher: nil,
	}
	tableOpts := sstable.WriterOptions{
		BitsPerKey: opts.BitsPerKey,
		BlockSize:  opts.BlockSize,
		FS:         fsys,
	}

	e.flusher = NewFlusher(set, opts.Dir, log, vs)
	e.flusher.SetBlockCache(blocks)
	e.flusher.SetTableOptions(tableOpts)

	// Compaction shares the flusher's table parameters, so a file's read
	// cost does not depend on which of them produced it, and the same block
	// cache, so an input it reads is served from blocks a reader may already
	// have paid for.
	e.compactor = compaction.NewScheduler(compaction.SchedulerConfig{
		Versions: vs,
		Picker:   compaction.NewPicker(opts.Compaction),
		Executor: &compaction.Executor{
			Dir:            opts.Dir,
			Opts:           opts.Compaction,
			TableOpts:      tableOpts,
			Cache:          blocks,
			FS:             fsys,
			NextFileNumber: vs.NextFileNumber,
		},
		Committer: &compaction.Committer{
			Dir:      opts.Dir,
			Log:      log,
			Versions: vs,
			Cache:    blocks,
			FS:       fsys,
		},
		Logger: opts.Logger,
	})
	e.compactionOpts = opts.Compaction

	// A flush is the only thing that adds to L0, so it is the only event
	// that can put L0 over its trigger.
	e.flusher.SetOnFlush(e.compactor.Trigger)

	// Any flush failure poisons the engine immediately, not just the
	// flusher: a partially flushed memtable leaves an orphan table only a
	// fresh Open's sweep can account for, and a write accepted after that
	// point would be durable in a WAL a dead flusher will never drain.
	e.flusher.SetOnError(func(err error) {
		e.mu.Lock()
		if e.fatal == nil {
			e.fatal = fmt.Errorf("%w: %w", ErrUnrecoverable, err)
		}
		e.mu.Unlock()
	})

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
	if err := e.retireReplayedWALs(consumed); err != nil {
		_ = set.Close()
		_ = log.Close()
		return nil, err
	}

	e.flusher.Start()
	e.compactor.Start()
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
func highestFileNumber(fsys vfs.FS, dir string) (uint64, error) {
	entries, err := fsys.ReadDir(dir)
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
func openManifest(fsys vfs.FS, dir string) (*manifest.VersionSet, *manifest.Log, error) {
	_, err := manifest.ReadCurrent(fsys, dir)
	switch {
	case err == nil:
		vs, err := manifest.Recover(fsys, dir)
		if err != nil {
			return nil, nil, err
		}

		// Advance the allocator past everything on disk before handing out
		// the new manifest's number.
		highest, err := highestFileNumber(fsys, dir)
		if err != nil {
			return nil, nil, err
		}
		vs.SetNextFileNumber(highest + 1)
		// Append to a new manifest rather than the replayed one. The old file
		// stays as it is, so a failure here leaves the previous CURRENT
		// pointing at a manifest that is still complete and readable.
		log, err := manifest.CreateLog(fsys, dir, vs.NextFileNumber())
		if err != nil {
			return nil, nil, err
		}
		if err := seedManifest(log, vs); err != nil {
			return nil, nil, err
		}
		if err := manifest.WriteCurrent(fsys, dir, log.Name()); err != nil {
			_ = log.Close()
			return nil, nil, err
		}
		return vs, log, nil

	case errors.Is(err, os.ErrNotExist):
		// No CURRENT does not mean no files. A previous open that failed
		// between creating its manifest and writing CURRENT leaves a
		// MANIFEST behind that this branch would otherwise try to create
		// again -- exclusive creation then fails, and the directory can
		// never be opened again. Advancing past what is on disk is the same
		// rule the recovery branch follows, and for the same reason:
		// docs/format.md §1 forbids reusing a file number, and the manifest
		// is not the authority on which numbers exist.
		vs := manifest.NewVersionSet()
		highest, err := highestFileNumber(fsys, dir)
		if err != nil {
			return nil, nil, err
		}
		vs.SetNextFileNumber(highest + 1)

		log, err := manifest.CreateLog(fsys, dir, vs.NextFileNumber())
		if err != nil {
			return nil, nil, err
		}
		if err := manifest.WriteCurrent(fsys, dir, log.Name()); err != nil {
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
//
// Only the newest version of each key is reloaded, and that is a correctness
// requirement rather than an optimisation. Add assigns fresh, ascending
// sequence numbers, but the replay iterator yields versions of a key newest
// first -- so reloading all of them would hand the oldest version the highest
// new sequence and invert the order. A key deleted and then rewritten would
// come back deleted.
//
// Keeping just the newest version sidesteps that entirely: older versions are
// shadowed and unobservable anyway, and the surviving one keeps its meaning,
// tombstone included. The recovered entries land above every sequence in a
// committed table, which is right, because the flusher only deletes a WAL
// after the data it protected was committed.
func (e *LSM) reloadReplayed(replayed *memtable.SkipList) error {
	var previous []byte
	first := true

	it := replayed.NewIterator()
	for it.Next() {
		entry := it.Entry()
		if !first && bytes.Equal(entry.Key, previous) {
			continue // an older version of a key already reloaded
		}
		previous = append(previous[:0], entry.Key...)
		first = false

		if _, _, err := e.set.Add(entry.Key, entry.Value, entry.Tombstone); err != nil {
			return fmt.Errorf("engine: reloading recovered write: %w", err)
		}
	}
	return nil
}

// existingWALs lists the WAL files already in dir.
func existingWALs(fsys vfs.FS, dir string) ([]string, error) {
	entries, err := fsys.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("engine: read dir %s: %w", dir, err)
	}

	var names []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".wal") {
			names = append(names, entry.Name())
		}
	}
	return names, nil
}

// retireReplayedWALs deletes the WALs whose contents were just rewritten into
// the current one.
//
// Without this, replay is not idempotent across restarts. The recovered
// writes are re-logged into the new WAL, but the old file stays on disk and
// is replayed again on the *next* open -- reintroducing writes that have
// since been superseded or deleted. A key deleted after one restart comes
// back after the next, because the original PUT is still sitting in a WAL
// nobody removed.
//
// The new WAL is fsynced first. These files are the only other copy of the
// data until that returns, so deleting them any earlier is the same mistake
// the flusher is careful not to make with the manifest.
func (e *LSM) retireReplayedWALs(consumed []string) error {
	if len(consumed) == 0 {
		return nil
	}

	if err := e.set.syncActive(); err != nil {
		return fmt.Errorf("engine: sync recovered writes: %w", err)
	}

	for _, name := range consumed {
		if err := e.fs.Remove(filepath.Join(e.dir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("engine: remove replayed wal %s: %w", name, err)
		}
	}
	return syncDir(e.fs, e.dir)
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

	// Backpressure is applied before the write is accepted, never after.
	// Slowing a writer that has already been acknowledged protects nothing.
	if err := e.compactor.Throttle(); err != nil {
		return err
	}

	e.userBytes.Add(uint64(len(key) + len(value)))

	if _, _, err := e.set.Add(key, value, false); err != nil {
		return e.guard(err)
	}
	// This write's own durability does not depend on the flusher: under
	// SyncAlways, e.set.Add has already synced it. A flush failure -- this
	// one's trigger or an earlier one still latched -- poisons every write
	// that follows through e.fatal, but it must not retroactively fail a
	// write that already made it to stable storage on its own.
	e.flusher.Trigger()
	return nil
}

// Get returns the value stored under key, or ErrNotFound.
func (e *LSM) Get(key []byte) ([]byte, error) {
	if err := ValidateKey(key); err != nil {
		return nil, err
	}
	e.gets.Add(1)
	if err := e.checkOpen(); err != nil {
		return nil, err
	}

	// The memtables are consulted before the version is acquired, for the
	// same reason Scan does it: a flush between acquiring a version and
	// reading the memtables would leave the data in neither, because the
	// memtable is gone and the table it became is named only by a newer
	// version. Reading memtables first can only see a key twice, and the
	// first match wins anyway.
	if entry, found := e.set.Get(key); found {
		if entry.Tombstone {
			return nil, ErrNotFound
		}
		return append([]byte(nil), entry.Value...), nil
	}

	// Acquire pins the version for the rest of the lookup, so a concurrent
	// flush cannot delete a file between choosing it and opening it.
	v := e.vs.Acquire()
	defer e.vs.Release(v)

	entry, found, err := lookup(nil, v, e.tables, key)
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

	if err := e.compactor.Throttle(); err != nil {
		return false, err
	}

	// A delete writes a key and no value, and costs the disk a record all
	// the same. Counting it keeps write amplification honest for a
	// delete-heavy workload.
	e.userBytes.Add(uint64(len(key)))

	if _, _, err := e.set.Add(key, nil, true); err != nil {
		return false, e.guard(err)
	}
	// See Put: this tombstone's own durability does not depend on the
	// flusher, so a flush failure must not retroactively fail it.
	e.flusher.Trigger()
	return existed, nil
}

// Stats reports engine state for INFO.
func (e *LSM) Stats() (Stats, error) {
	if err := e.checkOpen(); err != nil {
		return Stats{}, err
	}

	rotation := e.set.Stats()
	cs := e.compactor.Stats()
	ampl := e.Amplification()
	st := Stats{
		SyncPolicy: e.policy.String(),
		Memtable:   &rotation,
		Compaction: &cs,
		Levels:     levelStats(e.vs.Current(), e.compactionOpts),
		Ampl:       &ampl,
	}
	if e.cache != nil {
		cs := e.cache.Stats()
		st.BlockCache = &cs
	}
	return st, nil
}

// levelStats summarises the tree's shape. Levels with no files are omitted:
// an LSM is sparse by design, and printing four empty levels buries the two
// that hold data.
func levelStats(v *manifest.Version, opts compaction.Options) []LevelStats {
	var out []LevelStats
	for level := 0; level < manifest.NumLevels; level++ {
		if v.NumFiles(level) == 0 {
			continue
		}
		out = append(out, LevelStats{
			Level:       level,
			Files:       v.NumFiles(level),
			Bytes:       v.LevelBytes(level),
			TargetBytes: opts.TargetBytes(level),
		})
	}
	return out
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

	// The compactor stops second, so the tables that final drain produced
	// are still eligible for compaction while it runs. It cancels a merge in
	// flight rather than waiting it out; nothing was committed, so the tree
	// is left exactly as the last commit left it.
	if err := e.compactor.Stop(); err != nil && !errors.Is(err, compaction.ErrCanceled) && firstErr == nil {
		firstErr = err
	}

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
	// The fatal check comes after the closed check so that a poisoned
	// database that has been closed reports the ordinary thing.
	return e.fatal
}

// guard latches an unrecoverable failure and returns err unchanged.
//
// A failed fsync is the only one today. It is latched here rather than at the
// point it happens because the write path returns it through several layers,
// and the property that matters is not where it was noticed but that no
// operation after it is allowed to succeed. Anything else lets the next write
// be acknowledged on the strength of an fsync that already failed.
func (e *LSM) guard(err error) error {
	if err == nil || !errors.Is(err, wal.ErrSyncFailed) {
		return err
	}

	e.mu.Lock()
	if e.fatal == nil {
		e.fatal = fmt.Errorf("%w: %w", ErrUnrecoverable, err)
	}
	e.mu.Unlock()
	return err
}

// scanSources assembles every iterator the scan must merge, newest first.
//
// Order matters even though the merge sorts by the comparator: two sources
// can hold the same key at the same sequence only if something has gone
// wrong, and the merge breaks that tie by source index. Listing newest first
// means the tie resolves the same way the Get path would.
func (e *LSM) scanSources(sources []memtable.Iterator, v *manifest.Version) ([]memtable.Iterator, []*sstable.Table, error) {
	var opened []*sstable.Table
	closeAll := func() {
		for _, t := range opened {
			_ = t.Close()
		}
	}

	for level := 0; level < manifest.NumLevels; level++ {
		for _, fm := range v.Files(level) {
			tbl, err := sstable.OpenWith(filepath.Join(e.dir, fm.Name()), sstable.OpenOptions{
				Number: fm.Number,
				Cache:  e.cache,
			})
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

	// The memtables are snapshotted before the version is acquired, and the
	// order is load-bearing. A flush moves data out of a memtable and into a
	// table named by a *newer* version; taking the version first leaves a
	// window where the memtable is already gone and the table is not yet in
	// the version we hold, so the data is in neither and the scan silently
	// drops it. Taking the memtables first can only ever double-count across
	// that window, and the merge deduplicates. Missing is fatal, duplicated
	// is free.
	sources := e.set.iterators()

	v := e.vs.Acquire()
	defer e.vs.Release(v)

	sources, opened, err := e.scanSources(sources, v)
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

// Flush forces the active memtable out to an SSTable and waits for it.
//
// Nothing in the write path needs this -- rotation happens on size and the
// background flusher drains the queue on its own. It exists for the cases
// that want the boundary to be observable: a test that needs data on disk at
// a known point, and the FLUSH command.
func (e *LSM) Flush() error {
	if err := e.checkOpen(); err != nil {
		return err
	}
	if err := e.set.rotate(); err != nil {
		return e.guard(err)
	}
	if err := e.flusher.DrainQueue(); err != nil {
		return e.guard(err)
	}

	// Draining compaction too is what makes FLUSH mean "the tree is
	// settled" rather than "the memtable is on disk". T6.6 depends on it:
	// space amplification measured before compaction has quiesced measures
	// the backlog, not the steady state.
	return e.compactor.Drain()
}

// measureLive records the live logical byte count of the settled tree, which
// is the denominator of space amplification. It must be called only after a
// drain.
func (e *LSM) measureLive() error {
	logical, err := measureLogicalBytes(e.fs, e.dir, e.vs.Current())
	if err != nil {
		return err
	}

	e.amplMu.Lock()
	e.logicalLive, e.measured = logical, true
	e.amplMu.Unlock()
	return nil
}

// Amplification reports the three costs the leveled layout trades against
// each other. The space figure is from the last Compact; the other two are
// cumulative and always current.
func (e *LSM) Amplification() Amplification {
	cs := e.compactor.Stats()

	e.amplMu.Lock()
	logical, measured := e.logicalLive, e.measured
	e.amplMu.Unlock()

	return Amplification{
		UserBytesWritten:       e.userBytes.Load(),
		FlushBytesWritten:      e.flusher.BytesWritten(),
		CompactionBytesWritten: cs.BytesWritten,
		Gets:                   e.gets.Load(),
		TableReads:             e.tables.Reads(),
		DiskBytesLive:          diskBytes(e.vs.Current()),
		LogicalBytesLive:       logical,
		LogicalMeasured:        measured,
	}
}

// diskBytes is the total size of every table the version names.
func diskBytes(v *manifest.Version) uint64 {
	var total uint64
	for level := 0; level < manifest.NumLevels; level++ {
		total += v.LevelBytes(level)
	}
	return total
}

// Compact forces the tree to settle: the memtable is flushed and every level
// compacted until none is over budget.
//
// It is the same work as Flush, and deliberately so -- settling the tree
// requires the memtable on disk first, since data still in memory is data
// compaction cannot see. The two names exist because the callers mean
// different things: FLUSHDB asks for durability at a known point, COMPACT
// asks for a steady state to measure.
func (e *LSM) Compact() error {
	if err := e.Flush(); err != nil {
		return err
	}
	// The tree is settled, which is the only point at which live logical
	// bytes can be counted by a single top-down walk.
	return e.measureLive()
}
