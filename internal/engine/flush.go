package engine

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/AbishekRaj2007/Strata/internal/manifest"
	"github.com/AbishekRaj2007/Strata/internal/sstable"
)

// flushStep names a point in the flush sequence, for the fault injection hook
// that proves the ordering holds under a crash.
type flushStep int

const (
	// StepBeforeTable is before the SSTable exists at all.
	StepBeforeTable flushStep = iota

	// StepAfterTableSync is the dangerous moment: the SSTable is fsynced but
	// nothing durable references it. A crash here must lose nothing, which
	// requires the WAL to still be on disk.
	StepAfterTableSync

	// StepAfterManifestSync is immediately after the commit point. The data
	// is durable and referenced; the WAL is now redundant.
	StepAfterManifestSync

	// StepAfterWALDelete is the end of a complete flush.
	StepAfterWALDelete
)

// ErrFlushAborted is what a fault injection hook returns to simulate a crash
// at a chosen step. It is not a real failure mode; it exists so a test can
// stop the sequence at an exact instant and inspect what is on disk.
var ErrFlushAborted = errors.New("flush aborted by fault injection")

// Flusher writes immutable memtables to SSTables and commits them.
//
// The ordering it enforces is the durability argument of the whole write
// path, and it is the one thing in this file that must not be rearranged:
//
//  1. Build the SSTable and fsync it, and its directory entry.
//  2. Append ADD_FILE to the manifest and fsync it.
//  3. Only then drop the memtable and delete its WAL.
//
// Step 2 is the commit point. Between steps 1 and 2 the SSTable is durable
// but nothing references it, so it is an orphan that startup will sweep --
// and the WAL is still the only durable record of those writes. Deleting the
// WAL after step 1 but before step 2 looks safe, because the data is "on
// disk"; it is not, and a crash in that window loses acknowledged writes
// permanently. The WAL may only go once its data is durable *and* referenced
// by a durable manifest.
type Flusher struct {
	set *memtableSet
	dir string
	log *manifest.Log
	vs  *manifest.VersionSet

	// onStep, when non-nil, is consulted at each step of the sequence. It
	// exists for tests that need to stop the flush at an exact instant.
	onStep func(flushStep) error

	trigger chan struct{}
	quit    chan struct{}
	wg      sync.WaitGroup

	// flushMu serialises whole flushes. Oldest and Discard are individually
	// safe, but a flush spans both with an SSTable build and two fsyncs in
	// between, and two flushes interleaved there would each pick the same
	// memtable and the second Discard would name one already gone. Flushes
	// are inherently serial anyway -- queue order is what keeps sequence
	// numbers ascending across the tables produced -- so serialising them
	// costs nothing and makes DrainQueue safe to call from anywhere.
	flushMu sync.Mutex

	mu      sync.Mutex
	err     error
	flushed uint64
}

// NewFlusher wires a flusher to a memtable set, a manifest log, and the
// version set the committed files are installed into.
func NewFlusher(set *memtableSet, dir string, log *manifest.Log, vs *manifest.VersionSet) *Flusher {
	return &Flusher{
		set:     set,
		dir:     dir,
		log:     log,
		vs:      vs,
		trigger: make(chan struct{}, 1),
		quit:    make(chan struct{}),
	}
}

// Start runs the flusher in the background until Stop.
func (f *Flusher) Start() {
	f.wg.Add(1)
	go f.run()
}

// Trigger asks the flusher to drain the queue. It never blocks: the channel
// is a one-slot signal, so a trigger arriving while one is already pending is
// redundant and dropped rather than queued.
func (f *Flusher) Trigger() {
	select {
	case f.trigger <- struct{}{}:
	default:
	}
}

// Stop shuts the flusher down and reports the first error it hit.
func (f *Flusher) Stop() error {
	close(f.quit)
	f.wg.Wait()
	return f.Err()
}

// Err reports the first error the background loop hit. A failed fsync is
// unrecoverable, so once this is set the database must be closed and
// restarted rather than limped along.
func (f *Flusher) Err() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.err
}

// Flushed reports how many memtables have been fully committed.
func (f *Flusher) Flushed() uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.flushed
}

func (f *Flusher) run() {
	defer f.wg.Done()

	for {
		if err := f.DrainQueue(); err != nil {
			f.mu.Lock()
			if f.err == nil {
				f.err = err
			}
			f.mu.Unlock()
			return
		}

		select {
		case <-f.quit:
			// Drain what is queued before leaving, so a clean shutdown does
			// not strand memtables that would have to be recovered from
			// their WALs on the next start.
			_ = f.DrainQueue()
			return
		case <-f.trigger:
		}
	}
}

// DrainQueue flushes every immutable memtable currently queued.
func (f *Flusher) DrainQueue() error {
	for {
		done, err := f.FlushOldest()
		if err != nil {
			return err
		}
		if done {
			return nil
		}
	}
}

// FlushOldest flushes the memtable at the front of the immutable queue and
// reports whether the queue was already empty.
func (f *Flusher) FlushOldest() (empty bool, err error) {
	f.flushMu.Lock()
	defer f.flushMu.Unlock()

	sl, ok := f.set.Oldest()
	if !ok {
		return true, nil
	}

	if err := f.step(StepBeforeTable); err != nil {
		return false, err
	}

	// Step 1: the SSTable. WriteTable fsyncs both the file and the directory
	// entry naming it, so on return the table is durable -- but no durable
	// state references it yet.
	number := f.vs.NextFileNumber()
	info, err := sstable.WriteTable(f.dir, number, sl.table.NewIterator())
	if err != nil {
		return false, fmt.Errorf("flush: build sstable %d: %w", number, err)
	}

	if err := f.step(StepAfterTableSync); err != nil {
		return false, err
	}

	// An empty memtable produces a table with no keys, which there is no
	// point recording. Drop the file and retire the slot; the WAL it came
	// with protects nothing.
	if info.EntryCount == 0 {
		if err := os.Remove(info.Path); err != nil {
			return false, fmt.Errorf("flush: remove empty sstable %d: %w", number, err)
		}
		return false, f.retire(sl)
	}

	// Step 2: the commit point. Append fsyncs the manifest, and only when it
	// returns is the table referenced by durable state.
	var edit manifest.VersionEdit
	edit.AddFile(0, &manifest.FileMetadata{
		Number:      number,
		Size:        uint64(info.Size),
		Smallest:    info.SmallestKey,
		Largest:     info.LargestKey,
		SmallestSeq: info.SmallestSeq,
		LargestSeq:  info.LargestSeq,
	})
	// Recording number+1 rather than calling NextFileNumber again: that call
	// allocates, and asking it for a value to write down would burn a file
	// number on every flush. Replay only ever moves the allocator forward,
	// so a value that undershoots a concurrent allocation is safe.
	edit.SetNextFileNumber(number + 1)

	// Every WAL numbered below the slot being retired is now redundant.
	// Recording it in the same record makes the fact durable atomically with
	// the file that made it true.
	edit.SetLogNumber(sl.number + 1)

	if err := f.log.Append(&edit); err != nil {
		return false, err
	}
	if _, err := f.vs.Apply(&edit); err != nil {
		return false, fmt.Errorf("flush: install version: %w", err)
	}

	if err := f.step(StepAfterManifestSync); err != nil {
		return false, err
	}

	// Step 3: the data is durable and referenced, so the WAL may finally go.
	if err := f.retire(sl); err != nil {
		return false, err
	}

	f.mu.Lock()
	f.flushed++
	f.mu.Unlock()

	return false, f.step(StepAfterWALDelete)
}

// retire drops the memtable from the queue and deletes the WAL that protected
// it. Discard closes the writer; the file itself is removed here, which is
// the moment the durability argument above has been waiting for.
func (f *Flusher) retire(sl *slot) error {
	if err := f.set.Discard(sl); err != nil {
		return fmt.Errorf("flush: discard memtable: %w", err)
	}

	path := filepath.Join(f.dir, fmt.Sprintf("%06d.wal", sl.number))
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("flush: remove wal %d: %w", sl.number, err)
	}
	if err := syncDir(f.dir); err != nil {
		return err
	}
	return nil
}

func (f *Flusher) step(s flushStep) error {
	if f.onStep == nil {
		return nil
	}
	return f.onStep(s)
}
