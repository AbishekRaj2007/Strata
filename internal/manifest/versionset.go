package manifest

import (
	"sync"
	"sync/atomic"
)

// VersionSet holds the current version and installs new ones.
//
// Installation is a single atomic pointer store, which is what makes the read
// path free of locks: a reader loads the pointer once and works from the
// version it got, while a compaction concurrently builds and installs a
// replacement. Neither waits for the other.
//
// The mutex serialises installers, not readers. Applying an edit is
// read-modify-write on the current version -- load, Apply, store -- and two
// concurrent compactions doing that without a lock would each build from the
// same base and the second store would silently discard the first's work.
type VersionSet struct {
	current atomic.Pointer[Version]

	// installMu serialises Apply so no edit is lost to a concurrent one.
	installMu sync.Mutex

	// nextFile is the allocator behind docs/format.md §1's single monotonic
	// file-number sequence, shared across .wal, .sst and MANIFEST files.
	nextFile atomic.Uint64
}

// NewVersionSet returns a set holding the empty version.
func NewVersionSet() *VersionSet {
	vs := &VersionSet{}
	vs.current.Store(NewVersion())
	// File numbers start at 1; zero is reserved so that an unset field in a
	// decoded edit is distinguishable from a real file.
	vs.nextFile.Store(1)
	return vs
}

// Current returns the current version.
//
// The returned version is immutable and safe to use for the duration of an
// operation, but on its own it carries no guarantee that the files it names
// still exist on disk -- a compaction may install a replacement and delete
// them. Callers that will open files must use Acquire instead.
func (vs *VersionSet) Current() *Version {
	return vs.current.Load()
}

// NextFileNumber allocates the next file number.
func (vs *VersionSet) NextFileNumber() uint64 {
	return vs.nextFile.Add(1) - 1
}

// SetNextFileNumber advances the allocator to at least n, which replay uses to
// restore it from the manifest. It never moves the counter backwards: a
// smaller value would hand out a number already in use, and
// docs/format.md §1 forbids reusing a number within a database's lifetime.
func (vs *VersionSet) SetNextFileNumber(n uint64) {
	for {
		cur := vs.nextFile.Load()
		if n <= cur {
			return
		}
		if vs.nextFile.CompareAndSwap(cur, n) {
			return
		}
	}
}

// Apply builds a new version from the current one and installs it, returning
// the installed version.
//
// The whole read-modify-write runs under installMu, so concurrent callers
// serialise rather than each deriving from the same base and one overwriting
// the other. If the edit would produce a version that violates an invariant,
// nothing is installed and the current version is left alone.
func (vs *VersionSet) Apply(e *VersionEdit) (*Version, error) {
	vs.installMu.Lock()
	defer vs.installMu.Unlock()

	next, err := vs.current.Load().Apply(e)
	if err != nil {
		return nil, err
	}

	if e.NextFileNumber != nil {
		vs.SetNextFileNumber(*e.NextFileNumber)
	}

	vs.current.Store(next)
	return next, nil
}
