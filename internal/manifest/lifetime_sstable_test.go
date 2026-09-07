package manifest

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/AbishekRaj2007/Strata/internal/memtable"
	"github.com/AbishekRaj2007/Strata/internal/sstable"
)

// sliceIterator feeds a sorted slice to sstable.WriteTable.
type sliceIterator struct {
	entries []memtable.Entry
	pos     int
}

func (it *sliceIterator) Next() bool {
	it.pos++
	return it.pos < len(it.entries)
}

func (it *sliceIterator) Entry() memtable.Entry { return it.entries[it.pos] }

// buildTable writes a real SSTable holding a handful of keys under prefix and
// returns the metadata naming it.
func buildTable(t *testing.T, dir string, number uint64, prefix string) *FileMetadata {
	t.Helper()

	var entries []memtable.Entry
	for i := 0; i < 8; i++ {
		entries = append(entries, memtable.Entry{
			Key:      []byte(fmt.Sprintf("%s-k%02d", prefix, i)),
			Sequence: number,
			Value:    []byte(fmt.Sprintf("value-%d-%d", number, i)),
		})
	}

	info, err := sstable.WriteTable(dir, number, &sliceIterator{entries: entries, pos: -1})
	if err != nil {
		t.Fatalf("WriteTable %d: %v", number, err)
	}
	return &FileMetadata{
		Number:      number,
		Size:        uint64(info.Size),
		Smallest:    info.SmallestKey,
		Largest:     info.LargestKey,
		SmallestSeq: info.SmallestSeq,
		LargestSeq:  info.LargestSeq,
	}
}

// deletionGuard is the fault-injection layer T4.2's done-when calls for. It
// records which files have been unlinked and panics if a read is attempted on
// one.
//
// It is kept alongside real file deletion rather than replacing it, because
// the two catch different things. Unlinking a file is the real consequence,
// but POSIX keeps an already-open file readable after unlink, so a reader
// that opened before the delete would not notice. The guard closes that hole:
// it fails at the exact goroutine that touched a file it had no right to,
// whether or not the kernel would still have served the bytes.
type deletionGuard struct {
	mu      sync.RWMutex
	deleted map[uint64]bool
	reads   atomic.Uint64
}

func newDeletionGuard() *deletionGuard {
	return &deletionGuard{deleted: make(map[uint64]bool)}
}

func (g *deletionGuard) markDeleted(number uint64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.deleted[number] = true
}

func (g *deletionGuard) checkReadable(number uint64) {
	g.mu.RLock()
	defer g.mu.RUnlock()

	if g.deleted[number] {
		panic(fmt.Sprintf("read of deleted file %d: a version naming it was still held", number))
	}
	g.reads.Add(1)
}

// TestReadersNeverTouchADeletedSSTable is T4.2's done-when against real
// tables on disk, which is what the version reading a tracker could not
// prove: the readers here open the actual file, parse its footer and index,
// and verify a data block's checksum. If the reference counting let a file be
// unlinked too early, the open fails with ENOENT and the read that the whole
// task exists to protect comes back as a hard error rather than a metaphor.
func TestReadersNeverTouchADeletedSSTable(t *testing.T) {
	dir := t.TempDir()
	vs := NewVersionSet()
	guard := newDeletionGuard()

	// Seed L1 with real tables spread across the keyspace.
	const seedFiles = 8
	var seed VersionEdit
	for i := 0; i < seedFiles; i++ {
		number := vs.NextFileNumber()
		seed.AddFile(1, buildTable(t, dir, number, fmt.Sprintf("f%04d", i)))
	}
	if _, err := vs.Apply(&seed); err != nil {
		t.Fatalf("seed: %v", err)
	}

	const (
		readers     = 100
		compactions = 100
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

				// A scheduling point between acquiring the version and using
				// it, so a compaction has every chance to install a
				// replacement and delete these files mid-read.
				runtime.Gosched()

				for _, fm := range files {
					guard.checkReadable(fm.Number)

					// The real read. Open re-resolves the path, so an
					// unlinked file fails here rather than being served from
					// a descriptor opened before the delete.
					tbl, err := sstable.Open(filepath.Join(dir, fm.Name()))
					if err != nil {
						failures <- fmt.Sprintf("open file %d while holding a version naming it: %v", fm.Number, err)
						return
					}
					// Touch a data block so the checksum path runs too.
					if _, _, err := tbl.Get(fm.Smallest); err != nil {
						failures <- fmt.Sprintf("read file %d: %v", fm.Number, err)
						_ = tbl.Close()
						return
					}
					_ = tbl.Close()
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

			// Replace one file with a fresh table covering the same range --
			// the shape of a real compaction output.
			victim := files[c%len(files)]
			number := vs.NextFileNumber()
			replacement := buildTable(t, dir, number, string(victim.Smallest[:5]))

			var e VersionEdit
			e.DeleteFile(1, victim.Number)
			e.AddFile(1, replacement)
			if _, err := vs.Apply(&e); err != nil {
				failures <- fmt.Sprintf("compaction %d: %v", c, err)
				return
			}

			// Only files that have lost every reference may leave the disk.
			// This is the line the whole task defends.
			for _, obsolete := range vs.Obsolete() {
				if vs.IsLive(obsolete) {
					failures <- fmt.Sprintf("file %d reported obsolete while still live", obsolete)
					return
				}
				guard.markDeleted(obsolete)
				if err := os.Remove(filepath.Join(dir, fmt.Sprintf("%06d.sst", obsolete))); err != nil {
					failures <- fmt.Sprintf("remove file %d: %v", obsolete, err)
					return
				}
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

	if got := guard.reads.Load(); got == 0 {
		t.Fatal("no reads were performed; the test proved nothing")
	}
	t.Logf("%d reads of real SSTables across %d compactions with %d readers",
		guard.reads.Load(), compactions, readers)

	// Every file the current version names must still be on disk and live.
	current := vs.Current()
	for level := 0; level < NumLevels; level++ {
		for _, fm := range current.Files(level) {
			if !vs.IsLive(fm.Number) {
				t.Errorf("file %d is named by the current version but not live", fm.Number)
			}
			if _, err := os.Stat(filepath.Join(dir, fm.Name())); err != nil {
				t.Errorf("file %d is named by the current version but missing from disk: %v", fm.Number, err)
			}
		}
	}
}
