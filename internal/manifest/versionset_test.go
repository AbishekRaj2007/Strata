package manifest

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

func TestVersionSetStartsEmpty(t *testing.T) {
	vs := NewVersionSet()
	if got := vs.Current().TotalFiles(); got != 0 {
		t.Errorf("TotalFiles = %d, want 0", got)
	}
	if got := vs.NextFileNumber(); got != 1 {
		t.Errorf("first file number = %d, want 1; zero is reserved", got)
	}
	if got := vs.NextFileNumber(); got != 2 {
		t.Errorf("second file number = %d, want 2", got)
	}
}

func TestVersionSetInstallsNewVersion(t *testing.T) {
	vs := NewVersionSet()
	before := vs.Current()

	var e VersionEdit
	e.AddFile(0, meta(1, "a", "c"))

	installed, err := vs.Apply(&e)
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if vs.Current() != installed {
		t.Error("Apply returned a version it did not install")
	}
	if before.TotalFiles() != 0 {
		t.Error("the previously current version was mutated")
	}
	if vs.Current().TotalFiles() != 1 {
		t.Errorf("TotalFiles = %d, want 1", vs.Current().TotalFiles())
	}
}

// TestFailedApplyLeavesCurrentAlone matters because an invariant violation
// must not be half-installed. The version in place has to stay usable.
func TestFailedApplyLeavesCurrentAlone(t *testing.T) {
	vs := NewVersionSet()

	var good VersionEdit
	good.AddFile(1, meta(1, "a", "c"))
	if _, err := vs.Apply(&good); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	before := vs.Current()

	var bad VersionEdit
	bad.AddFile(1, meta(2, "b", "d")) // overlaps file 1 at a level that forbids it

	if _, err := vs.Apply(&bad); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Apply = %v, want an ErrCorrupt", err)
	}
	if vs.Current() != before {
		t.Error("a failed Apply replaced the current version")
	}
	if vs.Current().TotalFiles() != 1 {
		t.Errorf("TotalFiles = %d, want the pre-failure 1", vs.Current().TotalFiles())
	}
}

func TestSetNextFileNumberNeverGoesBackwards(t *testing.T) {
	vs := NewVersionSet()

	vs.SetNextFileNumber(100)
	if got := vs.NextFileNumber(); got != 100 {
		t.Errorf("file number = %d, want 100", got)
	}

	// Replaying an older edit must not rewind the allocator: handing out a
	// number already in use would break §1's no-reuse rule.
	vs.SetNextFileNumber(5)
	if got := vs.NextFileNumber(); got != 101 {
		t.Errorf("file number = %d, want 101; the allocator moved backwards", got)
	}
}

func TestApplyAdvancesTheAllocator(t *testing.T) {
	vs := NewVersionSet()

	var e VersionEdit
	e.SetNextFileNumber(50)
	if _, err := vs.Apply(&e); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	if got := vs.NextFileNumber(); got != 50 {
		t.Errorf("file number = %d, want 50", got)
	}
}

// TestConcurrentApplyLosesNothing is why installation is not a bare pointer
// swap. Two installers each loading the current version, applying to it, and
// storing the result would have the second discard the first's work.
func TestConcurrentApplyLosesNothing(t *testing.T) {
	vs := NewVersionSet()

	const (
		installers   = 8
		perGoroutine = 25
	)

	var wg sync.WaitGroup
	errs := make(chan error, installers)

	for g := 0; g < installers; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < perGoroutine; i++ {
				// Level 0 tolerates overlap, so concurrent installers do not
				// have to coordinate key ranges to stay valid.
				var e VersionEdit
				e.AddFile(0, &FileMetadata{
					Number:   vs.NextFileNumber(),
					Size:     1024,
					Smallest: []byte(fmt.Sprintf("g%d-%03d", g, i)),
					Largest:  []byte(fmt.Sprintf("g%d-%03d", g, i)),
				})
				if _, err := vs.Apply(&e); err != nil {
					errs <- err
					return
				}
			}
		}(g)
	}

	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent Apply: %v", err)
	}

	if got, want := vs.Current().TotalFiles(), installers*perGoroutine; got != want {
		t.Errorf("TotalFiles = %d, want %d: concurrent installs overwrote each other", got, want)
	}
	if err := vs.Current().CheckInvariants(); err != nil {
		t.Errorf("final version violates invariants: %v", err)
	}
}

// TestConcurrentReadersSeeAConsistentVersion checks the property the pointer
// swap exists to provide: a reader holding a version sees a file list that
// does not change under it, however many installs happen meanwhile.
func TestConcurrentReadersSeeAConsistentVersion(t *testing.T) {
	vs := NewVersionSet()

	stop := make(chan struct{})
	var writers sync.WaitGroup
	writers.Add(1)
	go func() {
		defer writers.Done()
		for i := 0; ; i++ {
			select {
			case <-stop:
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

	var readers sync.WaitGroup
	bad := make(chan string, 4)
	for r := 0; r < 4; r++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for i := 0; i < 2000; i++ {
				v := vs.Current()
				n := v.NumFiles(0)
				files := v.Files(0)
				if len(files) != n {
					bad <- "file list changed under a held version"
					return
				}
				for _, f := range files {
					if f == nil {
						bad <- "nil file in a held version"
						return
					}
				}
			}
		}()
	}

	readers.Wait()
	close(stop)
	writers.Wait()

	select {
	case msg := <-bad:
		t.Fatal(msg)
	default:
	}
}
