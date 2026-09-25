package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// dirLock holds an advisory exclusive lock on a data directory for the
// lifetime of the *os.File it wraps. The lock is released by closing the
// file, which the kernel does automatically if the process dies -- exactly
// the property a lock meant to survive a crash needs.
type dirLock struct {
	f *os.File
}

// lockDir takes an exclusive, non-blocking flock on dir/LOCK.
//
// It goes through the real filesystem rather than the vfs.FS seam: a
// directory lock is a process-level guard against two engines sharing one
// data directory, not a durability operation, so T7.2's fault injector has
// no business over it, and the fault sweep would otherwise have to account
// for a syscall that has nothing to do with the durability it is testing.
//
// **Done when** (T7.3 edge case): a second Open against a directory already
// held by a live process fails with a clear error instead of both engines
// silently interleaving writes into the same WAL and manifest.
func lockDir(dir string) (*dirLock, error) {
	path := filepath.Join(dir, "LOCK")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("engine: open lock file %s: %w", path, err)
	}

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("engine: directory %s is already open by another process: %w", dir, err)
	}

	return &dirLock{f: f}, nil
}

// unlock releases the flock and closes the underlying file.
func (l *dirLock) unlock() error {
	if l == nil {
		return nil
	}
	// Unlocking before close is belt-and-suspenders: close alone releases
	// the flock, but making the release explicit means a future change that
	// keeps the fd open past Close cannot silently turn this into a leak.
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	return l.f.Close()
}
