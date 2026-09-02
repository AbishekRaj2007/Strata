package sstable

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"

	"github.com/AbishekRaj2007/Strata/internal/memtable"
)

// crcTable is the Castagnoli polynomial docs/format.md §0 fixes for every
// checksum in the format.
var crcTable = crc32.MakeTable(crc32.Castagnoli)

// RestartInterval is how often a block stores a key in full rather than
// prefix-compressed against the previous entry (docs/format.md §3.2). A
// shorter interval saves less space but shortens the linear scan a seek runs
// after its binary search; readers must use the block's own restart_count
// rather than assume this constant, since it travels with the block.
const RestartInterval = 16

// ErrCorruptBlock is returned when a block fails its checksum, or when its
// bytes cannot be a valid encoding of one.
var ErrCorruptBlock = errors.New("sstable: corrupt block")

// BlockBuilder accumulates entries, in comparator order, into one data block.
// The caller decides when a block is full (docs/format.md §3.2 targets ~4096
// bytes); Size reports the encoder's progress so far to make that decision.
type BlockBuilder struct {
	buf      []byte
	restarts []uint32
	count    int
	prevKey  []byte
}

// NewBlockBuilder returns an empty builder.
func NewBlockBuilder() *BlockBuilder {
	return &BlockBuilder{}
}

// Add appends one entry. Callers must present entries in comparator order;
// the builder does not check this, since checking would mean holding every
// key rather than only the previous one.
func (b *BlockBuilder) Add(e memtable.Entry) {
	shared := 0
	if b.count%RestartInterval == 0 {
		b.restarts = append(b.restarts, uint32(len(b.buf)))
	} else {
		shared = commonPrefixLen(b.prevKey, e.Key)
	}
	unshared := e.Key[shared:]

	kind := byte(0)
	valueLen := len(e.Value)
	if e.Tombstone {
		kind = 1
		valueLen = 0 // §3.2: a DELETE carries no value bytes at all.
	}

	b.buf = binary.AppendUvarint(b.buf, uint64(shared))
	b.buf = binary.AppendUvarint(b.buf, uint64(len(unshared)))
	b.buf = binary.AppendUvarint(b.buf, uint64(valueLen))
	b.buf = append(b.buf, kind)
	b.buf = binary.LittleEndian.AppendUint64(b.buf, e.Sequence)
	b.buf = append(b.buf, unshared...)
	if !e.Tombstone {
		b.buf = append(b.buf, e.Value...)
	}

	b.prevKey = append(b.prevKey[:0], e.Key...)
	b.count++
}

// Size is the number of entry bytes buffered so far, excluding the trailer.
// It is an under-estimate of the final block size by a small, bounded amount
// (the restart array plus a dozen fixed bytes), which is fine for deciding
// when a ~4 KiB block is full.
func (b *BlockBuilder) Size() int {
	return len(b.buf)
}

// Empty reports whether any entry has been added.
func (b *BlockBuilder) Empty() bool {
	return b.count == 0
}

// Finish appends the trailer (restart offsets, restart count, compression
// type, checksum) and returns the complete block. The builder must not be
// reused afterwards.
func (b *BlockBuilder) Finish() []byte {
	out := b.buf
	for _, r := range b.restarts {
		out = binary.LittleEndian.AppendUint32(out, r)
	}
	out = binary.LittleEndian.AppendUint32(out, uint32(len(b.restarts)))
	out = append(out, 0) // compression_type = none; snappy is reserved, not written by v1.

	// The checksum covers everything above -- entries, restart array, restart
	// count and the compression byte -- but not itself.
	checksum := crc32.Checksum(out, crcTable)
	out = binary.LittleEndian.AppendUint32(out, checksum)
	return out
}

// commonPrefixLen returns how many leading bytes a and b share.
func commonPrefixLen(a, b []byte) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	i := 0
	for i < n && a[i] == b[i] {
		i++
	}
	return i
}

// Block is a decoded, checksum-verified data block ready for reads.
type Block struct {
	entries  []byte // entry bytes only, trailer stripped
	restarts []uint32
}

// NewBlock verifies data's checksum and parses its trailer. data must be
// exactly one block's bytes, trailer included.
func NewBlock(data []byte) (*Block, error) {
	// Minimum trailer: restart_count(4) + compression_type(1) + crc32c(4),
	// even for a block whose restart array happens to be empty.
	const minTrailer = 9
	if len(data) < minTrailer {
		return nil, fmt.Errorf("%w: block is %d bytes, too short for a trailer", ErrCorruptBlock, len(data))
	}

	checksum := binary.LittleEndian.Uint32(data[len(data)-4:])
	covered := data[:len(data)-4]
	if crc32.Checksum(covered, crcTable) != checksum {
		return nil, fmt.Errorf("%w: checksum mismatch", ErrCorruptBlock)
	}

	compressionType := covered[len(covered)-1]
	if compressionType != 0 {
		return nil, fmt.Errorf("%w: unsupported compression type %d", ErrCorruptBlock, compressionType)
	}

	restartCountOff := len(covered) - 1 - 4
	restartCount := binary.LittleEndian.Uint32(covered[restartCountOff : restartCountOff+4])

	restartArrayOff := restartCountOff - int(restartCount)*4
	if restartArrayOff < 0 {
		return nil, fmt.Errorf("%w: restart_count %d does not fit the block", ErrCorruptBlock, restartCount)
	}

	restarts := make([]uint32, restartCount)
	for i := range restarts {
		off := restartArrayOff + i*4
		restarts[i] = binary.LittleEndian.Uint32(covered[off : off+4])
	}

	return &Block{entries: covered[:restartArrayOff], restarts: restarts}, nil
}

// decodeEntry parses one entry from the front of data, reconstructing its key
// from prevKey when it is prefix-compressed. It returns the entry and the
// number of bytes consumed.
func decodeEntry(data []byte, prevKey []byte) (memtable.Entry, int, error) {
	shared, n := binary.Uvarint(data)
	if n <= 0 {
		return memtable.Entry{}, 0, fmt.Errorf("%w: malformed shared_prefix_len", ErrCorruptBlock)
	}
	rest := data[n:]

	unsharedLen, n := binary.Uvarint(rest)
	if n <= 0 {
		return memtable.Entry{}, 0, fmt.Errorf("%w: malformed unshared_len", ErrCorruptBlock)
	}
	rest = rest[n:]

	valueLen, n := binary.Uvarint(rest)
	if n <= 0 {
		return memtable.Entry{}, 0, fmt.Errorf("%w: malformed value_len", ErrCorruptBlock)
	}
	rest = rest[n:]

	if len(rest) < 1+8 {
		return memtable.Entry{}, 0, fmt.Errorf("%w: entry ends before kind/sequence", ErrCorruptBlock)
	}
	kind := rest[0]
	if kind != 0 && kind != 1 {
		return memtable.Entry{}, 0, fmt.Errorf("%w: kind %d is neither SET nor DELETE", ErrCorruptBlock, kind)
	}
	seq := binary.LittleEndian.Uint64(rest[1:9])
	rest = rest[9:]

	if shared > uint64(len(prevKey)) {
		return memtable.Entry{}, 0, fmt.Errorf("%w: shared_prefix_len %d exceeds the previous key", ErrCorruptBlock, shared)
	}
	if uint64(len(rest)) < unsharedLen {
		return memtable.Entry{}, 0, fmt.Errorf("%w: unshared_len %d runs past the block", ErrCorruptBlock, unsharedLen)
	}
	unshared := rest[:unsharedLen]
	rest = rest[unsharedLen:]

	tombstone := kind == 1
	var value []byte
	if tombstone {
		if valueLen != 0 {
			return memtable.Entry{}, 0, fmt.Errorf("%w: DELETE entry declares a non-zero value_len", ErrCorruptBlock)
		}
	} else {
		if uint64(len(rest)) < valueLen {
			return memtable.Entry{}, 0, fmt.Errorf("%w: value_len %d runs past the block", ErrCorruptBlock, valueLen)
		}
		value = rest[:valueLen]
		rest = rest[valueLen:]
	}

	key := make([]byte, 0, shared+unsharedLen)
	key = append(key, prevKey[:shared]...)
	key = append(key, unshared...)

	consumed := len(data) - len(rest)
	return memtable.Entry{Key: key, Sequence: seq, Value: value, Tombstone: tombstone}, consumed, nil
}

// NewIterator returns an iterator positioned before the first entry, matching
// the memtable.Iterator convention.
func (blk *Block) NewIterator() *BlockIterator {
	return &BlockIterator{block: blk}
}

// Seek returns an iterator positioned so that the next call to Next lands on
// the first entry whose key is >= target -- the correct successor when target
// falls between two entries. It binary-searches the restart array for the
// last restart point at or before target, then scans forward from there.
func (blk *Block) Seek(target []byte) (*BlockIterator, error) {
	startOff := uint32(0)
	if len(blk.restarts) > 0 {
		lo, hi := 0, len(blk.restarts)-1
		best := 0
		for lo <= hi {
			mid := (lo + hi) / 2
			e, _, err := decodeEntry(blk.entries[blk.restarts[mid]:], nil)
			if err != nil {
				return nil, err
			}
			if bytes.Compare(e.Key, target) <= 0 {
				best = mid
				lo = mid + 1
			} else {
				hi = mid - 1
			}
		}
		startOff = blk.restarts[best]
	}

	it := &BlockIterator{block: blk, pos: int(startOff)}
	for {
		save := *it
		if !it.Next() {
			*it = save
			break
		}
		if bytes.Compare(it.cur.Key, target) >= 0 {
			*it = save
			break
		}
	}
	return it, nil
}

// BlockIterator walks a Block's entries in order. The zero value is not
// usable; obtain one from Block.NewIterator or Block.Seek.
type BlockIterator struct {
	block *Block
	pos   int
	key   []byte // full key of the entry at pos-1, for prefix reconstruction
	cur   memtable.Entry
	err   error
}

// Next advances to the next entry and reports whether one exists. It returns
// false at a clean end of block and also on decode failure; callers that need
// to distinguish the two call Err afterwards.
func (it *BlockIterator) Next() bool {
	if it.err != nil || it.pos >= len(it.block.entries) {
		return false
	}

	e, n, err := decodeEntry(it.block.entries[it.pos:], it.key)
	if err != nil {
		it.err = err
		return false
	}

	it.pos += n
	it.key = e.Key
	it.cur = e
	return true
}

// Entry returns the entry at the current position. Only valid after Next has
// returned true.
func (it *BlockIterator) Entry() memtable.Entry {
	return it.cur
}

// Err reports the decode error, if Next returned false because the block was
// malformed rather than because it was exhausted.
func (it *BlockIterator) Err() error {
	return it.err
}
