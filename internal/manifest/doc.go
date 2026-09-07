// Package manifest tracks which SSTables are live at each level.
//
// The central type is [Version]: an immutable snapshot of the per-level file
// lists. Compaction never edits a version in place, it derives a new one and
// [VersionSet] installs it with a single atomic pointer store, so a reader
// holding a version sees a file list that cannot change and files that cannot
// be deleted while it is held.
//
// [Log] is the manifest log: [Log.Append] writes one record per edit and
// fsyncs it, and that fsync is the commit point docs/format.md §4.1 defines.
// [Recover] replays the manifest CURRENT names back into a VersionSet.
//
// The framing is the WAL's, reused rather than reimplemented -- §4 specifies
// the same blocks, headers and checksums, so the manifest calls
// wal.Writer.WriteRecord and wal.Reader.NextRecord directly. The clean-tail
// rule from §2.3 comes with it, which is what makes a manifest truncated
// mid-edit recover to the last complete edit without any code of its own.
package manifest
