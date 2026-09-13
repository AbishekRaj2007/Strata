package vfs

import (
	"io"
	"io/fs"
	"os"
)

// File is the subset of *os.File the engine uses.
//
// It is deliberately no larger than that. A seam that mirrors the whole of
// *os.File is a seam every implementation has to reimplement faithfully, and
// the fault injector only has to be right about the operations that actually
// occur.
type File interface {
	io.Reader
	io.ReaderAt
	io.Writer
	io.Closer

	// Sync makes everything written so far durable. A failure here is
	// unrecoverable: see the durability argument in docs/concurrency.md and
	// the handling in the engine.
	Sync() error

	Stat() (fs.FileInfo, error)
	Name() string
}

// FS is the filesystem the engine runs on.
//
// Every method corresponds to an operation the engine genuinely performs.
// SyncDir is separate from Sync because directory durability is a distinct
// requirement -- a file that has been fsynced is still not guaranteed to have
// a name until its directory entry is durable too -- and making it a distinct
// method means the fault injector can fail it on its own.
type FS interface {
	// Create makes a new file, failing if it already exists. Exclusive
	// creation is what makes never-reused file numbers a real guarantee
	// rather than a convention.
	Create(path string) (File, error)

	// CreateTruncate makes or replaces a file. It exists for CURRENT's
	// temporary file, which is written and renamed on every manifest roll.
	CreateTruncate(path string) (File, error)

	// Open opens an existing file for reading.
	Open(path string) (File, error)

	// OpenDir opens a directory so it can be fsynced. The returned File
	// supports only Sync, Close and Name.
	OpenDir(path string) (File, error)

	Remove(path string) error
	Rename(oldPath, newPath string) error
	MkdirAll(path string, perm fs.FileMode) error
	ReadDir(path string) ([]os.DirEntry, error)
	ReadFile(path string) ([]byte, error)
	Stat(path string) (fs.FileInfo, error)
}

// Default is the real filesystem, and the FS every component uses unless a
// test substitutes another.
var Default FS = osFS{}

// OS returns the real filesystem.
func OS() FS { return Default }

// Or returns fs, or the real filesystem when fs is nil. It exists so every
// Options struct can carry an optional FS field without each caller repeating
// the nil check.
func Or(fsys FS) FS {
	if fsys == nil {
		return Default
	}
	return fsys
}

// osFS is the passthrough implementation. *os.File already satisfies File, so
// there is nothing here but the calls themselves.
type osFS struct{}

const (
	filePerm = 0o600
	dirPerm  = 0o700
)

func (osFS) Create(path string) (File, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, filePerm)
}

func (osFS) CreateTruncate(path string) (File, error) {
	return os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, filePerm)
}

func (osFS) Open(path string) (File, error) { return os.Open(path) }

func (osFS) OpenDir(path string) (File, error) { return os.Open(path) }

func (osFS) Remove(path string) error { return os.Remove(path) }

func (osFS) Rename(oldPath, newPath string) error { return os.Rename(oldPath, newPath) }

func (osFS) MkdirAll(path string, perm fs.FileMode) error {
	if perm == 0 {
		perm = dirPerm
	}
	return os.MkdirAll(path, perm)
}

func (osFS) ReadDir(path string) ([]os.DirEntry, error) { return os.ReadDir(path) }

func (osFS) ReadFile(path string) ([]byte, error) { return os.ReadFile(path) }

func (osFS) Stat(path string) (fs.FileInfo, error) { return os.Stat(path) }

// SyncDir fsyncs a directory, which is what makes a file's name durable.
//
// It is a helper rather than a method because every caller does the same
// three things -- open, sync, close -- and getting the close wrong on the
// error path is an easy way to leak a descriptor under exactly the conditions
// the fault injector creates.
func SyncDir(fsys FS, path string) error {
	d, err := Or(fsys).OpenDir(path)
	if err != nil {
		return err
	}
	if err := d.Sync(); err != nil {
		_ = d.Close()
		return err
	}
	return d.Close()
}
