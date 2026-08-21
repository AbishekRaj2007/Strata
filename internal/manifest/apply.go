package manifest

import (
	"bytes"
	"fmt"
	"sort"
)

// Apply returns a new Version with the edit applied. The receiver is not
// modified, so every holder of it continues to see exactly what it saw.
//
// Order matters, and it is the T4.1 trap. Deletions are applied before
// additions within a single edit, and edits are applied in the order they
// appear in the manifest. An ADD_FILE followed by a DELETE_FILE of the same
// file number leaves the file absent; accumulating additions and deletions
// into two sets and subtracting at the end would instead leave it absent only
// by luck of which set won. The encoder writes deletions first for the same
// reason -- a compaction rewriting a file into the level it came from must not
// delete the file it just added.
func (v *Version) Apply(e *VersionEdit) (*Version, error) {
	next := &Version{
		logNumber:      v.logNumber,
		nextFileNumber: v.nextFileNumber,
		lastSequence:   v.lastSequence,
	}

	// Copy the level slices. The files themselves are shared, because a
	// FileMetadata is immutable; only the lists differ between versions.
	deletedByLevel := make(map[int]map[uint64]bool, len(e.Deleted))
	for _, d := range e.Deleted {
		if d.Level < 0 || d.Level >= NumLevels {
			return nil, fmt.Errorf("%w: DELETE_FILE names level %d", ErrCorrupt, d.Level)
		}
		if deletedByLevel[d.Level] == nil {
			deletedByLevel[d.Level] = make(map[uint64]bool)
		}
		deletedByLevel[d.Level][d.Number] = true
	}

	for level := range v.levels {
		deleted := deletedByLevel[level]
		if len(deleted) == 0 {
			next.levels[level] = v.levels[level]
			continue
		}

		kept := make([]*FileMetadata, 0, len(v.levels[level]))
		for _, f := range v.levels[level] {
			if deleted[f.Number] {
				delete(deleted, f.Number)
				continue
			}
			kept = append(kept, f)
		}
		next.levels[level] = kept
	}

	// docs/format.md §4.2: a DELETE_FILE naming a file not currently present
	// is corruption. Anything still in the map was never matched.
	for level, remaining := range deletedByLevel {
		for number := range remaining {
			return nil, fmt.Errorf("%w: DELETE_FILE names file %d, absent from level %d",
				ErrCorrupt, number, level)
		}
	}

	for _, a := range e.Added {
		if a.Level < 0 || a.Level >= NumLevels {
			return nil, fmt.Errorf("%w: ADD_FILE names level %d", ErrCorrupt, a.Level)
		}
		if err := a.Meta.Validate(); err != nil {
			return nil, err
		}

		// Copy on first write to a level, so the slice this version handed to
		// the previous one is never appended into.
		level := a.Level
		grown := make([]*FileMetadata, len(next.levels[level]), len(next.levels[level])+1)
		copy(grown, next.levels[level])
		next.levels[level] = append(grown, a.Meta)
	}

	if e.LogNumber != nil {
		next.logNumber = *e.LogNumber
	}
	if e.NextFileNumber != nil {
		next.nextFileNumber = *e.NextFileNumber
	}
	if e.LastSequence != nil {
		next.lastSequence = *e.LastSequence
	}

	next.sortLevels()

	if err := next.CheckInvariants(); err != nil {
		return nil, err
	}
	return next, nil
}

// sortLevels restores each level's required ordering after files were added.
//
// L0 is ordered by file number descending because the read path must consult
// it newest first -- plan.md T4.3's trap is precisely that L0 ordering is by
// file number, never by any key-range heuristic. Levels below are ordered by
// smallest key so they stay binary-searchable.
func (v *Version) sortLevels() {
	sort.SliceStable(v.levels[0], func(i, j int) bool {
		return v.levels[0][i].Number > v.levels[0][j].Number
	})

	for level := 1; level < NumLevels; level++ {
		files := v.levels[level]
		sort.SliceStable(files, func(i, j int) bool {
			if c := bytes.Compare(files[i].Smallest, files[j].Smallest); c != 0 {
				return c < 0
			}
			return files[i].Number < files[j].Number
		})
	}
}
