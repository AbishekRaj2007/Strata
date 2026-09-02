// Package sstable reads and writes immutable sorted tables on disk.
//
// [BlockBuilder] and [Block] are the data block codec (T3.3, docs/format.md
// §3.2): prefix-compressed entries ordered by the memtable package's
// comparator, periodic restart points, and a checksummed trailer.
package sstable
