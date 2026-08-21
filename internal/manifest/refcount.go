package manifest

// Acquire returns the current version with a reference held on it, and on
// every file it names. The caller must Release it when the operation ends.
//
// This is the entry point for any read that will open a file. Current() is
// enough to inspect metadata, but a version obtained that way carries no
// promise that its files still exist: a compaction may install a replacement
// and delete them while the caller is still walking the list.
//
// Loading the pointer and incrementing the count happen together under the
// lock, which is the whole point. Doing it as an atomic load followed by an
// atomic increment leaves a window in which the version reaches zero
// references between the two, its files are deleted, and the increment
// resurrects a version naming files that are gone. That failure appears once
// in a few million operations, which means it appears in front of an
// audience rather than in a test.
func (vs *VersionSet) Acquire() *Version {
	vs.mu.Lock()
	defer vs.mu.Unlock()

	v := vs.current.Load()
	v.refs++
	return v
}

// Release drops a reference taken by Acquire. When the last reference to a
// version goes, the files it named lose one reference each, and any that
// reach zero become eligible for deletion.
func (vs *VersionSet) Release(v *Version) {
	vs.mu.Lock()
	defer vs.mu.Unlock()

	vs.releaseLocked(v)
}

// releaseLocked drops one reference from v. The caller holds mu.
func (vs *VersionSet) releaseLocked(v *Version) {
	if v.refs <= 0 {
		panic("manifest: release of a version with no references")
	}

	v.refs--
	if v.refs > 0 {
		return
	}

	// The version is dead. Every file it named loses the reference it held.
	for level := range v.levels {
		for _, f := range v.levels[level] {
			n := vs.fileRefs[f.Number] - 1
			if n < 0 {
				panic("manifest: file reference count fell below zero")
			}
			if n == 0 {
				delete(vs.fileRefs, f.Number)
				vs.obsolete = append(vs.obsolete, f.Number)
				continue
			}
			vs.fileRefs[f.Number] = n
		}
	}
}

// refFilesLocked takes a reference on every file a version names. The caller
// holds mu.
func (vs *VersionSet) refFilesLocked(v *Version) {
	for level := range v.levels {
		for _, f := range v.levels[level] {
			vs.fileRefs[f.Number]++
		}
	}
}

// FileRefs reports how many live versions name a file. It exists for tests and
// for the invariant checker; the read path never needs it.
func (vs *VersionSet) FileRefs(number uint64) int {
	vs.mu.Lock()
	defer vs.mu.Unlock()
	return vs.fileRefs[number]
}

// VersionRefs reports how many references are held on a version, including the
// one the set holds while it is current.
func (vs *VersionSet) VersionRefs(v *Version) int {
	vs.mu.Lock()
	defer vs.mu.Unlock()
	return v.refs
}

// Obsolete returns the file numbers that have reached zero references since
// the last call, and clears the list.
//
// Draining is deliberate: a file appears here exactly once, when its last
// holder released it, so the caller owns deleting it and a second caller
// cannot try to delete it again.
func (vs *VersionSet) Obsolete() []uint64 {
	vs.mu.Lock()
	defer vs.mu.Unlock()

	if len(vs.obsolete) == 0 {
		return nil
	}
	out := vs.obsolete
	vs.obsolete = nil
	return out
}

// IsLive reports whether any held version still names the file. A file that is
// not live may be deleted from disk; one that is live must not be, however
// many compactions have superseded it, because a reader is still holding a
// version that names it.
func (vs *VersionSet) IsLive(number uint64) bool {
	vs.mu.Lock()
	defer vs.mu.Unlock()
	return vs.fileRefs[number] > 0
}
