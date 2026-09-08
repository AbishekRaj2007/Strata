package bloom

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"

	"github.com/cespare/xxhash/v2"
)

// DefaultBitsPerKey is the m/n the engine builds filters at unless told
// otherwise. Ten bits per key yields a ~1% false positive rate at the optimal
// probe count, which is the knee of the curve: going to 16 costs 60% more
// memory to buy roughly 0.6 percentage points.
const DefaultBitsPerKey = 10

// minBitArrayBytes floors the bit array at 64 bits. A table with two or three
// keys would otherwise get an array so small that the modulo folds every probe
// onto the same handful of bits and the filter reports "maybe" for everything.
// The floor costs 8 bytes per table and makes small tables actually filter.
const minBitArrayBytes = 8

// maxProbes caps k. Past this the probe loop costs more than the block read it
// is trying to avoid, and no sane bits-per-key gets near it.
const maxProbes = 30

// ErrCorruptFilter is returned for a bloom block whose framing does not
// decode: too short, or a checksum that does not match its bytes.
var ErrCorruptFilter = errors.New("bloom: corrupt filter block")

var crcTable = crc32.MakeTable(crc32.Castagnoli)

// headerSize is the three u32 fields ahead of the bit array.
const headerSize = 12

// OptimalProbes returns the k that minimises the false positive rate for a
// given bits-per-key, which is k = (m/n)·ln2.
//
// The shape of that optimum is worth stating, because "more hash functions
// means more filtering" is the natural and wrong intuition. Each probe both
// adds a test a non-member must pass -- which helps -- and sets one more bit
// per inserted key -- which hurts, because it drives the array toward
// saturation. Below the optimum the first effect dominates; above it the
// second does, and a filter with too many probes is a bit array of mostly
// ones, where every query finds its bits already set and reports "maybe" for
// everything. At exactly k = (m/n)·ln2 the array ends up half full, which is
// the point where one more probe stops paying for itself.
func OptimalProbes(bitsPerKey int) int {
	// 0.69 is ln2 to the precision that matters here; the result is rounded
	// to an integer anyway.
	k := int(float64(bitsPerKey)*0.69 + 0.5)
	if k < 1 {
		k = 1
	}
	if k > maxProbes {
		k = maxProbes
	}
	return k
}

// Builder accumulates the keys of one table and encodes the filter block.
//
// It stores 64-bit hashes rather than the keys themselves: the hash is all the
// filter needs, and holding one 8-byte word per key instead of the key bytes
// keeps a table build's peak memory independent of key length.
type Builder struct {
	bitsPerKey int
	hashes     []uint64
	haveLast   bool
	last       uint64
}

// NewBuilder returns a Builder at the given bits-per-key. A non-positive
// value selects DefaultBitsPerKey.
func NewBuilder(bitsPerKey int) *Builder {
	if bitsPerKey <= 0 {
		bitsPerKey = DefaultBitsPerKey
	}
	return &Builder{bitsPerKey: bitsPerKey}
}

// Add records a user key. The sequence number must already be stripped:
// docs/format.md §3.3 filters on user keys alone so every version of a key
// shares one entry.
//
// Repeats of the immediately preceding key are dropped. That is not a general
// deduplication -- it relies on the SSTable writer feeding keys in comparator
// order, where all versions of a key are adjacent -- and it matters because
// counting a key once per version would size the bit array by entry count
// rather than by distinct keys, wasting memory in proportion to the update
// rate.
func (b *Builder) Add(key []byte) {
	h := xxhash.Sum64(key)
	if b.haveLast && h == b.last {
		return
	}
	b.hashes = append(b.hashes, h)
	b.haveLast = true
	b.last = h
}

// Keys reports how many distinct keys have been added.
func (b *Builder) Keys() int { return len(b.hashes) }

// Finish encodes the docs/format.md §3.3 bloom block, checksum included.
//
// A builder with no keys emits bit_array_len = 0, which decodes to a filter
// that reports every key absent. That is the correct answer for an empty
// table, not a degenerate one.
func (b *Builder) Finish() []byte {
	if len(b.hashes) == 0 {
		return encode(uint32(b.bitsPerKey), 0, nil)
	}

	probes := OptimalProbes(b.bitsPerKey)

	arrayBytes := (len(b.hashes)*b.bitsPerKey + 7) / 8
	if arrayBytes < minBitArrayBytes {
		arrayBytes = minBitArrayBytes
	}
	bits := make([]byte, arrayBytes)
	nbits := uint32(arrayBytes * 8)

	for _, h := range b.hashes {
		h1, h2 := uint32(h), uint32(h>>32)
		for i := 0; i < probes; i++ {
			pos := h1 % nbits
			bits[pos/8] |= 1 << (pos % 8)
			h1 += h2
		}
	}

	return encode(uint32(b.bitsPerKey), uint32(probes), bits)
}

func encode(bitsPerKey, probes uint32, bits []byte) []byte {
	buf := make([]byte, 0, headerSize+len(bits)+4)
	buf = binary.LittleEndian.AppendUint32(buf, bitsPerKey)
	buf = binary.LittleEndian.AppendUint32(buf, probes)
	buf = binary.LittleEndian.AppendUint32(buf, uint32(len(bits)))
	buf = append(buf, bits...)
	return binary.LittleEndian.AppendUint32(buf, crc32.Checksum(buf, crcTable))
}

// Filter is a decoded bloom block, ready for membership queries.
type Filter struct {
	bitsPerKey uint32
	probes     uint32
	bits       []byte
	nbits      uint32
}

// Decode parses and checksum-verifies a bloom block.
//
// The checksum is verified here on every open rather than lazily, because a
// corrupted filter is not a benign performance problem: flipping a one to a
// zero turns a present key into a reported absence, which is indistinguishable
// from data loss at the read path.
func Decode(b []byte) (*Filter, error) {
	if len(b) < headerSize+4 {
		return nil, fmt.Errorf("%w: %d bytes, too short for a header and trailer", ErrCorruptFilter, len(b))
	}
	checksum := binary.LittleEndian.Uint32(b[len(b)-4:])
	covered := b[:len(b)-4]
	if crc32.Checksum(covered, crcTable) != checksum {
		return nil, fmt.Errorf("%w: checksum mismatch", ErrCorruptFilter)
	}

	f := &Filter{
		bitsPerKey: binary.LittleEndian.Uint32(covered[0:4]),
		probes:     binary.LittleEndian.Uint32(covered[4:8]),
	}
	arrayLen := binary.LittleEndian.Uint32(covered[8:12])
	if uint64(arrayLen) != uint64(len(covered)-headerSize) {
		return nil, fmt.Errorf("%w: bit_array_len is %d but %d bytes follow the header", ErrCorruptFilter, arrayLen, len(covered)-headerSize)
	}
	f.bits = covered[headerSize:]
	f.nbits = arrayLen * 8
	return f, nil
}

// MayContain reports whether key might be in the table.
//
// False means definitively absent; true means "read the block and find out".
//
// The two "no bits" cases answer opposite ways, and the distinction is the
// whole correctness argument for this method. A *decoded* filter with an empty
// bit array describes a table that really has no keys, so false is the true
// answer. A *nil* filter is the absence of information -- no filter was loaded
// -- and must answer true, because reporting absent on no evidence is a false
// negative, which at the read path is indistinguishable from losing the key.
func (f *Filter) MayContain(key []byte) bool {
	if f == nil {
		return true
	}
	if f.nbits == 0 || f.probes == 0 {
		return false
	}
	h := xxhash.Sum64(key)
	h1, h2 := uint32(h), uint32(h>>32)
	for i := uint32(0); i < f.probes; i++ {
		pos := h1 % f.nbits
		if f.bits[pos/8]&(1<<(pos%8)) == 0 {
			return false
		}
		h1 += h2
	}
	return true
}

// BitsPerKey reports the m/n the filter was built at.
func (f *Filter) BitsPerKey() int { return int(f.bitsPerKey) }

// Probes reports k, the number of hash probes per query.
func (f *Filter) Probes() int { return int(f.probes) }

// SizeBytes reports the size of the bit array.
func (f *Filter) SizeBytes() int { return len(f.bits) }
