package compaction

import (
	"fmt"

	"github.com/AbishekRaj2007/Strata/internal/cache"
	"github.com/AbishekRaj2007/Strata/internal/manifest"
)

// Committer makes a finished compaction visible.
//
// This is the crash-safety crux of the whole system, and it rests on one
// fact: the manifest fsync is the commit point, so there are exactly two
// states and no third.
//
//	Crash before it — the compaction never happened. Every input is still
//	named by the committed manifest and still live. The outputs exist on
//	disk referenced by nothing, and startup sweeps them.
//
//	Crash after it — the compaction happened completely. The outputs are
//	named, the inputs are not, and startup sweeps the inputs instead.
//
// Everything in Commit is arranged to keep that true. In particular no input
// file is removed until after the fsync, which is T6.3's trap: deleting one
// before the edit is durable looks harmless because the window is
// microseconds wide, and it is permanent data loss when the crash lands in
// it.
type Committer struct {
	// Dir is the data directory.
	Dir string

	// Log is the manifest the edit is appended to. Its Append fsyncs.
	Log *manifest.Log

	// Versions holds the current version and installs the replacement.
	Versions *manifest.VersionSet

	// Cache is the shared block cache. Blocks of a deleted file are evicted
	// from it, since nothing will ever read that file again and the LRU
	// cannot reclaim them on its own. Nil is usable.
	Cache *cache.Cache
}

// Edit builds the version edit that commits a compaction: a DELETE for every
// input at the level it currently sits at, and an ADD for every output at the
// output level.
//
// One edit, not two. The record is the atomic unit (docs/format.md §4.1), so
// a replay either sees every deletion and every addition or none of them.
// Splitting them across records would create the third state -- inputs gone,
// outputs missing -- which is data loss that nothing can recover from.
func Edit(c *Compaction, res *Result) *manifest.VersionEdit {
	var e manifest.VersionEdit

	for _, f := range c.Base {
		e.DeleteFile(c.Level, f.Number)
	}
	for _, f := range c.Parent {
		e.DeleteFile(c.OutputLevel(), f.Number)
	}

	var highest uint64
	for _, f := range res.Outputs {
		e.AddFile(c.OutputLevel(), f)
		if f.Number > highest {
			highest = f.Number
		}
	}

	// Record the allocator's position so a replay does not hand out a number
	// already on disk. Undershooting a concurrent allocation is safe --
	// replay only ever moves the counter forward -- but undershooting the
	// files this edit just named is not.
	if highest > 0 {
		e.SetNextFileNumber(highest + 1)
	}

	return &e
}

// Commit installs a finished compaction and returns the new current version.
//
// The order is the durability argument, and it is the one thing here that
// must not be rearranged:
//
//  1. Check the edit applies to the current version. A rejected edit must
//     never reach the manifest.
//  2. Append the edit and fsync. This is the commit point.
//  3. Swap the version pointer and release the inputs' references.
//  4. Delete files that reached zero references, and drop their blocks.
//
// Step 1 is not merely defensive. Once step 2 returns, the durable manifest
// says the compaction happened; if step 3 then rejected the edit, the state
// on disk and the state in memory would disagree permanently and no restart
// would fix it. Validating first turns that unrecoverable case into an error
// returned before anything was written.
//
// Step 4 is where the trap lives. A file is removed only once no held version
// names it, which is what the reference counts in the version set track --
// never merely because a newer version superseded it. A reader that started
// before the swap is still walking the old version, and its files must stay
// on disk until it is done.
func (cm *Committer) Commit(c *Compaction, res *Result) (*manifest.Version, error) {
	edit := Edit(c, res)

	// Step 1: a dry run against the current version. The result is
	// discarded; what matters is that Apply would accept it.
	if _, err := cm.Versions.Current().Apply(edit); err != nil {
		return nil, fmt.Errorf("compaction: edit does not apply to the current version: %w", err)
	}

	// Step 2: the commit point.
	if err := cm.Log.Append(edit); err != nil {
		return nil, fmt.Errorf("compaction: commit: %w", err)
	}

	// Step 3. A failure here is unrecoverable by construction -- the dry run
	// above is what makes it near-impossible -- so it is reported as such
	// rather than retried.
	next, err := cm.Versions.Apply(edit)
	if err != nil {
		return nil, fmt.Errorf("compaction: install version after a durable commit, "+
			"the database must be reopened: %w", err)
	}

	// Step 4, and only now.
	if err := cm.dropObsolete(); err != nil {
		return next, err
	}
	return next, nil
}

// dropObsolete deletes every file that has lost its last reference and evicts
// its cached blocks.
//
// The eviction is not an optimisation. Compaction retires files continuously,
// and without it the cache fills with blocks of files that no longer exist --
// blocks nothing will ever read again, so they never become least recently
// used by access and the LRU works around them forever.
func (cm *Committer) dropObsolete() error {
	deleted, err := cm.Versions.DeleteObsolete(cm.Dir)
	for _, number := range deleted {
		cm.Cache.EvictFile(number)
	}
	if err != nil {
		return fmt.Errorf("compaction: delete obsolete files: %w", err)
	}
	return nil
}
