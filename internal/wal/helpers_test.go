package wal

import (
	"encoding/binary"
	"hash/crc32"
	"os"
	"strconv"
	"testing"
)

// castagnoli is the checksum table docs/format.md §0 fixes for every structure
// in the format. Declared here rather than imported from the implementation so
// the tests check the spec rather than checking the code against itself.
var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// itoa names subtests by size. Shared by the framing, recovery, and syncer
// suites, so it lives here rather than in whichever file happened to need it
// first.
func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// fragment is one framed record as it appears on disk, with the offset it was
// found at so failures name a position rather than an index.
type fragment struct {
	Offset int
	Type   FragmentType
	Length int
}

// walkBlock returns every fragment in one 32 KiB block, stopping at the first
// padding byte, and reports the offset where padding begins.
//
// It fails the test rather than returning on malformed framing. An earlier
// version advanced by HeaderSize+length unchecked, so a bogus length walked
// the cursor past the block, the caller's trailing-zero check then ran over an
// empty range, and the test passed green on a broken writer. A helper that
// cannot fail is worse than no helper.
func walkBlock(t *testing.T, block []byte, blockStart int) ([]fragment, int) {
	t.Helper()

	if len(block) != BlockSize {
		t.Fatalf("block at %d is %d bytes, want %d", blockStart, len(block), BlockSize)
	}

	var frags []fragment
	off := 0
	for off+HeaderSize <= BlockSize {
		length := int(binary.LittleEndian.Uint16(block[off+4 : off+6]))
		typ := block[off+6]
		if typ == 0 {
			// §2.1: type 0 is never valid, which is what makes zero padding
			// unambiguous. Padding starts here.
			return frags, off
		}

		if FragmentType(typ) < FragmentFull || FragmentType(typ) > FragmentLast {
			t.Fatalf("fragment at %d: type %d is not one of FULL/FIRST/MIDDLE/LAST",
				blockStart+off, typ)
		}
		// §2.1: a header is 7 bytes, so no payload can exceed 32761 without
		// leaving its own header no room.
		if length > MaxPayloadSize {
			t.Fatalf("fragment at %d: payload length %d exceeds MaxPayloadSize %d",
				blockStart+off, length, MaxPayloadSize)
		}
		if off+HeaderSize+length > BlockSize {
			t.Fatalf("fragment at %d: header+payload (%d) runs past the block end (%d bytes left)",
				blockStart+off, HeaderSize+length, BlockSize-off)
		}

		frags = append(frags, fragment{Offset: blockStart + off, Type: FragmentType(typ), Length: length})
		off += HeaderSize + length
	}
	return frags, off
}

// assertPaddingIsZero checks every byte of trailing padding in each full block,
// and validates the framing of every fragment it walks past on the way.
func assertPaddingIsZero(t *testing.T, path string) {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}

	for start := 0; start+BlockSize <= len(data); start += BlockSize {
		block := data[start : start+BlockSize]
		_, padStart := walkBlock(t, block, start)

		for i := padStart; i < BlockSize; i++ {
			if block[i] != 0 {
				t.Fatalf("padding at block offset %d (file offset %d) is %#x, want zero",
					i, start+i, block[i])
			}
		}
	}
}
