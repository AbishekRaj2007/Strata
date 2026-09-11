package compaction

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AbishekRaj2007/Strata/internal/manifest"
	"github.com/AbishekRaj2007/Strata/internal/memtable"
)

// scheduler wires a scheduler over the fixture's directory.
func (f *fixture) scheduler(opts Options) *Scheduler {
	f.t.Helper()

	return NewScheduler(SchedulerConfig{
		Versions:  f.vs,
		Picker:    NewPicker(opts),
		Executor:  f.executor(opts),
		Committer: f.cm,
	})
}

// flushL0 installs one L0 file the way a memtable flush would, with keys
// spread across the keyspace so L0 files overlap each other.
func (f *fixture) flushL0(index int) *manifest.FileMetadata {
	f.t.Helper()

	var entries []memtable.Entry
	for k := 0; k < 10; k++ {
		entries = append(entries, memtable.Entry{
			Key:      []byte(fmt.Sprintf("key%03d", k)),
			Sequence: uint64(index*100 + k + 1),
			Value:    []byte(strings.Repeat("v", 64)),
		})
	}
	return f.install(0, entries)
}

func TestSchedulerCompactsUntilEveryLevelIsUnderBudget(t *testing.T) {
	f := newFixture(t)
	opts := Options{L0Trigger: 4, BaseLevelBytes: 1 << 20, TargetFileBytes: 1 << 20}

	for i := 0; i < 6; i++ {
		f.flushL0(i)
	}
	if got := f.vs.Current().NumFiles(0); got != 6 {
		t.Fatalf("L0 has %d files, want 6", got)
	}

	s := f.scheduler(opts)
	if err := s.Drain(); err != nil {
		t.Fatalf("drain: %v", err)
	}

	v := f.vs.Current()
	if got := v.NumFiles(0); got != 0 {
		t.Errorf("L0 still holds %d files after draining", got)
	}
	if v.NumFiles(1) == 0 {
		t.Error("nothing reached L1")
	}
	if err := v.CheckInvariants(); err != nil {
		t.Errorf("the drained tree is not valid: %v", err)
	}

	// The data survived.
	for k := 0; k < 10; k++ {
		key := fmt.Sprintf("key%03d", k)
		if _, visible := readLevels(t, f.dir, levelsOf(v), key); !visible {
			t.Errorf("key %q was lost in compaction", key)
		}
	}

	if st := s.Stats(); st.Compactions == 0 || st.BytesWritten == 0 {
		t.Errorf("stats = %+v, want a non-zero compaction count and byte total", st)
	}
}

func levelsOf(v *manifest.Version) [][]*manifest.FileMetadata {
	var out [][]*manifest.FileMetadata
	for level := 0; level < manifest.NumLevels; level++ {
		out = append(out, v.Files(level))
	}
	return out
}

func TestThrottlePassesAnUncongestedWriter(t *testing.T) {
	f := newFixture(t)
	s := f.scheduler(Options{L0Trigger: 4, L0StallThreshold: 12})

	start := time.Now()
	if err := s.Throttle(); err != nil {
		t.Fatalf("throttle: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Millisecond {
		t.Errorf("an empty L0 delayed a writer by %v", elapsed)
	}
	if st := s.Stats(); st.Stalls != 0 || st.SoftDelays != 0 {
		t.Errorf("stats = %+v, want no backpressure recorded", st)
	}
}

func TestThrottleSlowsAWriterInTheSoftBand(t *testing.T) {
	f := newFixture(t)
	opts := Options{L0Trigger: 4, L0StallThreshold: 12}
	s := f.scheduler(opts)

	// Nine L0 files: past the soft threshold of 8, short of the hard 12.
	for i := 0; i < 9; i++ {
		f.flushL0(i)
	}

	if err := s.Throttle(); err != nil {
		t.Fatalf("throttle: %v", err)
	}

	st := s.Stats()
	if st.SoftDelays != 1 {
		t.Errorf("soft delays = %d, want 1", st.SoftDelays)
	}
	if st.SoftDelayDuration <= 0 {
		t.Error("a soft delay was recorded with no duration")
	}
	if st.Stalls != 0 {
		t.Errorf("stalls = %d, want none: a writer short of the hard threshold must not block", st.Stalls)
	}
}

// The done-when condition: a writer exceeding compaction throughput is held
// rather than allowed to grow L0 without bound, and the stall is reported.
func TestThrottleStallsAtTheHardThresholdAndBoundsL0(t *testing.T) {
	f := newFixture(t)
	opts := Options{L0Trigger: 4, L0StallThreshold: 6, BaseLevelBytes: 1 << 20, TargetFileBytes: 1 << 20}
	s := f.scheduler(opts)

	for i := 0; i < 6; i++ {
		f.flushL0(i)
	}
	if got := f.vs.Current().NumFiles(0); got < 6 {
		t.Fatalf("L0 has %d files, want at least the hard threshold", got)
	}

	// A writer arriving now must block. The compactor is started only after
	// the writer is confirmed stuck, so the test proves it was the stall
	// that held it rather than scheduling luck.
	done := make(chan error, 1)
	go func() { done <- s.Throttle() }()

	select {
	case err := <-done:
		t.Fatalf("the writer was admitted at the hard threshold (err %v)", err)
	case <-time.After(50 * time.Millisecond):
	}

	s.Start()
	defer func() { _ = s.Stop() }()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("throttle: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the writer was never released after compaction drained L0")
	}

	if got := f.vs.Current().NumFiles(0); got >= 6 {
		t.Errorf("L0 still holds %d files; the writer was released without the backlog clearing", got)
	}

	st := s.Stats()
	if st.Stalls != 1 {
		t.Errorf("stalls = %d, want 1", st.Stalls)
	}
	if st.StallDuration <= 0 {
		t.Error("a stall was recorded with no duration")
	}
}

// A stalled writer must not hang forever when the database is shutting down.
func TestStoppingReleasesAStalledWriter(t *testing.T) {
	f := newFixture(t)
	opts := Options{L0Trigger: 4, L0StallThreshold: 3}
	s := f.scheduler(opts)

	for i := 0; i < 4; i++ {
		f.flushL0(i)
	}

	done := make(chan error, 1)
	go func() { done <- s.Throttle() }()

	// Give the writer time to reach the wait before shutting down.
	time.Sleep(20 * time.Millisecond)
	s.Start()
	if err := s.Stop(); err != nil && !errors.Is(err, ErrCanceled) {
		t.Fatalf("stop: %v", err)
	}

	select {
	case err := <-done:
		// Either the compactor drained L0 before the stop landed, or the
		// stop released the writer. Both are correct; hanging is not.
		if err != nil && !errors.Is(err, ErrStopped) {
			t.Errorf("throttle returned %v, want nil or ErrStopped", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the writer was never released by shutdown")
	}
}

// Shutdown during a merge must leave the tree exactly as it was: the outputs
// removed, nothing committed.
func TestStopCancelsAMergeInFlight(t *testing.T) {
	f := newFixture(t)
	opts := Options{L0Trigger: 1, TargetFileBytes: 1}

	for i := 0; i < 4; i++ {
		f.flushL0(i)
	}
	before := f.vs.Current()

	cancel := make(chan struct{})
	ex := f.executor(opts)
	ex.Cancel = cancel
	close(cancel)

	c := NewPicker(opts).Pick(before)
	if c == nil {
		t.Fatal("nothing to compact")
	}

	if _, err := ex.Run(c); !errors.Is(err, ErrCanceled) {
		t.Fatalf("run: %v, want ErrCanceled", err)
	}

	if f.vs.Current() != before {
		t.Error("a canceled merge changed the current version")
	}
	if got, want := len(tables(t, f.dir)), before.TotalFiles(); got != want {
		t.Errorf("%d tables on disk, want the %d inputs: the canceled merge left outputs behind", got, want)
	}
}

// Throttle is called from every writer, so its counters must be safe under
// concurrency -- and its fast path must not serialise them.
func TestThrottleIsConcurrencySafe(t *testing.T) {
	f := newFixture(t)
	opts := Options{L0Trigger: 4, L0StallThreshold: 12}
	s := f.scheduler(opts)

	for i := 0; i < 9; i++ {
		f.flushL0(i)
	}

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.Throttle(); err != nil {
				t.Errorf("throttle: %v", err)
			}
		}()
	}
	wg.Wait()

	if st := s.Stats(); st.SoftDelays != 16 {
		t.Errorf("soft delays = %d, want 16", st.SoftDelays)
	}
}
