package manifest

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/AbishekRaj2007/Strata/internal/vfs"
)

// DeleteObsolete removes every file that has reached zero references, and
// returns the numbers of the files it deleted.
//
// It is safe to call at any time precisely because Obsolete only reports files
// no held version names. A file already gone is not an error: a previous run
// may have deleted it and crashed before the manifest recorded anything, and
// re-deleting is the expected outcome rather than a fault.
//
// It returns the numbers rather than a count because a deleted file leaves
// state elsewhere in the process -- its blocks in the read cache -- and the
// caller cannot drop that without knowing which files went. A count would
// force the caller to guess.
func (vs *VersionSet) DeleteObsolete(fsys vfs.FS, dir string) ([]uint64, error) {
	fsys = vfs.Or(fsys)
	numbers := vs.Obsolete()
	if len(numbers) == 0 {
		return nil, nil
	}

	var deleted []uint64
	for _, number := range numbers {
		path := filepath.Join(dir, fmt.Sprintf("%06d.sst", number))
		if err := fsys.Remove(path); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return deleted, fmt.Errorf("manifest: delete %s: %w", path, err)
		}
		deleted = append(deleted, number)
	}

	// The deletions are not durable until the directory entries are.
	// Reporting success before that would let a caller record the files as
	// gone when a crash could still bring them back.
	if len(deleted) > 0 {
		if err := SyncDir(fsys, dir); err != nil {
			return deleted, err
		}
	}
	return deleted, nil
}

// SweepOrphans deletes .sst files in dir that the version does not name, and
// returns how many it removed.
//
// docs/format.md §4.1 requires this at startup. The manifest fsync is
// compaction's commit point: a crash before it leaves the outputs already
// written to disk but referenced by nothing, and a crash after leaves the
// inputs referenced by nothing. Either way the unreferenced files are dead
// weight that no future version can ever name, because file numbers are never
// reused.
//
// It must run only at startup, before any compaction is in flight. Mid-run, a
// compaction's output exists on disk for a while before the edit naming it is
// committed, and sweeping then would delete the file out from under it.
func SweepOrphans(fsys vfs.FS, dir string, v *Version) (int, error) {
	fsys = vfs.Or(fsys)
	live := make(map[uint64]bool, v.TotalFiles())
	for level := 0; level < NumLevels; level++ {
		for _, f := range v.Files(level) {
			live[f.Number] = true
		}
	}

	entries, err := fsys.ReadDir(dir)
	if err != nil {
		return 0, fmt.Errorf("manifest: read dir %s: %w", dir, err)
	}

	removed := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		number, ok := parseTableName(entry.Name())
		if !ok || live[number] {
			continue
		}
		if err := fsys.Remove(filepath.Join(dir, entry.Name())); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return removed, fmt.Errorf("manifest: delete orphan %s: %w", entry.Name(), err)
		}
		removed++
	}

	if removed > 0 {
		if err := SyncDir(fsys, dir); err != nil {
			return removed, err
		}
	}
	return removed, nil
}

// parseTableName extracts the file number from an SSTable filename, and
// reports whether the name is one.
//
// Anything that is not exactly a table name is left alone. A sweep that
// guessed at unfamiliar filenames would delete the WAL, the manifest, or
// CURRENT, so the match is strict rather than a suffix check.
func parseTableName(name string) (uint64, bool) {
	digits, ok := strings.CutSuffix(name, ".sst")
	if !ok || digits == "" {
		return 0, false
	}
	number, err := strconv.ParseUint(digits, 10, 64)
	if err != nil {
		return 0, false
	}
	// Reject a name that would not round-trip, so "1.sst" and "0000001.sst"
	// are not both accepted as file 1 and silently treated as one file.
	if fmt.Sprintf("%06d.sst", number) != name {
		return 0, false
	}
	return number, true
}
