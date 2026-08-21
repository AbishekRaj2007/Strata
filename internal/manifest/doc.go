// Package manifest tracks which SSTables are live at each level.
//
// The central type is [Version]: an immutable snapshot of the per-level file
// lists. Compaction never edits a version in place, it derives a new one and
// [VersionSet] installs it with a single atomic pointer store, so a reader
// holding a version sees a file list that cannot change and files that cannot
// be deleted while it is held.
//
// The manifest log itself -- appending encoded edits to MANIFEST-NNNNNN and
// replaying them at startup -- is not here yet. docs/format.md §4 has it reuse
// the WAL block framing exactly, so it waits on the framing reader from T2.3.
// Everything above that line is present: [VersionEdit] encoding and decoding,
// [Version.Apply], reference counting, and the CURRENT file.
package manifest
