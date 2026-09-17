package engine

import (
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"github.com/AbishekRaj2007/Strata/internal/memtable"
	"github.com/AbishekRaj2007/Strata/internal/vfs"
	"github.com/AbishekRaj2007/Strata/internal/wal"
)

// Defaults for RotationConfig. The memtable threshold is plan.md T3.2's
// default; the queue bound is deliberately small, because its purpose is to
// convert "flushing has fallen behind" into measured backpressure early rather
// than into an out-of-memory kill later.
const (
	DefaultMemtableThreshold = 4 << 20
	DefaultMaxImmutable      = 2
)

// RotationConfig configures the memtable set.
type RotationConfig struct {
	// Dir is the data directory that holds WAL files.
	Dir string

	// Threshold is the ApproxSize at which the active memtable rotates.
	// Zero means DefaultMemtableThreshold.
	Threshold int

	// MaxImmutable bounds the queue of memtables awaiting flush. Zero means
	// DefaultMaxImmutable. Writers stall once the queue is full.
	MaxImmutable int

	// New allocates an empty memtable. It is a dependency so that the skip
	// list of T3.1 drops in without changing this file.
	New func() memtable.Memtable

	// NextFileNumber allocates the next number in the directory's single
	// monotonic number space, shared by .wal and .sst files (docs/format.md
	// §7.1). It is a dependency because the manifest owns that counter from
	// T4.1 onward.
	NextFileNumber func() uint64

	// SyncPolicy decides whether Add fsyncs the WAL before returning. Under
	// SyncAlways it does, which is what makes an acknowledged write survive
	// kill -9; under the other policies durability is the IntervalSyncer's
	// business and Add only guarantees the bytes reached the OS.
	//
	// The fsync itself runs through a per-slot wal.Syncer (T2.2), called
	// after mu is released, so concurrent writers queue behind one fsync
	// instead of serialising one-per-write. The syncer is bound to the slot
	// rather than the memtableSet because rotation swaps the active slot:
	// binding it once, in openSlot, means a writer that captured its slot
	// before a concurrent rotation still awaits the correct file's syncer.
	SyncPolicy wal.SyncPolicy

	// FS is the filesystem WAL files are created and fsynced through. Nil
	// is the real one.
	FS vfs.FS

	// FirstSequence seeds the sequence counter after recovery, so that
	// sequence numbers continue past everything the replayed WAL contained
	// rather than restarting and colliding with it.
	FirstSequence uint64
}

// slot pairs a memtable with the WAL that protects it.
//
// They are one unit for their whole lifetime: the WAL may only be deleted once
// its memtable's data is durable in an SSTable *and* referenced by a durable
// manifest. Keeping the pair together is what stops a later phase from
// deleting the file a moment early, which loses acknowledged writes.
type slot struct {
	table  memtable.Memtable
	wal    *wal.Writer
	syncer *wal.Syncer
	file   vfs.File
	number uint64

	// inflight counts writers that have captured this slot as the target of
	// an Add and may still be awaiting fsync durability after mu is
	// released. A slot can rotate onto the immutable queue and reach the
	// flusher while such a writer is still mid-fsync against its WAL file;
	// Close and Discard must wait for this to drain before closing that
	// file, or a concurrent Close of the fd could hit an in-flight Sync
	// syscall on it.
	inflight sync.WaitGroup
}

// memtableSet holds the active memtable and the queue of immutable memtables
// awaiting flush, and performs the rotation between them.
//
// Concurrency: mu serialises the entire write path -- sequence assignment, WAL
// append and memtable insert -- rather than only the rotation. That is not
// incidental. Sequence numbers must be appended to a WAL in the order they
// were assigned, because recovery replays a file in offset order; if two
// writers could assign 5 and 6 and then append 6 before 5, replaying that file
// would apply the older write last and silently resurrect it. Amortising the
// resulting fsync cost is the Syncer's job (T2.2), not this lock's.
//
// Readers take the read lock and never block writers for longer than a pointer
// copy.
type memtableSet struct {
	mu   sync.RWMutex
	cond *sync.Cond // signalled when a flush frees queue space

	active *slot

	// immutable is ordered oldest first: the flusher consumes from the front,
	// and reads walk it backwards so that newer versions win.
	immutable []*slot

	seq uint64

	cfg RotationConfig

	closed bool

	// Stall accounting. plan.md T3.2 requires backpressure to be measured
	// rather than merely to happen, and exposing it in INFO now is what lets
	// Phase 6 reuse the mechanism for compaction stalls.
	stalls   uint64
	stallFor time.Duration
}

// newMemtableSet opens the first memtable and its WAL.
func newMemtableSet(cfg RotationConfig) (*memtableSet, error) {
	if cfg.New == nil {
		return nil, fmt.Errorf("rotation: New is required")
	}
	if cfg.NextFileNumber == nil {
		return nil, fmt.Errorf("rotation: NextFileNumber is required")
	}
	if cfg.Threshold <= 0 {
		cfg.Threshold = DefaultMemtableThreshold
	}
	if cfg.MaxImmutable <= 0 {
		cfg.MaxImmutable = DefaultMaxImmutable
	}

	s := &memtableSet{cfg: cfg, seq: cfg.FirstSequence}
	s.cond = sync.NewCond(&s.mu)

	first, err := s.openSlot()
	if err != nil {
		return nil, err
	}
	s.active = first
	return s, nil
}

// openSlot creates a fresh memtable and the WAL file that protects it.
func (s *memtableSet) openSlot() (*slot, error) {
	number := s.cfg.NextFileNumber()
	path := filepath.Join(s.cfg.Dir, fmt.Sprintf("%06d.wal", number))

	f, err := vfs.Or(s.cfg.FS).Create(path)
	if err != nil {
		return nil, fmt.Errorf("rotation: create wal %s: %w", path, err)
	}

	// A file is not durable until the directory entry naming it is durable.
	// Skipping this leaves a crash window in which the WAL protecting
	// acknowledged writes does not exist after a restart.
	if err := syncDir(s.cfg.FS, s.cfg.Dir); err != nil {
		f.Close()
		return nil, err
	}

	w := wal.NewWriter(f)
	return &slot{table: s.cfg.New(), wal: w, syncer: wal.NewSyncer(w, s.cfg.SyncPolicy), file: f, number: number}, nil
}

// syncDir fsyncs a directory so that entries created in it are durable.
func syncDir(fsys vfs.FS, dir string) error {
	if err := vfs.SyncDir(fsys, dir); err != nil {
		return fmt.Errorf("rotation: fsync dir %s: %w", dir, err)
	}
	return nil
}

// Add assigns the next sequence number, appends the write to the active WAL,
// and inserts it into the active memtable, rotating first if the memtable is
// full. It returns the assigned sequence and the WAL offset immediately after
// the record.
//
// The returned offset means the bytes are with the OS, never that they are
// durable: durability is the Syncer's business, and the caller hands this
// offset to it.
func (s *memtableSet) Add(key, value []byte, tombstone bool) (seq uint64, endOffset int64, err error) {
	s.mu.Lock()

	if s.closed {
		s.mu.Unlock()
		return 0, 0, ErrClosed
	}

	// Rotate before the write rather than after, so a value larger than the
	// threshold lands in a memtable of its own instead of being appended to
	// one that is already over budget.
	if s.active.table.ApproxSize() >= s.cfg.Threshold {
		if err := s.rotateLocked(); err != nil {
			s.mu.Unlock()
			return 0, 0, err
		}
	}

	active := s.active
	// Registered while still holding mu, on the slot itself rather than the
	// set: this slot can rotate onto the immutable queue and be handed to
	// the flusher the instant mu is released below, and Discard/Close must
	// know this writer is still using the slot's WAL file even after that.
	active.inflight.Add(1)
	defer active.inflight.Done()

	seq = s.seq + 1

	b := &wal.Batch{Sequence: seq}
	if tombstone {
		b.AppendDelete(key)
	} else {
		b.AppendSet(key, value)
	}

	endOffset, err = active.wal.Write(b)
	if err != nil {
		// The sequence counter is deliberately not advanced: nothing was
		// logged, so the number is still unused and reusing it keeps the WAL's
		// sequences dense.
		s.mu.Unlock()
		return 0, 0, fmt.Errorf("rotation: wal append: %w", err)
	}

	// The record is on the OS now; its sequence number is consumed whether or
	// not the fsync below succeeds, and inserting it into the memtable here
	// (rather than after the fsync) is what lets the fsync itself move
	// outside this lock. That is the point: holding mu across a fsync would
	// serialise every writer one-per-syscall and defeat group commit, which
	// exists precisely to let concurrent writers share one fsync.
	//
	// This does mean a reader can observe the value before AwaitDurable
	// below confirms it durable. That window is safe because a fsync
	// failure is not retried or rolled back piecemeal -- it poisons the
	// whole engine (LSM.guard latches e.fatal on wal.ErrSyncFailed) and
	// every operation after it is refused until restart. The process must
	// then recover from the WAL on disk, whose truncation-tolerant reader
	// (T2.3) already handles a tail that never made it past the page cache.
	e := memtable.Entry{Key: key, Sequence: seq, Value: value, Tombstone: tombstone}
	if err := active.table.Insert(e); err != nil {
		// The record is already in the WAL, so recovery will replay it. The
		// in-memory state is now behind the log, which is not a state this
		// process can reconcile: report it and let the caller close.
		s.mu.Unlock()
		return 0, 0, fmt.Errorf("rotation: memtable insert after wal append: %w", err)
	}

	s.seq = seq
	s.mu.Unlock()

	// Syncer.AwaitDurable no-ops under SyncInterval and SyncNever, so this
	// call is unconditional; the policy check lives in exactly one place.
	if err := active.syncer.AwaitDurable(endOffset); err != nil {
		return 0, 0, fmt.Errorf("rotation: wal sync: %w", err)
	}

	return seq, endOffset, nil
}

// rotateLocked moves the active memtable onto the immutable queue and opens a
// fresh memtable and WAL. The caller holds mu.
//
// Opening both inside the one critical section is the point of this function.
// If a write could land in the new memtable but the old WAL, recovery would
// replay it against the wrong memtable and reorder it relative to writes that
// followed -- silent corruption rather than a detected fault.
func (s *memtableSet) rotateLocked() error {
	if err := s.awaitQueueSpaceLocked(); err != nil {
		return err
	}

	next, err := s.openSlot()
	if err != nil {
		return err
	}

	s.immutable = append(s.immutable, s.active)
	s.active = next
	return nil
}

// awaitQueueSpaceLocked blocks until the immutable queue has room, recording
// how long writers were stalled. The caller holds mu.
//
// Stalling is the designed behaviour: an unbounded queue converts a slow
// flusher into unbounded memory growth, and the process dies with no
// diagnosis. A stall is visible in INFO and survivable.
func (s *memtableSet) awaitQueueSpaceLocked() error {
	if len(s.immutable) < s.cfg.MaxImmutable {
		return nil
	}

	started := time.Now()
	s.stalls++
	for len(s.immutable) >= s.cfg.MaxImmutable {
		if s.closed {
			return ErrClosed
		}
		s.cond.Wait()
	}
	s.stallFor += time.Since(started)
	return nil
}

// Get returns the newest version of key held in memory, searching the active
// memtable and then the immutable queue newest first.
//
// found reports whether any version exists. A tombstone is a version: it is
// returned with Tombstone set rather than as "not found", because the caller
// must stop searching lower levels on a tombstone instead of falling through
// to a stale value in an SSTable.
func (s *memtableSet) Get(key []byte) (e memtable.Entry, found bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if e, ok := s.active.table.Get(key); ok {
		return e, true
	}
	for i := len(s.immutable) - 1; i >= 0; i-- {
		if e, ok := s.immutable[i].table.Get(key); ok {
			return e, true
		}
	}
	return memtable.Entry{}, false
}

// rotate forces the active memtable onto the immutable queue regardless of
// its size, and blocks if the queue is full.
//
// Writes reach the queue on their own once the threshold is crossed; this is
// for the cases where something other than size decides -- an explicit flush
// request, or a clean shutdown that would rather leave an SSTable behind than
// a WAL to replay. An empty active memtable is left alone, since rotating it
// would produce an empty table and a WAL that protects nothing.
func (s *memtableSet) rotate() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return ErrClosed
	}
	if s.active.table.ApproxSize() == 0 {
		return nil
	}
	if err := s.awaitQueueSpaceLocked(); err != nil {
		return err
	}
	return s.rotateLocked()
}

// Oldest returns the memtable at the front of the immutable queue, which is
// the one the flusher must write next, and whether one exists. Flushing in
// queue order keeps sequence numbers ascending across the SSTables produced.
func (s *memtableSet) Oldest() (*slot, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if len(s.immutable) == 0 {
		return nil, false
	}
	return s.immutable[0], true
}

// Discard drops the oldest immutable memtable and frees its queue slot,
// releasing any stalled writers.
//
// It must be called only after that memtable's data is durable in an SSTable
// *and* referenced by a durable manifest -- not merely after the SSTable
// fsync. Calling it earlier opens a window in which a crash loses acknowledged
// writes, because the WAL that protected them is eligible for deletion while
// nothing durable yet references the data. Closing the WAL here does not
// delete the file; that is T3.5's decision to make at the right moment.
func (s *memtableSet) Discard(sl *slot) error {
	s.mu.Lock()

	if len(s.immutable) == 0 || s.immutable[0] != sl {
		s.mu.Unlock()
		return fmt.Errorf("rotation: discard of a memtable that is not the oldest")
	}

	s.immutable = s.immutable[1:]
	s.cond.Broadcast()
	s.mu.Unlock()

	// A writer that captured sl as the active slot before it rotated onto
	// this queue may still be between releasing mu and its own
	// AwaitDurable returning; wait for it before closing the fd it is
	// syncing.
	sl.inflight.Wait()

	return sl.wal.Close()
}

// RotationStats reports the memtable set's state for INFO.
type RotationStats struct {
	// ActiveSize is the active memtable's approximate heap cost in bytes.
	ActiveSize int

	// Immutable is the number of memtables awaiting flush.
	Immutable int

	// MaxImmutable is the queue bound, so a reader can see how close
	// Immutable is to stalling writers.
	MaxImmutable int

	// Stalls counts how many times a writer waited for queue space, and
	// StallDuration totals that waiting. Both are cumulative since startup.
	Stalls        uint64
	StallDuration time.Duration

	// Sequence is the highest sequence number assigned.
	Sequence uint64
}

// Stats reports the current state of the memtable set.
func (s *memtableSet) Stats() RotationStats {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return RotationStats{
		ActiveSize:    s.active.table.ApproxSize(),
		Immutable:     len(s.immutable),
		MaxImmutable:  s.cfg.MaxImmutable,
		Stalls:        s.stalls,
		StallDuration: s.stallFor,
		Sequence:      s.seq,
	}
}

// Close releases the active and queued WAL writers and wakes any stalled
// writer so it can observe the closure rather than block forever.
func (s *memtableSet) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.cond.Broadcast()
	slots := append([]*slot{s.active}, s.immutable...)
	s.mu.Unlock()

	// Once closed is true under mu, s.active can no longer change (rotation
	// only happens inside Add/rotate, both of which check closed first), so
	// the slots captured above are the complete, final set.
	var firstErr error
	for _, sl := range slots {
		// Symmetric with Discard: a writer that captured this slot before
		// Close set closed=true may still be mid-fsync against its WAL file
		// after releasing mu, so wait for it before closing the fd.
		sl.inflight.Wait()
		if err := sl.wal.Close(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// iterators returns an iterator over every live memtable, newest first: the
// active memtable, then the immutable queue from newest to oldest.
//
// The order is the same one Get walks, so a merge over these resolves a tie
// between two sources the way a point lookup would.
func (s *memtableSet) iterators() []memtable.Iterator {
	s.mu.RLock()
	defer s.mu.RUnlock()

	its := make([]memtable.Iterator, 0, len(s.immutable)+1)
	its = append(its, s.active.table.NewIterator())
	for i := len(s.immutable) - 1; i >= 0; i-- {
		its = append(its, s.immutable[i].table.NewIterator())
	}
	return its
}

// syncActive fsyncs the WAL protecting the active memtable.
//
// Recovery needs this: it rewrites replayed data into a fresh WAL and then
// deletes the WALs it replayed, and those deletes are only safe once the new
// copy is durable. It is the same rule the flusher obeys before removing a
// WAL, applied to the same situation -- data existing in exactly one place
// that is about to be removed.
func (s *memtableSet) syncActive() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return ErrClosed
	}
	return s.active.wal.Sync()
}
