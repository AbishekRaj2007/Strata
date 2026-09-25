package manifest

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/AbishekRaj2007/Strata/internal/vfs"
)

// CurrentFile is the name of the file naming the active manifest.
const CurrentFile = "CURRENT"

// Name returns the manifest filename for a file number.
func Name(number uint64) string {
	return fmt.Sprintf("MANIFEST-%06d", number)
}

// WriteCurrent atomically points CURRENT at the named manifest.
//
// The sequence is write-temp, fsync-temp, rename, fsync-directory, and every
// step earns its place. Without the fsync of the temporary file, the rename
// can be durable while the contents it names are not, leaving a CURRENT that
// is present but empty. Without the fsync of the directory, the rename itself
// is not durable and a crash can leave CURRENT naming the previous manifest.
// The rename in the middle is what makes the switch atomic: a reader sees the
// old name or the new one, never a partially written file.
func WriteCurrent(fsys vfs.FS, dir, manifestName string) error {
	fsys = vfs.Or(fsys)
	tmp := filepath.Join(dir, CurrentFile+".tmp")
	final := filepath.Join(dir, CurrentFile)

	// docs/format.md §1: the filename followed by a single newline, nothing
	// else.
	content := append([]byte(manifestName), '\n')

	f, err := fsys.CreateTruncate(tmp)
	if err != nil {
		return fmt.Errorf("manifest: create %s: %w", tmp, err)
	}

	if _, err := f.Write(content); err != nil {
		_ = f.Close()
		return fmt.Errorf("manifest: write %s: %w", tmp, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("manifest: fsync %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("manifest: close %s: %w", tmp, err)
	}

	if err := fsys.Rename(tmp, final); err != nil {
		return fmt.Errorf("manifest: rename %s to %s: %w", tmp, final, err)
	}

	return SyncDir(fsys, dir)
}

// ReadCurrent returns the manifest filename CURRENT names.
//
// docs/format.md §1 makes a CURRENT that is empty, lacks its trailing
// newline, or names a file that does not exist corruption rather than a
// recoverable state. Each of those means the atomic write did not complete,
// and guessing past it would open the wrong manifest and reconstruct a version
// that does not describe what is on disk.
func ReadCurrent(fsys vfs.FS, dir string) (string, error) {
	fsys = vfs.Or(fsys)
	path := filepath.Join(dir, CurrentFile)

	data, err := fsys.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("manifest: read %s: %w", path, err)
	}

	if len(data) == 0 {
		return "", fmt.Errorf("%w: %s is empty", ErrCorrupt, CurrentFile)
	}
	if data[len(data)-1] != '\n' {
		return "", fmt.Errorf("%w: %s lacks its trailing newline", ErrCorrupt, CurrentFile)
	}

	name := string(data[:len(data)-1])
	if len(name) == 0 {
		return "", fmt.Errorf("%w: %s names no manifest", ErrCorrupt, CurrentFile)
	}
	// A newline inside the name means more than one line was written, which
	// the format does not allow.
	if strings.ContainsRune(name, '\n') {
		return "", fmt.Errorf("%w: %s holds more than one line", ErrCorrupt, CurrentFile)
	}

	if _, err := fsys.Stat(filepath.Join(dir, name)); err != nil {
		return "", fmt.Errorf("%w: %s names %q, which does not exist", ErrCorrupt, CurrentFile, name)
	}

	return name, nil
}

// SyncDir fsyncs a directory so that entries created or renamed within it are
// durable. A file's creation is not durable until the directory entry naming
// it is.
func SyncDir(fsys vfs.FS, dir string) error {
	if err := vfs.SyncDir(fsys, dir); err != nil {
		return fmt.Errorf("manifest: fsync dir %s: %w", dir, err)
	}
	return nil
}
