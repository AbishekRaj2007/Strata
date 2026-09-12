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

	// mu serialises installation and guards every reference count, both the
	// per-version counts and fileRefs. Installing is read-modify-write on the
	// current version, so two concurrent installers without it would each
	// derive from the same base and one would discard the other's work.
	//
	// It also closes T4.2's trap. Acquire cannot load the pointer and then
	// increment: between those two steps the version can reach zero
	// references and its files can be deleted, so the increment would land on
	// a version whose files are already gone. Loading and incrementing happen
	// together under this lock.
	mu sync.Mutex

	// fileRefs counts how many live versions name each file. A file is
	// eligible for deletion only at zero. The counts live here rather than on
	// FileMetadata so that a FileMetadata stays immutable and shareable
	// between versions.
	fileRefs map[uint64]int

	// obsolete holds files that reached zero references and are ready to be
	// deleted from disk.
	obsolete []uint64

	// commitMu serialises whole commits -- the manifest append and the
	// version install together -- and is held across the fsync.
	//
	// It is separate from mu on purpose. Two goroutines append to the
	// manifest once compaction exists: the flusher committing a new L0
	// table, and the compactor committing a merge. Holding only mu would
	// leave two ways for that to go wrong. The records could interleave in
	// the file, since the framing writer is not itself synchronised. And
	// even with atomic appends, two committers could append in one order and
	// install in the other, leaving the version a replay rebuilds different
	// from the one in memory.
	//
	// It must not be mu, because mu is what Acquire takes. Holding mu across
	// an fsync would block every reader for the duration of a disk write.
	commitMu sync.Mutex

	// nextFile is the allocator behind docs/format.md §1's single monotonic
	// file-number sequence, shared across .wal, .sst and MANIFEST files.
	nextFile atomic.Uint64
}

// Commit appends an edit to the manifest, fsyncs it, and installs the version
// it produces.
//
// This is the only way an edit should reach a manifest that more than one
// goroutine writes to. The two halves are one atomic step: the append is the
// commit point, and the install is what makes the commit visible, so a
// caller that did them separately could be interrupted between them by
// another committer and leave the durable order disagreeing with the
// in-memory one.
//
// The returned version is the new current one.
func (vs *VersionSet) Commit(log *Log, e *VersionEdit) (*Version, error) {
	vs.commitMu.Lock()
	defer vs.commitMu.Unlock()

	if err := log.Append(e); err != nil {
		return nil, err
	}
	return vs.Apply(e)
}

// NewVersionSet returns a set holding the empty version.
func NewVersionSet() *VersionSet {
	vs := &VersionSet{fileRefs: make(map[uint64]int)}

	// The set itself holds one reference on the current version, so the
	// version in place never reaches zero while it is still current.
	initial := NewVersion()
	initial.refs = 1
	vs.current.Store(initial)
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
// The whole read-modify-write runs under mu, so concurrent callers serialise
// rather than each deriving from the same base and one overwriting the other.
// If the edit would produce a version that violates an invariant, nothing is
// installed and the current version is left alone.
func (vs *VersionSet) Apply(e *VersionEdit) (*Version, error) {
	vs.mu.Lock()
	defer vs.mu.Unlock()

	previous := vs.current.Load()

	next, err := previous.Apply(e)
	if err != nil {
		return nil, err
	}

	if e.NextFileNumber != nil {
		vs.SetNextFileNumber(*e.NextFileNumber)
	}

	// The new version takes a reference on every file it names before the old
	// one gives its references up, so a file carried across the installation
	// never momentarily reaches zero and never becomes wrongly collectable.
	next.refs = 1
	vs.refFilesLocked(next)

	vs.current.Store(next)
	vs.releaseLocked(previous)

	return next, nil
}
