package wal

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"

	"github.com/AbishekRaj2007/Strata/internal/vfs"
)

// crcTable is the Castagnoli polynomial docs/format.md §0 fixes for every
// checksum in the format.
var crcTable = crc32.MakeTable(crc32.Castagnoli)

// Writer frames batches into 32 KiB blocks (§2.1). Fragmentation bounds the
// blast radius of a torn write: a crash mid-write damages at most one block,
// and the framing lets recovery locate exactly where the damage starts.
//
// A Writer is not safe for concurrent use. Serialising writes is the caller's
// job; the Syncer coordinates the fsyncs that follow them.
type Writer struct {
	f vfs.File

	// buf accumulates the block being filled. It is flushed when full and on
	// every Write, so bytes reach the OS before the Syncer is asked to make
	// them durable.
	buf      [BlockSize]byte
	blockPos int

	// blockStart is the file offset of buf[0]; a partial block is rewritten in
	// place at this offset as it grows.
	blockStart int64

	// offset counts every byte handed to the OS, headers included. Syncer
	// compares waiter offsets against it to decide which writes an fsync
	// covered, so undercounting here silently breaks durability.
	offset int64
}

// NewWriter frames batches into f, appending from its current end.
//
// f is a vfs.File rather than an *os.File so a test can fail the writes and
// the fsyncs underneath a real WAL (T7.2). The framing is unchanged either
// way: it is the same bytes at the same offsets.
func NewWriter(f vfs.File) *Writer {
	return &Writer{f: f}
}

// Write frames one batch, fragmenting it across blocks as needed. It returns
// the file offset immediately after the last byte written, which is the value
// to hand to Syncer.AwaitDurable.
//
// The returned offset means "these bytes are with the OS", never "these bytes
// are durable" -- durability is the Syncer's business.
func (w *Writer) Write(b *Batch) (int64, error) {
	return w.WriteRecord(b.Encode(nil))
}

// WriteRecord frames one opaque payload as a single record, fragmenting it
// across blocks as needed, and returns the file offset immediately after the
// last byte written.
//
// The framing knows nothing about what the payload means, which is what lets
// docs/format.md §4 reuse it verbatim for the manifest: the manifest log is
// the same 32 KiB blocks, the same 7-byte fragment header, and the same
// CRC32C, carrying encoded version edits instead of batches. One framing
// implementation serves both, so there is exactly one place where a framing
// bug can live.
func (w *Writer) WriteRecord(payload []byte) (int64, error) {
	// A WriteAt below can fail partway through framing a multi-block record.
	// The bytes that already reached the file for an earlier block in this
	// same call are real, but this call as a whole has not: its caller
	// treats any error as "nothing was logged" (see Add's comment on the
	// point), and the next attempt must start from exactly where this one
	// did, not from wherever this one gave up. Restoring the mark on every
	// error path below is what keeps that true -- without it, a failed
	// WriteAt would leave the writer's own position ahead of what the file
	// actually holds, and everything appended after would be framed at the
	// wrong offset.
	mark := w.Mark()

	first := true

	for {
		avail := BlockSize - w.blockPos

		// A header needs 7 bytes. Fewer left means this block is finished:
		// zero-fill the remainder so a reader sees type 0 and skips to the
		// next boundary. §2.1 relies on type 0 never being valid.
		if avail < HeaderSize {
			for i := w.blockPos; i < BlockSize; i++ {
				w.buf[i] = 0
			}

			if _, err := w.f.WriteAt(w.buf[:BlockSize], w.blockStart); err != nil {
				w.offset, w.blockStart, w.blockPos = mark.offset, mark.blockStart, mark.blockPos
				return 0, fmt.Errorf("wal: write block at %d: %w", w.blockStart, err)
			}
			w.offset += int64(BlockSize - w.blockPos)
			w.blockStart += BlockSize
			w.blockPos = 0
			avail = BlockSize
		}

		room := avail - HeaderSize
		n := room
		if len(payload) < n {
			n = len(payload)
		}
		last := n == len(payload)

		pos := w.blockPos
		binary.LittleEndian.PutUint16(w.buf[pos+4:pos+6], uint16(n))
		w.buf[pos+6] = byte(fragmentType(first, last))
		copy(w.buf[pos+7:], payload[:n])

		w.blockPos += HeaderSize + n
		w.offset += int64(HeaderSize + n)

		binary.LittleEndian.PutUint32(w.buf[pos:pos+4],
			crc32.Checksum(w.buf[pos+6:pos+7+n], crcTable))

		payload = payload[n:]
		first = false
		if last {
			break
		}
	}

	// Hand the partial block to the OS now; the Syncer decides when it
	// becomes durable.
	if _, err := w.f.WriteAt(w.buf[:w.blockPos], w.blockStart); err != nil {
		w.offset, w.blockStart, w.blockPos = mark.offset, mark.blockStart, mark.blockPos
		return 0, fmt.Errorf("wal: write block at %d: %w", w.blockStart, err)
	}

	return w.offset, nil

}

// Offset reports how many bytes have been handed to the OS. Part of Syncable.
func (w *Writer) Offset() int64 {
	return w.offset
}

// Mark snapshots the writer's position, to be restored by Rollback if the
// write that follows turns out not to be durable.
type Mark struct {
	offset     int64
	blockStart int64
	blockPos   int
}

// Mark captures the writer's current position, before a write whose fsync
// might fail.
func (w *Writer) Mark() Mark {
	return Mark{offset: w.offset, blockStart: w.blockStart, blockPos: w.blockPos}
}

// Rollback undoes every write since m: it truncates the file back to m's
// offset and rewinds the writer's own position to match.
//
// This is what makes a failed fsync actually unrecoverable-and-handled rather
// than unrecoverable-and-ignored. The bytes a WriteRecord handed to the OS
// just before Sync failed are, by content alone, indistinguishable from a
// genuinely durable record: same framing, same checksum. Left in place, a
// later reader -- this process on its next Open, or a fresh one after a real
// crash -- has no way to know the fsync that was supposed to confirm them
// never did, and replays a write nothing ever acknowledged. Truncating here,
// on the failure path, is the only point where that ambiguity can still be
// resolved.
func (w *Writer) Rollback(m Mark) error {
	if err := w.f.Truncate(m.offset); err != nil {
		return fmt.Errorf("wal: truncate to %d: %w", m.offset, err)
	}
	w.offset, w.blockStart, w.blockPos = m.offset, m.blockStart, m.blockPos
	return nil
}

// ErrSyncFailed marks an error as a failed fsync rather than any other kind
// of I/O failure.
//
// The distinction is load-bearing. A failed write can be retried: nothing was
// promised and nothing is ambiguous. A failed fsync cannot. On Linux the
// error is reported exactly once and the dirty page is then dropped, so a
// caller that retries is told the data is safe when it is gone. Everything
// above this layer uses the sentinel to tell the two apart and to refuse to
// carry on after the second.
var ErrSyncFailed = errors.New("fsync failed")

// Sync forces everything written so far to physical media. Part of Syncable.
func (w *Writer) Sync() error {
	if err := w.f.Sync(); err != nil {
		return fmt.Errorf("wal: sync: %w: %w", ErrSyncFailed, err)
	}
	return nil
}

// Close syncs and closes the underlying file.
func (w *Writer) Close() error {
	if err := w.f.Sync(); err != nil {
		_ = w.f.Close()
		return fmt.Errorf("wal: sync on close: %w: %w", ErrSyncFailed, err)
	}
	if err := w.f.Close(); err != nil {
		return fmt.Errorf("wal: close: %w", err)
	}
	return nil
}

// fragmentType maps a fragment's position within its record to the §2.1 type.
func fragmentType(first, last bool) FragmentType {
	switch {
	case first && last:
		return FragmentFull
	case first:
		return FragmentFirst
	case last:
		return FragmentLast
	default:
		return FragmentMiddle
	}
}
