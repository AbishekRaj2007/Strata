// Package sstable reads and writes immutable sorted tables on disk.
//
// [BlockBuilder] and [Block] are the data block codec (T3.3, docs/format.md
// §3.2): prefix-compressed entries ordered by the memtable package's
// comparator, periodic restart points, and a checksummed trailer.
//
// [Writer] and [Table] are the table builder and reader (T3.4,
// docs/format.md §3): a complete file of data blocks, a bloom block (stub
// until Phase 5, but checksummed like every other section), an index block,
// and a fixed footer, with every section's offset recorded as it is written
// rather than patched in afterward.
package sstable
