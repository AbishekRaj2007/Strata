package manifest

import (
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// deletionTracker is the fault-injection layer T4.2's done-when asks for. It
// records which files have been deleted and panics on any read of one.
//
// Panicking rather than returning an error is deliberate: reading a deleted
// file is not a condition to handle, it is proof that the reference counting
// is wrong, and it must fail loudly at the exact goroutine that did it rather
// than surface later as a checksum mismatch somewhere unrelated.
type deletionTracker struct {
	mu      sync.RWMutex
	deleted map[uint64]bool

	reads atomic.Uint64
}

func newDeletionTracker() *deletionTracker {
	return &deletionTracker{deleted: make(map[uint64]bool)}
}

func (d *deletionTracker) delete(number uint64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.deleted[number] = true
}

// read stands in for opening the file and reading a block from it.
func (d *deletionTracker) read(number uint64) {
	d.mu.RLock()
	defer d.mu.RUnlock()

	if d.deleted[number] {
		panic(fmt.Sprintf("read of deleted file %d: a version naming it was still held", number))
	}
	d.reads.Add(1)
}

// TestReadersNeverTouchADeletedFile is T4.2's done-when: compactions running
// while many clients read continuously, with every read of a deleted file
// failing loudly.
//
// The shape that matters is the interleaving. A reader acquires a version,
// then does real work with the files it names -- with a scheduling point in
// the middle, so a compaction has every opportunity to install a replacement
// and delete those files while the reader is still walking them. If Acquire
// had the load-then-increment race, or if Apply released the old version's
// files before the new one took its references, this is where it shows.
func TestReadersNeverTouchADeletedFile(t *testing.T) {
	vs := NewVersionSet()
	tracker := newDeletionTracker()

	// Seed L1 with files spread across the keyspace.
	const seedFiles = 16
	var seed VersionEdit
	for i := 0; i < seedFiles; i++ {
		seed.AddFile(1, &FileMetadata{
			Number:   vs.NextFileNumber(),
			Size:     4096,
			Smallest: []byte(fmt.Sprintf("k%04d-a", i)),
			Largest:  []byte(fmt.Sprintf("k%04d-z", i)),
		})
	}
	if _, err := vs.Apply(&seed); err != nil {
		t.Fatalf("seed: %v", err)
	}

	const (
		readers     = 100
		compactions = 200
	)

	stop := make(chan struct{})
	failures := make(chan string, readers+1)

	var readerWG sync.WaitGroup
	for r := 0; r < readers; r++ {
		readerWG.Add(1)
		go func() {
			defer readerWG.Done()
			defer func() {
				if p := recover(); p != nil {
					failures <- fmt.Sprint(p)
				}
			}()

			for {
				select {
				case <-stop:
					return
				default:
				}

				v := vs.Acquire()
				files := v.Files(1)

				// A scheduling point mid-operation, so a compaction can run
				// entirely between acquiring the version and using it.
				runtimeGosched()

				for _, f := range files {
					tracker.read(f.Number)
				}
				vs.Release(v)
			}
		}()
	}

	var compactorWG sync.WaitGroup
	compactorWG.Add(1)
	go func() {
		defer compactorWG.Done()
		defer func() {
			if p := recover(); p != nil {
				failures <- fmt.Sprint(p)
			}
		}()

		for c := 0; c < compactions; c++ {
			current := vs.Current()
			files := current.Files(1)
			if len(files) == 0 {
				break
			}

			// Replace one file with a fresh one covering the same range,
			// which is the shape of a real compaction output.
			victim := files[c%len(files)]
			var e VersionEdit
			e.DeleteFile(1, victim.Number)
			e.AddFile(1, &FileMetadata{
				Number:   vs.NextFileNumber(),
				Size:     4096,
				Smallest: victim.Smallest,
				Largest:  victim.Largest,
			})
			if _, err := vs.Apply(&e); err != nil {
				failures <- fmt.Sprintf("compaction %d: %v", c, err)
				return
			}

			// Only files that have lost every reference may be deleted. This
			// is the line the whole task defends.
			for _, number := range vs.Obsolete() {
				if vs.IsLive(number) {
					failures <- fmt.Sprintf("file %d reported obsolete while still live", number)
					return
				}
				tracker.delete(number)
			}
		}
	}()

	compactorWG.Wait()
	close(stop)
	readerWG.Wait()
	close(failures)

	for msg := range failures {
		t.Fatal(msg)
	}

	if got := tracker.reads.Load(); got == 0 {
		t.Fatal("no reads were performed; the test proved nothing")
	}
	t.Logf("%d reads across %d compactions with %d readers", tracker.reads.Load(), compactions, readers)

	// Every file still named by the current version must be live, and nothing
	// else should be holding references once the readers have finished.
	current := vs.Current()
	for level := 0; level < NumLevels; level++ {
		for _, f := range current.Files(level) {
			if !vs.IsLive(f.Number) {
				t.Errorf("file %d is named by the current version but not live", f.Number)
			}
		}
	}
}

// TestVersionsAreReleasedNotLeaked checks the other half of the lifetime
// story: a superseded version must actually die once its readers finish, or
// files accumulate forever and the reference counting is a memory leak that
// happens to be safe.
func TestVersionsAreReleasedNotLeaked(t *testing.T) {
	vs := NewVersionSet()

	var seed VersionEdit
	seed.AddFile(0, &FileMetadata{Number: vs.NextFileNumber(), Smallest: []byte("a"), Largest: []byte("b")})
	if _, err := vs.Apply(&seed); err != nil {
		t.Fatalf("seed: %v", err)
	}

	const rounds = 50
	for i := 0; i < rounds; i++ {
		held := vs.Acquire()

		old := vs.Current().Files(0)[0].Number
		var e VersionEdit
		e.DeleteFile(0, old)
		e.AddFile(0, &FileMetadata{Number: vs.NextFileNumber(), Smallest: []byte("a"), Largest: []byte("b")})
		if _, err := vs.Apply(&e); err != nil {
			t.Fatalf("round %d: %v", i, err)
		}

		vs.Release(held)
		vs.Obsolete()
	}

	// Exactly one file should remain referenced: the one the current version
	// names. Anything more means superseded versions never died.
	live := 0
	for n := uint64(1); n <= uint64(rounds)+2; n++ {
		if vs.IsLive(n) {
			live++
		}
	}
	if live != 1 {
		t.Errorf("%d files still live, want 1: superseded versions were never released", live)
	}
}

func TestAcquireUnderConcurrentInstalls(t *testing.T) {
	vs := NewVersionSet()

	var wg sync.WaitGroup
	done := make(chan struct{})

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ; i++ {
			select {
			case <-done:
				return
			default:
			}
			var e VersionEdit
			e.AddFile(0, &FileMetadata{
				Number:   vs.NextFileNumber(),
				Smallest: []byte(fmt.Sprintf("k%06d", i)),
				Largest:  []byte(fmt.Sprintf("k%06d", i)),
			})
			if _, err := vs.Apply(&e); err != nil {
				return
			}
		}
	}()

	deadline := time.After(200 * time.Millisecond)
	for {
		select {
		case <-deadline:
			close(done)
			wg.Wait()
			return
		default:
		}
		v := vs.Acquire()
		if vs.VersionRefs(v) < 1 {
			t.Fatal("acquired a version with no references")
		}
		vs.Release(v)
	}
}

// runtimeGosched is a named wrapper so the reason for the scheduling point is
// documented at the call site rather than looking like a stray yield.
func runtimeGosched() { runtime.Gosched() }
