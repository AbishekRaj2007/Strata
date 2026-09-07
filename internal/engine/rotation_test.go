package engine

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AbishekRaj2007/Strata/internal/memtable"
	"github.com/AbishekRaj2007/Strata/internal/wal"
)

func newTestTable() memtable.Memtable { return memtable.NewSkipList() }

// perEntryOverhead mirrors internal/memtable's own per-node overhead constant
// (unexported, so it cannot be imported directly) so that thresholds sized
// against it behave the same way here as they do against the real skip list.
const perEntryOverhead = 48

// newTestSet builds a memtableSet in a temporary directory with a monotonic
// file numbering hook, mirroring the allocator the manifest owns from T4.1.
func newTestSet(t *testing.T, threshold, maxImmutable int) *memtableSet {
	t.Helper()

	var counter atomic.Uint64
	s, err := newMemtableSet(RotationConfig{
		Dir:            t.TempDir(),
		Threshold:      threshold,
		MaxImmutable:   maxImmutable,
		New:            newTestTable,
		NextFileNumber: func() uint64 { return counter.Add(1) },
	})
	if err != nil {
		t.Fatalf("newMemtableSet: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestDefaultsApplied(t *testing.T) {
	var counter atomic.Uint64
	s, err := newMemtableSet(RotationConfig{
		Dir:            t.TempDir(),
		New:            newTestTable,
		NextFileNumber: func() uint64 { return counter.Add(1) },
	})
	if err != nil {
		t.Fatalf("newMemtableSet: %v", err)
	}
	defer s.Close()

	if got := s.cfg.Threshold; got != DefaultMemtableThreshold {
		t.Errorf("threshold = %d, want %d", got, DefaultMemtableThreshold)
	}
	if got := s.cfg.MaxImmutable; got != DefaultMaxImmutable {
		t.Errorf("maxImmutable = %d, want %d", got, DefaultMaxImmutable)
	}
}

func TestRequiredDependencies(t *testing.T) {
	dir := t.TempDir()
	if _, err := newMemtableSet(RotationConfig{Dir: dir, NextFileNumber: func() uint64 { return 1 }}); err == nil {
		t.Error("missing New: want an error")
	}
	if _, err := newMemtableSet(RotationConfig{Dir: dir, New: newTestTable}); err == nil {
		t.Error("missing NextFileNumber: want an error")
	}
}

func TestRotatesAtThreshold(t *testing.T) {
	// One entry per memtable: threshold is below the cost of a single entry,
	// so every write after the first finds the table already over budget.
	s := newTestSet(t, perEntryOverhead, 8)

	for i := 0; i < 4; i++ {
		if _, _, err := s.Add([]byte(fmt.Sprintf("k%d", i)), []byte("v"), false); err != nil {
			t.Fatalf("Add %d: %v", i, err)
		}
	}

	st := s.Stats()
	if st.Immutable != 3 {
		t.Errorf("immutable queue = %d, want 3 (one rotation per write after the first)", st.Immutable)
	}
	if st.Sequence != 4 {
		t.Errorf("sequence = %d, want 4", st.Sequence)
	}
}

func TestNoRotationBelowThreshold(t *testing.T) {
	s := newTestSet(t, 1<<20, 2)

	for i := 0; i < 50; i++ {
		if _, _, err := s.Add([]byte(fmt.Sprintf("k%d", i)), []byte("v"), false); err != nil {
			t.Fatalf("Add %d: %v", i, err)
		}
	}

	if st := s.Stats(); st.Immutable != 0 {
		t.Errorf("immutable queue = %d, want 0; nothing reached the threshold", st.Immutable)
	}
}

func TestEachRotationOpensANewWALFile(t *testing.T) {
	s := newTestSet(t, perEntryOverhead, 8)

	for i := 0; i < 3; i++ {
		if _, _, err := s.Add([]byte(fmt.Sprintf("k%d", i)), []byte("v"), false); err != nil {
			t.Fatalf("Add %d: %v", i, err)
		}
	}

	names, err := filepath.Glob(filepath.Join(s.cfg.Dir, "*.wal"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(names) != 3 {
		t.Fatalf("wal files = %v, want 3 distinct files", names)
	}

	// Distinct numbers, and every slot references a different file.
	seen := map[uint64]bool{}
	for _, sl := range append([]*slot{s.active}, s.immutable...) {
		if seen[sl.number] {
			t.Errorf("file number %d used by two slots", sl.number)
		}
		seen[sl.number] = true
	}
}

// TestReadsSeeNewestVersionAcrossRotation covers the read-path ordering plan.md
// §6 specifies: active memtable first, then immutables newest to oldest.
func TestReadsSeeNewestVersionAcrossRotation(t *testing.T) {
	s := newTestSet(t, perEntryOverhead, 8)

	if _, _, err := s.Add([]byte("shared"), []byte("old"), false); err != nil {
		t.Fatalf("Add old: %v", err)
	}
	if _, _, err := s.Add([]byte("only-immutable"), []byte("kept"), false); err != nil {
		t.Fatalf("Add kept: %v", err)
	}
	if _, _, err := s.Add([]byte("shared"), []byte("new"), false); err != nil {
		t.Fatalf("Add new: %v", err)
	}

	if st := s.Stats(); st.Immutable == 0 {
		t.Fatal("expected at least one rotation for this test to mean anything")
	}

	e, ok := s.Get([]byte("shared"))
	if !ok {
		t.Fatal("shared: not found")
	}
	if string(e.Value) != "new" {
		t.Errorf("shared = %q, want %q; an older memtable shadowed a newer one", e.Value, "new")
	}

	e, ok = s.Get([]byte("only-immutable"))
	if !ok {
		t.Fatal("only-immutable: not found; a rotated memtable stopped being read")
	}
	if string(e.Value) != "kept" {
		t.Errorf("only-immutable = %q, want %q", e.Value, "kept")
	}

	if _, ok := s.Get([]byte("absent")); ok {
		t.Error("absent key reported as found")
	}
}

// TestTombstoneIsAVersionNotAnAbsence pins the semantics the read path below
// this layer depends on: a delete must shadow an older value, so it has to
// come back as a found entry rather than as "not found", or a lower level's
// stale value would win.
func TestTombstoneIsAVersionNotAnAbsence(t *testing.T) {
	s := newTestSet(t, perEntryOverhead, 8)

	if _, _, err := s.Add([]byte("k"), []byte("v"), false); err != nil {
		t.Fatalf("Add: %v", err)
	}
	if _, _, err := s.Add([]byte("k"), nil, true); err != nil {
		t.Fatalf("Add tombstone: %v", err)
	}

	e, ok := s.Get([]byte("k"))
	if !ok {
		t.Fatal("deleted key reported as absent; the tombstone was lost")
	}
	if !e.Tombstone {
		t.Errorf("entry = %+v, want a tombstone", e)
	}
}

// TestSequenceRangesAreDisjointAcrossSlots is the memtable half of T3.2's
// trap: rotation must be atomic with respect to sequence assignment. Each
// memtable must hold a contiguous run of sequences, and the runs must ascend
// across the queue with no overlap and no gaps.
//
// The WAL half -- that every record was appended to the WAL belonging to the
// memtable it was inserted into -- is TestEachRecordLandsInItsMemtablesWAL,
// below.
func TestSequenceRangesAreDisjointAcrossSlots(t *testing.T) {
	// No flusher drains the queue here, because the point is to inspect every
	// memtable the workload produced. The bound must therefore exceed the
	// number of rotations, or the writers correctly stall forever waiting for
	// a flusher that this test does not have.
	const writes = 200
	s := newTestSet(t, perEntryOverhead*3, writes)

	for i := 0; i < writes; i++ {
		if _, _, err := s.Add([]byte(fmt.Sprintf("k%03d", i)), []byte("v"), false); err != nil {
			t.Fatalf("Add %d: %v", i, err)
		}
	}

	slots := append(append([]*slot{}, s.immutable...), s.active)

	next := uint64(1)
	for i, sl := range slots {
		var seqs []uint64
		it := sl.table.NewIterator()
		for it.Next() {
			seqs = append(seqs, it.Entry().Sequence)
		}
		if len(seqs) == 0 {
			continue
		}
		sort.Slice(seqs, func(a, b int) bool { return seqs[a] < seqs[b] })

		if seqs[0] != next {
			t.Fatalf("slot %d starts at sequence %d, want %d: rotation lost or duplicated a write", i, seqs[0], next)
		}
		for j := 1; j < len(seqs); j++ {
			if seqs[j] != seqs[j-1]+1 {
				t.Fatalf("slot %d has a gap: %d follows %d", i, seqs[j], seqs[j-1])
			}
		}
		next = seqs[len(seqs)-1] + 1
	}

	if next-1 != writes {
		t.Errorf("highest sequence across slots = %d, want %d", next-1, writes)
	}
}

// TestEachRecordLandsInItsMemtablesWAL is the WAL half of T3.2's trap: a
// write must be durable in the WAL belonging to the memtable it was inserted
// into. If a write landed in a new memtable but the old WAL -- or vice versa
// -- recovery would replay it against the wrong file and reorder it relative
// to writes that followed, silently corrupting the read path. This needs
// T2.3's reader to parse the files back, which is why it could not be
// asserted until T2.3 landed.
func TestEachRecordLandsInItsMemtablesWAL(t *testing.T) {
	const writes = 200
	s := newTestSet(t, perEntryOverhead*3, writes)

	for i := 0; i < writes; i++ {
		if _, _, err := s.Add([]byte(fmt.Sprintf("k%03d", i)), []byte("v"), false); err != nil {
			t.Fatalf("Add %d: %v", i, err)
		}
	}

	slots := append(append([]*slot{}, s.immutable...), s.active)
	if len(slots) < 2 {
		t.Fatalf("need at least two slots for this test to mean anything, have %d", len(slots))
	}

	for i, sl := range slots {
		memtableSeqs := map[uint64]bool{}
		it := sl.table.NewIterator()
		for it.Next() {
			memtableSeqs[it.Entry().Sequence] = true
		}

		if err := sl.wal.Sync(); err != nil {
			t.Fatalf("slot %d: sync: %v", i, err)
		}

		path := filepath.Join(s.cfg.Dir, fmt.Sprintf("%06d.wal", sl.number))
		f, err := os.Open(path)
		if err != nil {
			t.Fatalf("slot %d: open %s: %v", i, path, err)
		}

		walSeqs := map[uint64]bool{}
		r := wal.NewReader(f)
		for {
			b, err := r.Next()
			if err != nil {
				break
			}
			for j := range b.Records {
				walSeqs[b.Sequence+uint64(j)] = true
			}
		}
		_ = f.Close()

		if len(walSeqs) != len(memtableSeqs) {
			t.Fatalf("slot %d: WAL has %d records, memtable has %d", i, len(walSeqs), len(memtableSeqs))
		}
		for seq := range memtableSeqs {
			if !walSeqs[seq] {
				t.Errorf("slot %d: sequence %d is in the memtable but not in its own WAL file", i, seq)
			}
		}
	}
}

// TestBackpressureStallsRatherThanGrowing is T3.2's second done-when
// condition: with the flusher artificially stalled, writers must block and the
// stall must be measured, rather than the queue growing without bound.
func TestBackpressureStallsRatherThanGrowing(t *testing.T) {
	const maxImmutable = 2
	s := newTestSet(t, perEntryOverhead, maxImmutable)

	// Fill the queue. No flusher runs, so nothing drains it.
	for i := 0; i <= maxImmutable; i++ {
		if _, _, err := s.Add([]byte(fmt.Sprintf("fill%d", i)), []byte("v"), false); err != nil {
			t.Fatalf("Add %d: %v", i, err)
		}
	}
	if st := s.Stats(); st.Immutable != maxImmutable {
		t.Fatalf("immutable = %d, want the queue full at %d", st.Immutable, maxImmutable)
	}

	blocked := make(chan error, 1)
	go func() {
		_, _, err := s.Add([]byte("stalled"), []byte("v"), false)
		blocked <- err
	}()

	// The write must not complete while the queue is full.
	select {
	case err := <-blocked:
		t.Fatalf("write completed with a full queue (err=%v); the queue is unbounded", err)
	case <-time.After(100 * time.Millisecond):
	}

	if st := s.Stats(); st.Immutable > maxImmutable {
		t.Errorf("immutable = %d, exceeded the bound of %d", st.Immutable, maxImmutable)
	}

	// Drain one, as a flusher would after its manifest fsync.
	oldest, ok := s.Oldest()
	if !ok {
		t.Fatal("Oldest: queue reported empty while full")
	}
	if err := s.Discard(oldest); err != nil {
		t.Fatalf("Discard: %v", err)
	}

	select {
	case err := <-blocked:
		if err != nil {
			t.Fatalf("stalled write failed after space freed: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("write stayed blocked after the queue drained; the stall never released")
	}

	st := s.Stats()
	if st.Stalls == 0 {
		t.Error("Stalls = 0; backpressure happened but was not measured")
	}
	if st.StallDuration <= 0 {
		t.Error("StallDuration = 0; a stall of at least 100ms was observed")
	}
}

func TestDiscardRejectsOutOfOrder(t *testing.T) {
	s := newTestSet(t, perEntryOverhead, 8)

	for i := 0; i < 3; i++ {
		if _, _, err := s.Add([]byte(fmt.Sprintf("k%d", i)), []byte("v"), false); err != nil {
			t.Fatalf("Add %d: %v", i, err)
		}
	}

	if len(s.immutable) < 2 {
		t.Fatalf("need at least two queued memtables, have %d", len(s.immutable))
	}
	if err := s.Discard(s.immutable[1]); err == nil {
		t.Error("discarding out of order succeeded; flush order must stay FIFO so sequences ascend across SSTables")
	}
}

func TestCloseWakesStalledWriters(t *testing.T) {
	const maxImmutable = 1
	s := newTestSet(t, perEntryOverhead, maxImmutable)

	for i := 0; i <= maxImmutable; i++ {
		if _, _, err := s.Add([]byte(fmt.Sprintf("fill%d", i)), []byte("v"), false); err != nil {
			t.Fatalf("Add %d: %v", i, err)
		}
	}

	blocked := make(chan error, 1)
	go func() {
		_, _, err := s.Add([]byte("stalled"), []byte("v"), false)
		blocked <- err
	}()

	time.Sleep(50 * time.Millisecond)
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	select {
	case err := <-blocked:
		if !errors.Is(err, ErrClosed) {
			t.Errorf("stalled write returned %v, want ErrClosed", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not wake the stalled writer; shutdown would hang")
	}
}

func TestAddAfterCloseReportsClosed(t *testing.T) {
	s := newTestSet(t, 1<<20, 2)
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, _, err := s.Add([]byte("k"), []byte("v"), false); !errors.Is(err, ErrClosed) {
		t.Errorf("Add after Close = %v, want ErrClosed", err)
	}
}

func TestMemtableSetCloseIsIdempotent(t *testing.T) {
	s := newTestSet(t, 1<<20, 2)
	if err := s.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

// TestSustainedWorkloadRotatesWithReadsCorrect is T3.2's first done-when
// condition. Writers, readers and a flusher run concurrently so that rotation
// is forced to overlap with reads rather than merely coexist with them --
// plan.md §20 asks for contrived overlap rather than hoping for a collision.
//
// Run under -race, which is what makes it worth its runtime.
func TestSustainedWorkloadRotatesWithReadsCorrect(t *testing.T) {
	s := newTestSet(t, perEntryOverhead*4, 4)

	const (
		writers        = 4
		writesPerBatch = 250
	)

	// A flusher that only drains the queue. It writes no SSTable, because
	// that is T3.5; here it exists to keep backpressure from deadlocking a
	// test that has no other consumer.
	stop := make(chan struct{})
	var flusherDone sync.WaitGroup
	flusherDone.Add(1)
	go func() {
		defer flusherDone.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if oldest, ok := s.Oldest(); ok {
				_ = s.Discard(oldest)
				continue
			}
			time.Sleep(time.Millisecond)
		}
	}()

	var wg sync.WaitGroup
	errs := make(chan error, writers)

	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < writesPerBatch; i++ {
				key := []byte(fmt.Sprintf("w%d-k%04d", w, i))
				if _, _, err := s.Add(key, []byte("value"), false); err != nil {
					errs <- fmt.Errorf("writer %d: %w", w, err)
					return
				}
			}
		}(w)
	}

	// Readers run throughout. They cannot assert which keys exist, because a
	// flushed memtable legitimately leaves memory -- what they assert is that
	// a read never panics, never races, and never returns a torn entry.
	var readers sync.WaitGroup
	readStop := make(chan struct{})
	for r := 0; r < 4; r++ {
		readers.Add(1)
		go func(r int) {
			defer readers.Done()
			for i := 0; ; i++ {
				select {
				case <-readStop:
					return
				default:
				}
				key := []byte(fmt.Sprintf("w%d-k%04d", i%writers, i%writesPerBatch))
				if e, ok := s.Get(key); ok {
					if len(e.Key) == 0 || e.Sequence == 0 {
						errs <- fmt.Errorf("reader %d: torn entry %+v", r, e)
						return
					}
				}
			}
		}(r)
	}

	wg.Wait()
	close(readStop)
	readers.Wait()
	close(stop)
	flusherDone.Wait()

	select {
	case err := <-errs:
		t.Fatalf("%v", err)
	default:
	}

	st := s.Stats()
	if st.Sequence != writers*writesPerBatch {
		t.Errorf("sequence = %d, want %d: a write was lost or a sequence was reused",
			st.Sequence, writers*writesPerBatch)
	}
	if st.Immutable > st.MaxImmutable {
		t.Errorf("immutable = %d exceeded bound %d", st.Immutable, st.MaxImmutable)
	}
}

// TestWALFileIsCreatedDurably checks the directory entry is synced when a WAL
// is opened. It asserts the file is visible and readable rather than that
// fsync was called, which is not observable from a test; the value here is
// catching an openSlot that forgets the directory entirely.
func TestWALFileIsCreatedDurably(t *testing.T) {
	s := newTestSet(t, 1<<20, 2)

	path := filepath.Join(s.cfg.Dir, fmt.Sprintf("%06d.wal", s.active.number))
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("active WAL %s: %v", path, err)
	}
}
