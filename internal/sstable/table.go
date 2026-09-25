package sstable

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"

	"sort"
	"sync/atomic"

	"github.com/AbishekRaj2007/Strata/internal/bloom"
	"github.com/AbishekRaj2007/Strata/internal/cache"
	"github.com/AbishekRaj2007/Strata/internal/memtable"
	"github.com/AbishekRaj2007/Strata/internal/vfs"
)

const (
	// targetBlockBytes is the ~4 KiB data block size docs/format.md §3.2
	// targets. A block closes once adding another entry would exceed it, so a
	// single entry larger than the target still forms a valid block of its
	// own rather than being truncated.
	targetBlockBytes = 4096

	footerSize    = 48
	formatVersion = 1

	// tableMagic is the ASCII bytes "STRATA" framed by NUL, stored
	// little-endian so the file's last 8 bytes read 00 41 54 41 52 54 53 00.
	tableMagic uint64 = 0x0053545241544100
)

// ErrCorruptTable is the class every real-damage error in a table's own
// framing wraps -- footer, index, or bloom block. A data block failure wraps
// ErrCorruptBlock instead, since that check lives in block.go and is useful
// independent of a full table.
var ErrCorruptTable = errors.New("sstable: corrupt table")

// ErrUnsupportedVersion is returned for a recognised table whose
// format_version this build does not understand -- a version mismatch, not a
// parse failure, so the diagnostic tells the operator what actually happened.
var ErrUnsupportedVersion = errors.New("sstable: unsupported format version")

// Info summarises a table just written or opened. It is everything the
// manifest needs to record the file without reopening it.
type Info struct {
	Path        string
	EntryCount  int
	SmallestKey []byte
	LargestKey  []byte
	Size        int64

	// SmallestSeq and LargestSeq bound the sequence numbers in the table.
	// They are tracked as a running min and max rather than read off the
	// first and last entries: the comparator orders by user key ascending
	// and sequence *descending*, so the extremes of the sequence range can
	// sit anywhere in the file.
	SmallestSeq uint64
	LargestSeq  uint64

	// DistinctKeys is the number of distinct user keys, which is what sized
	// the bloom filter. It differs from EntryCount whenever a table holds
	// more than one version of a key.
	DistinctKeys int

	// BloomBytes is the size of the encoded bloom block, the memory price of
	// the filter for this table. T5.3 plots it against false positive rate.
	BloomBytes int
}

// indexEntry is one docs/format.md §3.4 index record: the largest user key in
// a data block, and where that block lives in the file.
type indexEntry struct {
	largestKey  []byte
	blockOffset uint64
	blockLength uint32
}

// footer is the fixed 48-byte trailer docs/format.md §3.5 defines.
type footer struct {
	bloomOffset uint64
	bloomLength uint64
	indexOffset uint64
	indexLength uint64
}

func (f footer) encode() []byte {
	buf := make([]byte, footerSize)
	binary.LittleEndian.PutUint64(buf[0:8], f.bloomOffset)
	binary.LittleEndian.PutUint64(buf[8:16], f.bloomLength)
	binary.LittleEndian.PutUint64(buf[16:24], f.indexOffset)
	binary.LittleEndian.PutUint64(buf[24:32], f.indexLength)
	binary.LittleEndian.PutUint32(buf[32:36], formatVersion)
	// buf[36:40] is the reserved field, written zero.
	binary.LittleEndian.PutUint64(buf[40:48], tableMagic)
	return buf
}

func decodeFooter(b []byte) (footer, error) {
	magic := binary.LittleEndian.Uint64(b[40:48])
	if magic != tableMagic {
		return footer{}, fmt.Errorf("%w: not a Strata SSTable (bad magic)", ErrCorruptTable)
	}
	version := binary.LittleEndian.Uint32(b[32:36])
	if version != formatVersion {
		return footer{}, fmt.Errorf("%w: file is format_version %d, this build understands %d", ErrUnsupportedVersion, version, formatVersion)
	}
	return footer{
		bloomOffset: binary.LittleEndian.Uint64(b[0:8]),
		bloomLength: binary.LittleEndian.Uint64(b[8:16]),
		indexOffset: binary.LittleEndian.Uint64(b[16:24]),
		indexLength: binary.LittleEndian.Uint64(b[24:32]),
	}, nil
}

func encodeIndex(entries []indexEntry) []byte {
	var buf []byte
	for _, e := range entries {
		buf = binary.AppendUvarint(buf, uint64(len(e.largestKey)))
		buf = append(buf, e.largestKey...)
		buf = binary.LittleEndian.AppendUint64(buf, e.blockOffset)
		buf = binary.LittleEndian.AppendUint32(buf, e.blockLength)
	}
	buf = binary.LittleEndian.AppendUint32(buf, uint32(len(entries)))
	checksum := crc32.Checksum(buf, crcTable)
	return binary.LittleEndian.AppendUint32(buf, checksum)
}

func decodeIndex(b []byte) ([]indexEntry, error) {
	if len(b) < 8 {
		return nil, fmt.Errorf("%w: index block is %d bytes, too short for a trailer", ErrCorruptTable, len(b))
	}
	checksum := binary.LittleEndian.Uint32(b[len(b)-4:])
	covered := b[:len(b)-4]
	if crc32.Checksum(covered, crcTable) != checksum {
		return nil, fmt.Errorf("%w: index checksum mismatch", ErrCorruptTable)
	}

	countOff := len(covered) - 4
	count := binary.LittleEndian.Uint32(covered[countOff:])
	rest := covered[:countOff]

	entries := make([]indexEntry, 0, count)
	for i := uint32(0); i < count; i++ {
		keyLen, n := binary.Uvarint(rest)
		if n <= 0 {
			return nil, fmt.Errorf("%w: index entry %d has a malformed key_len", ErrCorruptTable, i)
		}
		rest = rest[n:]

		if uint64(len(rest)) < keyLen+8+4 {
			return nil, fmt.Errorf("%w: index entry %d runs past the block", ErrCorruptTable, i)
		}
		key := append([]byte(nil), rest[:keyLen]...)
		rest = rest[keyLen:]
		offset := binary.LittleEndian.Uint64(rest[:8])
		rest = rest[8:]
		length := binary.LittleEndian.Uint32(rest[:4])
		rest = rest[4:]

		entries = append(entries, indexEntry{largestKey: key, blockOffset: offset, blockLength: length})
	}
	if len(rest) != 0 {
		return nil, fmt.Errorf("%w: %d trailing bytes in the index block", ErrCorruptTable, len(rest))
	}
	return entries, nil
}

// WriterOptions tunes how a table is built. BitsPerKey and BlockSize have
// defaults, and both are swept by T5.3's tuning study -- which is why they are
// parameters here rather than constants: a study that cannot vary the
// parameter it is studying is a benchmark, not a study.
type WriterOptions struct {
	// FS is the filesystem to write through. Nil is the real one; a test
	// substitutes an injector to fail the writes and fsyncs underneath a
	// table build (T7.2).
	FS vfs.FS

	// BitsPerKey sizes the bloom filter. Zero selects
	// bloom.DefaultBitsPerKey.
	BitsPerKey int

	// BlockSize is the target data block size in bytes. Zero selects the
	// docs/format.md §3.2 default of 4 KiB.
	BlockSize int
}

func (o WriterOptions) withDefaults() WriterOptions {
	if o.BitsPerKey <= 0 {
		o.BitsPerKey = bloom.DefaultBitsPerKey
	}
	if o.BlockSize <= 0 {
		o.BlockSize = targetBlockBytes
	}
	return o
}

// Writer builds one SSTable, writing sections in order and recording their
// offsets as it goes -- never patching the footer after the fact, since the
// final size of a preceding section is not known until it is written.
type Writer struct {
	f      vfs.File
	opts   WriterOptions
	offset int64

	block        *BlockBuilder
	blockLargest []byte
	filter       *bloom.Builder
	index        []indexEntry
	entryCount   int
	smallest     []byte
	largest      []byte
	smallestSeq  uint64
	largestSeq   uint64
}

func newWriter(f vfs.File, opts WriterOptions) *Writer {
	opts = opts.withDefaults()
	return &Writer{
		f:      f,
		opts:   opts,
		block:  NewBlockBuilder(),
		filter: bloom.NewBuilder(opts.BitsPerKey),
	}
}

// Add appends one entry. Entries must arrive in comparator order; Add trusts
// its caller the same way BlockBuilder.Add does.
func (w *Writer) Add(e memtable.Entry) error {
	estimate := len(e.Key) + len(e.Value) + 24
	if !w.block.Empty() && w.block.Size()+estimate > w.opts.BlockSize {
		if err := w.flushBlock(); err != nil {
			return err
		}
	}

	w.block.Add(e)
	w.blockLargest = append(w.blockLargest[:0], e.Key...)

	// The filter is fed the user key with the sequence stripped, per
	// docs/format.md §3.3. Because entries arrive in comparator order every
	// version of a key is adjacent, so the builder collapses the repeats and
	// sizes the bit array by distinct keys rather than by entry count.
	w.filter.Add(e.Key)

	if w.entryCount == 0 {
		w.smallest = append([]byte(nil), e.Key...)
		w.smallestSeq, w.largestSeq = e.Sequence, e.Sequence
	}
	w.largest = append(w.largest[:0], e.Key...)
	if e.Sequence < w.smallestSeq {
		w.smallestSeq = e.Sequence
	}
	if e.Sequence > w.largestSeq {
		w.largestSeq = e.Sequence
	}
	w.entryCount++
	return nil
}

// flushBlock writes the current data block to the file and records its index
// entry. It is a no-op on an empty block, which is what lets Finish call it
// unconditionally.
func (w *Writer) flushBlock() error {
	if w.block.Empty() {
		return nil
	}

	data := w.block.Finish()
	if _, err := w.f.Write(data); err != nil {
		return fmt.Errorf("sstable: write data block: %w", err)
	}

	w.index = append(w.index, indexEntry{
		largestKey:  append([]byte(nil), w.blockLargest...),
		blockOffset: uint64(w.offset),
		blockLength: uint32(len(data)),
	})
	w.offset += int64(len(data))

	w.block = NewBlockBuilder()
	w.blockLargest = w.blockLargest[:0]
	return nil
}

// Finish flushes the last data block, writes the bloom stub, index, and
// footer, and fsyncs the file. It does not fsync the containing directory --
// that is WriteTable's job, since only the caller that created the file's
// directory entry knows which directory to sync.
func (w *Writer) Finish() (Info, error) {
	if err := w.flushBlock(); err != nil {
		return Info{}, err
	}

	bloomOffset := w.offset
	bloomBytes := w.filter.Finish()
	if _, err := w.f.Write(bloomBytes); err != nil {
		return Info{}, fmt.Errorf("sstable: write bloom block: %w", err)
	}
	w.offset += int64(len(bloomBytes))

	indexOffset := w.offset
	indexBytes := encodeIndex(w.index)
	if _, err := w.f.Write(indexBytes); err != nil {
		return Info{}, fmt.Errorf("sstable: write index block: %w", err)
	}
	w.offset += int64(len(indexBytes))

	ft := footer{
		bloomOffset: uint64(bloomOffset),
		bloomLength: uint64(len(bloomBytes)),
		indexOffset: uint64(indexOffset),
		indexLength: uint64(len(indexBytes)),
	}
	if _, err := w.f.Write(ft.encode()); err != nil {
		return Info{}, fmt.Errorf("sstable: write footer: %w", err)
	}
	w.offset += footerSize

	if err := w.f.Sync(); err != nil {
		return Info{}, fmt.Errorf("sstable: sync: %w", err)
	}

	return Info{
		EntryCount:   w.entryCount,
		DistinctKeys: w.filter.Keys(),
		SmallestKey:  w.smallest,
		LargestKey:   w.largest,
		Size:         w.offset,
		SmallestSeq:  w.smallestSeq,
		LargestSeq:   w.largestSeq,
		BloomBytes:   len(bloomBytes),
	}, nil
}

// WriteTable builds a complete, durable SSTable from it (which must yield
// entries in comparator order) at dir/%06d.sst, and returns once both the
// file and its directory entry are fsynced.
//
// The directory fsync is not optional: a file is not durable until the
// directory entry naming it is durable, and skipping this step can leave a
// crash-recovered database missing a file its manifest still references.
func WriteTable(dir string, number uint64, it memtable.Iterator) (Info, error) {
	return WriteTableOpts(dir, number, it, WriterOptions{})
}

// WriteTableOpts is WriteTable with the build parameters exposed. A zero
// WriterOptions is exactly WriteTable.
func WriteTableOpts(dir string, number uint64, it memtable.Iterator, opts WriterOptions) (Info, error) {
	w, err := Create(dir, number, opts)
	if err != nil {
		return Info{}, err
	}
	// Abort is a no-op once Finish has run, so deferring it covers every
	// error path below without a partial file being left behind.
	defer func() { _ = w.Abort() }()

	for it.Next() {
		if err := w.Add(it.Entry()); err != nil {
			return Info{}, err
		}
	}

	return w.Finish()
}

func syncDir(fsys vfs.FS, dir string) error {
	if err := vfs.SyncDir(fsys, dir); err != nil {
		return fmt.Errorf("sstable: fsync dir %s: %w", dir, err)
	}
	return nil
}

// OpenOptions attaches a table to the shared block cache. A zero value opens
// an uncached table, which is what every test that only cares about the file
// format wants.
type OpenOptions struct {
	// Number is the table's file number, the first half of every cache key.
	// It must be the real number: two tables sharing a number would serve
	// each other's blocks, and the checksums would pass because the bytes
	// are individually valid.
	Number uint64

	// Cache is the shared block cache, or nil for no caching.
	Cache *cache.Cache

	// FS is the filesystem to read through. Nil is the real one. A table
	// opened through an injector is how a read-path fault is delivered.
	FS vfs.FS
}

// Table is an opened, immutable SSTable ready for reads. The footer and
// index are loaded and validated at Open; data blocks are loaded and
// checksum-verified on demand.
type Table struct {
	f      vfs.File
	number uint64
	cache  *cache.Cache
	index  []indexEntry
	filter *bloom.Filter
	footer footer

	// bloomRejects and bloomProbes count Get calls the filter answered
	// without touching a data block, and Get calls total. They are the
	// evidence for T5.3's claim about absent-key lookups, and they are
	// atomic because a Table is shared across reader goroutines.
	bloomRejects atomic.Uint64
	bloomProbes  atomic.Uint64

	// bytesRead counts bytes actually pulled from the file, excluding what
	// the cache served. It is the read amplification the T5.3 block-size
	// sweep measures: the ratio of this to the bytes a caller asked for.
	bytesRead atomic.Uint64
}

// Open validates the footer magic and format version, then loads and
// checksum-verifies the index. A file whose final 8 bytes are not the magic,
// or that is shorter than a footer, is rejected before any other field is
// read.
func Open(path string) (*Table, error) {
	return OpenWith(path, OpenOptions{})
}

// OpenWith is Open with the block cache attached. A zero OpenOptions is
// exactly Open.
func OpenWith(path string, opts OpenOptions) (*Table, error) {
	f, err := vfs.Or(opts.FS).Open(path)
	if err != nil {
		return nil, fmt.Errorf("sstable: open %s: %w", path, err)
	}

	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("sstable: stat %s: %w", path, err)
	}
	if st.Size() < footerSize {
		_ = f.Close()
		return nil, fmt.Errorf("%w: %s is %d bytes, too short for a footer", ErrCorruptTable, path, st.Size())
	}

	footerBytes := make([]byte, footerSize)
	if _, err := f.ReadAt(footerBytes, st.Size()-footerSize); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("sstable: read footer of %s: %w", path, err)
	}
	ft, err := decodeFooter(footerBytes)
	if err != nil {
		_ = f.Close()
		return nil, err
	}

	// The footer's offset/length fields are read from disk before anything
	// checksums them. A corrupted length here must be rejected before it
	// reaches make(), or a single flipped bit can turn into a
	// multi-gigabyte allocation instead of a clean error.
	fileSize := uint64(st.Size())
	if err := boundsCheck("bloom", ft.bloomOffset, ft.bloomLength, fileSize); err != nil {
		_ = f.Close()
		return nil, err
	}
	if err := boundsCheck("index", ft.indexOffset, ft.indexLength, fileSize); err != nil {
		_ = f.Close()
		return nil, err
	}

	bloomBytes := make([]byte, ft.bloomLength)
	if _, err := f.ReadAt(bloomBytes, int64(ft.bloomOffset)); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("sstable: read bloom block of %s: %w", path, err)
	}
	filter, err := bloom.Decode(bloomBytes)
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("%w: %s: %w", ErrCorruptTable, path, err)
	}

	indexBytes := make([]byte, ft.indexLength)
	if _, err := f.ReadAt(indexBytes, int64(ft.indexOffset)); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("sstable: read index of %s: %w", path, err)
	}
	index, err := decodeIndex(indexBytes)
	if err != nil {
		_ = f.Close()
		return nil, err
	}

	return &Table{
		f:      f,
		number: opts.Number,
		cache:  opts.Cache,
		index:  index,
		filter: filter,
		footer: ft,
	}, nil
}

// boundsCheck rejects an offset/length pair that does not fit within the
// file, guarding against integer overflow in offset+length before it is ever
// compared. name identifies the section in the resulting error.
func boundsCheck(name string, offset, length, fileSize uint64) error {
	if offset > fileSize || length > fileSize-offset {
		return fmt.Errorf("%w: %s block [offset=%d, length=%d] does not fit in a %d-byte file", ErrCorruptTable, name, offset, length, fileSize)
	}
	return nil
}

// Filter exposes the table's bloom filter for inspection and measurement.
func (t *Table) Filter() *bloom.Filter { return t.filter }

// BloomStats reports how many Get calls this table has served and how many of
// them the filter answered without reading a block.
func (t *Table) BloomStats() (probes, rejects uint64) {
	return t.bloomProbes.Load(), t.bloomRejects.Load()
}

// BytesRead reports how many bytes this table has pulled from the file,
// excluding blocks the cache served.
func (t *Table) BytesRead() uint64 { return t.bytesRead.Load() }

// Close closes the underlying file.
func (t *Table) Close() error {
	if err := t.f.Close(); err != nil {
		return fmt.Errorf("sstable: close: %w", err)
	}
	return nil
}

// loadBlock returns data block i, from the cache if it is resident and from
// the file otherwise.
//
// The checksum is verified on every call, cache hit included. That is not
// belt-and-braces: cached bytes live in process memory for as long as the
// cache holds them, and memory is not a medium that only fails at rest. The
// checksum is what makes a corrupted block a reported error rather than a
// wrong answer, and skipping it on the hot path would mean the fast path is
// the one without the safety check.
//
// The block is only inserted after it decodes. Caching bytes that failed
// verification would turn one bad read into a permanently bad read, since
// every subsequent lookup would be served the same broken block from memory
// without ever going back to the file.
func (t *Table) loadBlock(i int) (*Block, error) {
	e := t.index[i]
	key := cache.Key{FileNumber: t.number, BlockOffset: e.blockOffset}

	if data, ok := t.cache.Get(key); ok {
		return NewBlock(data)
	}

	data := make([]byte, e.blockLength)
	if _, err := t.f.ReadAt(data, int64(e.blockOffset)); err != nil {
		return nil, fmt.Errorf("sstable: read block %d: %w", i, err)
	}
	t.bytesRead.Add(uint64(len(data)))

	blk, err := NewBlock(data)
	if err != nil {
		return nil, err
	}
	t.cache.Put(key, data)
	return blk, nil
}

// blockFor returns the index of the first data block whose largest key is
// >= key -- the only block that could contain key, per docs/format.md §3.4.
// It reports false if key is greater than every key in the table.
func (t *Table) blockFor(key []byte) (int, bool) {
	i := sort.Search(len(t.index), func(i int) bool {
		return bytes.Compare(t.index[i].largestKey, key) >= 0
	})
	return i, i < len(t.index)
}

// Get returns the newest version of key. found reports whether any version
// exists; a tombstone is returned as a found entry with Tombstone set, the
// same contract as memtable.Memtable.Get.
func (t *Table) Get(key []byte) (e memtable.Entry, found bool, err error) {
	return t.get(key, true)
}

// getUnfiltered is the same lookup with the bloom filter bypassed. It is the
// pre-T5.1 read path, kept so the T5.3 study can measure what the filter buys
// against the identical code rather than against a different build.
func (t *Table) getUnfiltered(key []byte) (memtable.Entry, bool, error) {
	return t.get(key, false)
}

func (t *Table) get(key []byte, useFilter bool) (e memtable.Entry, found bool, err error) {
	// The filter is consulted first, ahead of the index binary search and
	// well ahead of any block read. That ordering is the entire point: on a
	// key this table does not hold, the whole lookup collapses to one hash
	// and k bit tests, with no I/O at all.
	//
	// This is only sound because the filter has no false negatives. A false
	// positive costs a wasted block read and nothing else; a false negative
	// would return "absent" for a key the table holds, and the LSM read path
	// would move on to older levels and answer with stale data or nothing.
	if useFilter {
		t.bloomProbes.Add(1)
		if !t.filter.MayContain(key) {
			t.bloomRejects.Add(1)
			return memtable.Entry{}, false, nil
		}
	}

	i, ok := t.blockFor(key)
	if !ok {
		return memtable.Entry{}, false, nil
	}

	blk, err := t.loadBlock(i)
	if err != nil {
		return memtable.Entry{}, false, err
	}
	it, err := blk.Seek(key)
	if err != nil {
		return memtable.Entry{}, false, err
	}
	if !it.Next() {
		if err := it.Err(); err != nil {
			return memtable.Entry{}, false, err
		}
		return memtable.Entry{}, false, nil
	}
	if !bytes.Equal(it.Entry().Key, key) {
		return memtable.Entry{}, false, nil
	}
	return it.Entry(), true, nil
}

// TableIterator walks a Table's entries across every data block in order.
type TableIterator struct {
	t        *Table
	blockIdx int
	blockIt  *BlockIterator
	err      error
}

// NewIterator returns an iterator positioned before the first entry.
func (t *Table) NewIterator() *TableIterator {
	return &TableIterator{t: t, blockIdx: -1}
}

// Seek returns an iterator positioned so that the next call to Next lands on
// the first entry whose key is >= target.
func (t *Table) Seek(target []byte) (*TableIterator, error) {
	i, ok := t.blockFor(target)
	if !ok {
		return &TableIterator{t: t, blockIdx: len(t.index)}, nil
	}

	blk, err := t.loadBlock(i)
	if err != nil {
		return nil, err
	}
	blkIt, err := blk.Seek(target)
	if err != nil {
		return nil, err
	}
	return &TableIterator{t: t, blockIdx: i, blockIt: blkIt}, nil
}

// Next advances to the next entry, loading the next data block on a block
// boundary, and reports whether one exists.
func (it *TableIterator) Next() bool {
	for {
		if it.blockIt != nil {
			if it.blockIt.Next() {
				return true
			}
			if err := it.blockIt.Err(); err != nil {
				it.err = err
				return false
			}
		}

		it.blockIdx++
		if it.blockIdx >= len(it.t.index) {
			return false
		}
		blk, err := it.t.loadBlock(it.blockIdx)
		if err != nil {
			it.err = err
			return false
		}
		it.blockIt = blk.NewIterator()
	}
}

// Entry returns the entry at the current position. Only valid after Next has
// returned true.
func (it *TableIterator) Entry() memtable.Entry {
	return it.blockIt.Entry()
}

// Err reports a decode error, if Next returned false because the table was
// malformed rather than because it was exhausted.
func (it *TableIterator) Err() error {
	return it.err
}
