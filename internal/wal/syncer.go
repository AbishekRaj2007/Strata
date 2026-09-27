package wal

import (
	"errors"
	"fmt"
	"sync"
	"time"
)

// Syncable is the durability target a Syncer coordinates. The WAL writer
// (T2.1) satisfies it; tests substitute a recording fake.
//
// Offset reports how many bytes have been handed to the operating system so
// far. Sync must make everything written before the call durable, and the
// value Offset returned before a Sync is what that Sync is guaranteed to
// cover -- which is the property the whole leader-follower handoff rests on.
type Syncable interface {
	Offset() int64
	Sync() error

	// TruncateTo discards every byte written after offset, which must be a
	// value this target's own Offset() previously returned. It is not
	// called by Syncer itself: leadSync runs without the caller's own
	// write-path lock held, and the target is documented as unsafe for
	// concurrent use, so truncating here could race a concurrent write the
	// same way two unrelated mutexes racing on one field always can.
	// SyncedOffset gives a caller holding that lock the value to pass here
	// once AwaitDurable reports failure.
	TruncateTo(offset int64) error
}

// ErrSyncerClosed reports use of a Syncer after Close.
var ErrSyncerClosed = errors.New("syncer is closed")

// Syncer turns many concurrent writers into few fsyncs.
//
// The problem it solves: at SyncAlways every writer needs its record on media
// before it can be acknowledged, and an fsync per write caps throughput at a
// few thousand operations per second regardless of how many clients are
// waiting. But one fsync makes *everything* written before it durable, not
// just the caller's record. So when writers arrive together, one of them can
// sync on behalf of all, and throughput rises with concurrency instead of
// collapsing.
//
// The design is a leader-follower handoff. Arriving writers queue; the first
// to find no sync in progress becomes leader; the leader records the current
// offset, syncs, and then releases every writer whose bytes were below that
// offset. Followers block until released and never call Sync themselves.
//
// # The boundary condition
//
// A writer that arrives *during* a sync must not be released by that sync. Its
// bytes may have reached the file after the leader read the offset, in which
// case the fsync it is waiting on never covered them. Releasing it anyway
// acknowledges a write that is not on media -- the falsely-signalled-writer
// bug, which silently breaks the durability guarantee and which no throughput
// test detects. Such a writer waits for the next sync.
//
// This is why the leader captures the offset *before* syncing and compares
// each waiter's end offset against that captured value, rather than releasing
// everyone who happens to be queued when the sync returns.
type Syncer struct {
	target Syncable
	policy SyncPolicy

	mu sync.Mutex
	// syncing is true while a leader is inside target.Sync.
	syncing bool
	// synced is the highest offset known to be on media. A writer whose bytes
	// end at or below it is durable.
	synced int64
	// generation increments on every completed sync, so a waiter can tell "a
	// sync finished" from "no sync has happened yet" without comparing times.
	generation uint64
	// cond broadcasts when a sync completes or the syncer closes.
	cond   *sync.Cond
	closed bool
	// syncErr latches a failed sync. A failed fsync is unrecoverable: the
	// bytes may or may not be on media and there is no way to find out, so
	// every later caller is told rather than allowed to assume success.
	syncErr error

	// Counters for the throughput test and for INFO.
	syncCount  uint64
	waitCount  uint64
	groupSizes uint64
}

// NewSyncer returns a Syncer coordinating fsyncs to target under policy.
func NewSyncer(target Syncable, policy SyncPolicy) *Syncer {
	s := &Syncer{target: target, policy: policy}
	s.cond = sync.NewCond(&s.mu)
	return s
}

// Policy reports the configured policy.
func (s *Syncer) Policy() SyncPolicy { return s.policy }

// AwaitDurable blocks until the bytes up to endOffset are on physical media,
// performing the fsync itself if no other writer is already doing so.
//
// It returns only once the caller's own bytes are durable. Under SyncNever and
// SyncInterval it returns immediately, because those policies do not promise
// durability at acknowledgement time.
func (s *Syncer) AwaitDurable(endOffset int64) error {
	if s.policy != SyncAlways {
		// The interval policy's syncing happens on a timer in the background;
		// the writer is not made to wait for it.
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	for {
		if s.closed {
			return ErrSyncerClosed
		}
		if s.syncErr != nil {
			return s.syncErr
		}
		// Already covered by a sync that completed earlier.
		if endOffset <= s.synced {
			return nil
		}

		if s.syncing {
			// A sync is in flight, but it may have captured its offset before
			// these bytes were written, so it cannot be assumed to cover them.
			// Wait for it to finish and re-test against the new synced offset;
			// if it did cover them the loop exits above, and if it did not
			// this writer becomes the next leader.
			s.waitCount++
			s.cond.Wait()
			continue
		}

		return s.leadSync()
	}
}

// leadSync performs one fsync on behalf of every writer queued behind it.
// Called with s.mu held; releases it for the duration of the sync.
func (s *Syncer) leadSync() error {
	// Captured before the sync starts. Bytes written after this point are not
	// covered by this fsync even if their writers are already waiting, which
	// is exactly the case the boundary condition protects.
	covered := s.target.Offset()
	s.syncing = true

	// The fsync runs without the lock so writers can keep queueing; they will
	// find syncing true and wait rather than starting a second sync.
	s.mu.Unlock()
	err := s.target.Sync()
	s.mu.Lock()

	s.syncing = false
	s.syncCount++
	s.groupSizes += s.waitCount
	s.waitCount = 0

	if err != nil {
		// A failed fsync is unrecoverable. Latching it means every writer --
		// waiting or arriving -- is told, rather than some being released on
		// the assumption the sync worked.
		s.syncErr = fmt.Errorf("wal sync: %w: %w", ErrSyncFailed, err)
		s.cond.Broadcast()
		return s.syncErr
	}

	if covered > s.synced {
		s.synced = covered
	}
	s.generation++
	s.cond.Broadcast()
	return nil
}

// SyncNow forces an fsync regardless of policy, for the interval timer and for
// shutdown. It reports the offset made durable.
func (s *Syncer) SyncNow() (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for s.syncing {
		s.cond.Wait()
	}
	if s.closed {
		return s.synced, ErrSyncerClosed
	}
	if s.syncErr != nil {
		return s.synced, s.syncErr
	}
	if err := s.leadSync(); err != nil {
		return s.synced, err
	}
	return s.synced, nil
}

// SyncedOffset reports the highest offset known to be on media.
func (s *Syncer) SyncedOffset() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.synced
}

// SyncStats reports fsync counts, for the throughput test and INFO.
type SyncStats struct {
	// Syncs is the number of fsyncs actually issued.
	Syncs uint64
	// Batched is the number of writers released by another writer's fsync.
	// The ratio of this to Syncs is what group commit buys.
	Batched uint64
}

// Stats returns a snapshot of the sync counters.
func (s *Syncer) Stats() SyncStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return SyncStats{Syncs: s.syncCount, Batched: s.groupSizes}
}

// Close stops the syncer and releases every waiter. It does not sync; callers
// wanting a final flush call SyncNow first.
func (s *Syncer) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return nil
	}
	s.closed = true
	s.cond.Broadcast()
	return nil
}

// IntervalSyncer drives SyncNow on a timer for SyncInterval.
//
// It is separate from Syncer because the interval policy's durability is a
// property of elapsed time rather than of any writer's progress: no caller
// waits on it, and its failures surface on the next write rather than to
// whoever happened to be writing when the timer fired.
type IntervalSyncer struct {
	syncer   *Syncer
	interval time.Duration
	stop     chan struct{}
	done     chan struct{}

	mu      sync.Mutex
	lastErr error
}

// NewIntervalSyncer returns a stopped interval syncer; call Start to run it.
func NewIntervalSyncer(s *Syncer, interval time.Duration) *IntervalSyncer {
	return &IntervalSyncer{
		syncer:   s,
		interval: interval,
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}
}

// Start begins syncing on the interval.
func (i *IntervalSyncer) Start() {
	go func() {
		defer close(i.done)

		t := time.NewTicker(i.interval)
		defer t.Stop()

		for {
			select {
			case <-i.stop:
				return
			case <-t.C:
				if _, err := i.syncer.SyncNow(); err != nil {
					// Recorded rather than logged and dropped: the next writer
					// learns about it through the syncer's latched error, and
					// Err makes it visible to shutdown.
					i.mu.Lock()
					i.lastErr = err
					i.mu.Unlock()
					return
				}
			}
		}
	}()
}

// Err reports the failure that stopped the interval syncer, if any.
func (i *IntervalSyncer) Err() error {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.lastErr
}

// Stop halts the timer and waits for the goroutine to exit.
func (i *IntervalSyncer) Stop() {
	close(i.stop)
	<-i.done
}
