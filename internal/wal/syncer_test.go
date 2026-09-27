package wal

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeTarget stands in for the WAL writer of T2.1, recording exactly which
// offsets each Sync made durable so a test can prove a writer was never
// released early.
type fakeTarget struct {
	mu sync.Mutex
	// offset is the bytes handed to the OS so far.
	offset int64
	// durable is the highest offset a completed Sync actually covered.
	durable int64
	// syncDelay stretches the sync window so writers reliably arrive mid-sync.
	syncDelay time.Duration
	// onSync runs inside Sync, for tests that need to write during one.
	onSync func()
	// failWith, when set, makes Sync fail.
	failWith error

	syncs atomic.Int64
}

func (f *fakeTarget) Offset() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.offset
}

// write advances the offset, as the WAL writer does when it appends a record.
func (f *fakeTarget) write(n int64) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.offset += n
	return f.offset
}

func (f *fakeTarget) Sync() error {
	f.syncs.Add(1)

	f.mu.Lock()
	covered := f.offset
	delay, hook, fail := f.syncDelay, f.onSync, f.failWith
	f.mu.Unlock()

	if hook != nil {
		hook()
	}
	if delay > 0 {
		time.Sleep(delay)
	}
	if fail != nil {
		return fail
	}

	// Only bytes present when the sync began are on media afterwards. Anything
	// written during the sync is explicitly not covered, which is what makes
	// the falsely-signalled-writer bug detectable below.
	f.mu.Lock()
	defer f.mu.Unlock()
	if covered > f.durable {
		f.durable = covered
	}
	return nil
}

func (f *fakeTarget) durableOffset() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.durable
}

// TruncateTo implements Syncable. Real callers only ever pass durableOffset's
// value, so this only needs to make Offset agree afterwards.
func (f *fakeTarget) TruncateTo(offset int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.offset = offset
	return nil
}

// TestNoWriterIsSignalledBeforeItsRecordIsSynced is the test plan.md T2.2
// requires by name. The falsely-signalled-writer bug -- releasing a writer
// that arrived mid-sync, whose bytes the in-flight fsync never covered -- is
// invisible to throughput tests and silently breaks the durability guarantee.
//
// The fake records which offsets each Sync genuinely covered, so any writer
// released above that watermark is caught here rather than in production.
func TestNoWriterIsSignalledBeforeItsRecordIsSynced(t *testing.T) {
	target := &fakeTarget{syncDelay: 5 * time.Millisecond}
	s := NewSyncer(target, SyncAlways)
	defer func() { _ = s.Close() }()

	const writers = 64

	var wg sync.WaitGroup
	violations := make(chan string, writers)

	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()

			// Stagger arrivals so many land while a sync is in flight, which
			// is the window the bug lives in.
			time.Sleep(time.Duration(i%8) * time.Millisecond)

			end := target.write(128)
			if err := s.AwaitDurable(end); err != nil {
				violations <- "AwaitDurable returned " + err.Error()
				return
			}

			// The contract: on return, these bytes are on media. The fake
			// knows the truth independently of the syncer's bookkeeping.
			if got := target.durableOffset(); got < end {
				violations <- "writer released at offset " +
					itoa(end) + " but only " + itoa(got) + " is durable"
			}
		}(i)
	}

	wg.Wait()
	close(violations)

	for v := range violations {
		t.Error(v)
	}
}

// TestWriterArrivingDuringSyncWaitsForTheNext covers the boundary condition
// directly: a writer whose bytes land after the leader captured its offset
// must not be released by that sync, and therefore must see a second one.
func TestWriterArrivingDuringSyncWaitsForTheNext(t *testing.T) {
	arrived := make(chan struct{})
	proceed := make(chan struct{})

	target := &fakeTarget{}
	var once sync.Once
	target.onSync = func() {
		// Only the first sync opens the window, so the late writer's own sync
		// does not recurse into this hook.
		once.Do(func() {
			close(arrived)
			<-proceed
		})
	}

	s := NewSyncer(target, SyncAlways)
	defer func() { _ = s.Close() }()

	// The leader: writes, then syncs, blocking inside Sync at the hook.
	leaderEnd := target.write(100)
	leaderDone := make(chan error, 1)
	go func() { leaderDone <- s.AwaitDurable(leaderEnd) }()

	<-arrived

	// The late writer's bytes land after the leader captured its offset, so
	// the in-flight fsync cannot cover them.
	lateEnd := target.write(100)
	lateDone := make(chan error, 1)
	go func() { lateDone <- s.AwaitDurable(lateEnd) }()

	// Give the late writer time to queue before the first sync completes.
	time.Sleep(20 * time.Millisecond)
	close(proceed)

	if err := <-leaderDone; err != nil {
		t.Fatalf("leader: %v", err)
	}
	select {
	case err := <-lateDone:
		if err != nil {
			t.Fatalf("late writer: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("late writer never returned; it is waiting for a sync that will not come")
	}

	if got := target.durableOffset(); got < lateEnd {
		t.Errorf("late writer released at %d with only %d durable", lateEnd, got)
	}
	// Two syncs are required: one that covered the leader, and a second that
	// covered the writer who arrived too late for it.
	if n := target.syncs.Load(); n < 2 {
		t.Errorf("syncs = %d, want at least 2; the late writer was released by the first sync", n)
	}
}

// TestGroupCommitBatchesConcurrentWriters is the throughput property: many
// concurrent writers must cost far fewer than one fsync each.
func TestGroupCommitBatchesConcurrentWriters(t *testing.T) {
	target := &fakeTarget{syncDelay: 2 * time.Millisecond}
	s := NewSyncer(target, SyncAlways)
	defer func() { _ = s.Close() }()

	const writers = 128

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			end := target.write(64)
			if err := s.AwaitDurable(end); err != nil {
				t.Errorf("AwaitDurable: %v", err)
			}
		}()
	}

	close(start)
	wg.Wait()

	syncs := target.syncs.Load()
	if syncs >= writers {
		t.Errorf("syncs = %d for %d writers; group commit is not batching", syncs, writers)
	}
	if got := s.Stats().Batched; got == 0 {
		t.Error("Stats().Batched = 0; no writer was released by another's sync")
	}
	t.Logf("%d writers cost %d fsyncs", writers, syncs)
}

// TestFailedSyncIsUnrecoverable covers the durability invariant from CLAUDE.md:
// a failed fsync leaves the data in an unknowable state, so it is latched and
// reported to every caller rather than retried into a false success.
func TestFailedSyncIsUnrecoverable(t *testing.T) {
	target := &fakeTarget{failWith: errors.New("disk on fire")}
	s := NewSyncer(target, SyncAlways)
	defer func() { _ = s.Close() }()

	end := target.write(10)
	if err := s.AwaitDurable(end); err == nil {
		t.Fatal("AwaitDurable = nil after a failed sync, want an error")
	}

	// The failure must persist: a later writer must not be told it succeeded
	// just because it did not personally observe the failing sync.
	if err := s.AwaitDurable(target.write(10)); err == nil {
		t.Error("a later writer saw no error after an earlier sync failed")
	}
}

// TestNonDurablePoliciesDoNotBlock confirms interval and never acknowledge
// without waiting, which is the trade they exist to make.
func TestNonDurablePoliciesDoNotBlock(t *testing.T) {
	for _, p := range []SyncPolicy{SyncInterval, SyncNever} {
		t.Run(p.String(), func(t *testing.T) {
			target := &fakeTarget{syncDelay: time.Second}
			s := NewSyncer(target, p)
			defer func() { _ = s.Close() }()

			done := make(chan error, 1)
			go func() { done <- s.AwaitDurable(target.write(10)) }()

			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("AwaitDurable: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("AwaitDurable blocked under a non-durable policy")
			}

			if n := target.syncs.Load(); n != 0 {
				t.Errorf("syncs = %d, want 0: %s must not sync on the write path", n, p)
			}
		})
	}
}

func TestAcknowledgementIsDurableOnlyForAlways(t *testing.T) {
	if !SyncAlways.AcknowledgementIsDurable() {
		t.Error("SyncAlways must promise durability at acknowledgement")
	}
	for _, p := range []SyncPolicy{SyncInterval, SyncNever} {
		if p.AcknowledgementIsDurable() {
			t.Errorf("%s must not promise durability at acknowledgement", p)
		}
	}
}

func TestParseSyncPolicy(t *testing.T) {
	tests := []struct {
		in      string
		want    SyncPolicy
		wantErr bool
	}{
		{in: "always", want: SyncAlways},
		{in: "interval", want: SyncInterval},
		{in: "never", want: SyncNever},
		{in: "Always", wantErr: true},
		{in: "", wantErr: true},
		{in: "fsync", wantErr: true},
	}

	for _, tt := range tests {
		got, err := ParseSyncPolicy(tt.in)
		if tt.wantErr {
			if err == nil {
				t.Errorf("ParseSyncPolicy(%q) = %v, want an error", tt.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseSyncPolicy(%q): %v", tt.in, err)
			continue
		}
		if got != tt.want {
			t.Errorf("ParseSyncPolicy(%q) = %v, want %v", tt.in, got, tt.want)
		}
	}
}

func TestIntervalSyncerSyncsOnTimer(t *testing.T) {
	target := &fakeTarget{}
	s := NewSyncer(target, SyncInterval)
	defer func() { _ = s.Close() }()

	i := NewIntervalSyncer(s, 5*time.Millisecond)
	i.Start()

	target.write(100)
	time.Sleep(50 * time.Millisecond)
	i.Stop()

	if n := target.syncs.Load(); n == 0 {
		t.Error("interval syncer never synced")
	}
	if err := i.Err(); err != nil {
		t.Errorf("interval syncer stopped early: %v", err)
	}
}

func TestSyncerCloseReleasesWaiters(t *testing.T) {
	target := &fakeTarget{syncDelay: 50 * time.Millisecond}
	s := NewSyncer(target, SyncAlways)

	// One writer becomes leader and blocks in Sync; a second queues behind it.
	go func() { _ = s.AwaitDurable(target.write(10)) }()
	time.Sleep(10 * time.Millisecond)

	done := make(chan error, 1)
	go func() { done <- s.AwaitDurable(target.write(1 << 20)) }()
	time.Sleep(10 * time.Millisecond)

	_ = s.Close()

	select {
	case <-done:
		// Either error or nil is acceptable; not returning is not.
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not release a queued writer")
	}
}
