package wal

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
)

// castagnoli is the checksum table docs/format.md §0 fixes for every structure
// in the format. Declared here rather than imported from the implementation so
// the tests check the spec rather than checking the code against itself.
var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// tempWAL creates an empty WAL file and returns it with its path.
func tempWAL(t *testing.T) (*os.File, string) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "000001.wal")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f, path
}

// writeBatches frames every batch into a fresh file and returns its path.
func writeBatches(t *testing.T, batches []*Batch) string {
	t.Helper()

	f, path := tempWAL(t)
	w := NewWriter(f)
	for i, b := range batches {
		if _, err := w.Write(b); err != nil {
			t.Fatalf("write batch %d: %v", i, err)
		}
	}
	if err := w.Sync(); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return path
}

// readAll reads every batch from path until a clean end of log.
func readAll(t *testing.T, path string) ([]Batch, error) {
	t.Helper()

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = f.Close() }()

	r := NewReader(f)
	var out []Batch
	for {
		b, err := r.Next()
		if err != nil {
			if isCleanEOF(err) {
				return out, nil
			}
			return out, err
		}
		out = append(out, b)
	}
}

// singleSet is the smallest useful batch.
func singleSet(seq uint64, key, value string) *Batch {
	b := &Batch{Sequence: seq}
	b.AppendSet([]byte(key), []byte(value))
	return b
}

func TestWriteReadSingleBatch(t *testing.T) {
	path := writeBatches(t, []*Batch{singleSet(1, "foo", "bar")})

	got, err := readAll(t, path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("read %d batches, want 1", len(got))
	}
	if got[0].Sequence != 1 {
		t.Errorf("Sequence = %d, want 1", got[0].Sequence)
	}
	if len(got[0].Records) != 1 {
		t.Fatalf("len(Records) = %d, want 1", len(got[0].Records))
	}
	rec := got[0].Records[0]
	if rec.Kind != KindSet || string(rec.Key) != "foo" || string(rec.Value) != "bar" {
		t.Errorf("record = %+v, want SET foo bar", rec)
	}
}

// TestDeleteCarriesNoValueField covers the §2.2 rule that a DELETE has no
// value field at all, not a zero-length one. A reader that always reads a
// value length will consume the next record's first byte and desynchronise.
func TestDeleteCarriesNoValueField(t *testing.T) {
	b := &Batch{Sequence: 10}
	b.AppendDelete([]byte("gone"))
	b.AppendSet([]byte("still"), []byte("here"))

	path := writeBatches(t, []*Batch{b})
	got, err := readAll(t, path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got) != 1 || len(got[0].Records) != 2 {
		t.Fatalf("got %+v, want one batch of two records", got)
	}

	if got[0].Records[0].Kind != KindDelete {
		t.Errorf("record 0 kind = %v, want KindDelete", got[0].Records[0].Kind)
	}
	if len(got[0].Records[0].Value) != 0 {
		t.Errorf("delete record carries value %q, want none", got[0].Records[0].Value)
	}
	// The SET after the DELETE is what catches a reader that consumed a
	// phantom value length.
	if r := got[0].Records[1]; string(r.Key) != "still" || string(r.Value) != "here" {
		t.Errorf("record 1 = %+v, want SET still here", r)
	}
}

// TestSequenceNumbersAreImplicit covers §2.2: only the batch's first sequence
// is stored, and the ith record's sequence is sequence+i.
func TestSequenceNumbersAreImplicit(t *testing.T) {
	b := &Batch{Sequence: 100}
	for i := 0; i < 5; i++ {
		b.AppendSet([]byte{byte('a' + i)}, []byte("v"))
	}

	path := writeBatches(t, []*Batch{b})
	got, err := readAll(t, path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got[0].Sequence != 100 {
		t.Errorf("Sequence = %d, want 100", got[0].Sequence)
	}
	if len(got[0].Records) != 5 {
		t.Errorf("len(Records) = %d, want 5", len(got[0].Records))
	}
}

func TestEmptyBatchIsLegal(t *testing.T) {
	// §2.2: record_count of zero is legal and recovery skips it.
	path := writeBatches(t, []*Batch{{Sequence: 7}})

	got, err := readAll(t, path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("read %d batches, want 1", len(got))
	}
	if len(got[0].Records) != 0 {
		t.Errorf("len(Records) = %d, want 0", len(got[0].Records))
	}
}

// TestRecordLargerThanBlockFragments is the core framing case: a payload
// bigger than a block must split into FIRST, MIDDLE..., LAST and reassemble
// byte-identically.
func TestRecordLargerThanBlockFragments(t *testing.T) {
	sizes := []int{
		BlockSize,         // exactly one block
		BlockSize + 1,     // one byte over
		BlockSize * 2,     // spans three blocks once headers are added
		BlockSize*3 + 517, // an awkward remainder
	}

	for _, size := range sizes {
		t.Run(itoa(int64(size)), func(t *testing.T) {
			value := bytes.Repeat([]byte{0xAB}, size)
			b := &Batch{Sequence: 1}
			b.AppendSet([]byte("big"), value)

			path := writeBatches(t, []*Batch{b})
			got, err := readAll(t, path)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			if len(got) != 1 || len(got[0].Records) != 1 {
				t.Fatalf("got %d batches, want 1 with 1 record", len(got))
			}
			if !bytes.Equal(got[0].Records[0].Value, value) {
				t.Errorf("value round trip differs: got %d bytes, want %d",
					len(got[0].Records[0].Value), len(value))
			}
		})
	}
}

// TestHeaderStraddlingBlockBoundary is the trap plan.md T2.1 names. When fewer
// than 7 bytes remain in a block the writer must zero-pad to the boundary
// rather than split a header across it, because a partial header cannot be
// validated and a reader cannot tell it from corruption.
//
// Each case sizes a first batch so the next fragment header would begin at
// BlockSize-6 through BlockSize-1 — every offset where a 7-byte header does
// not fit.
func TestHeaderStraddlingBlockBoundary(t *testing.T) {
	for gap := 1; gap <= 6; gap++ {
		t.Run("gap"+itoa(int64(gap)), func(t *testing.T) {
			// The first batch is padded so it ends exactly gap bytes before
			// the block boundary, leaving too little room for a header.
			filler := blockFillerSize(t, gap)

			first := &Batch{Sequence: 1}
			first.AppendSet([]byte("pad"), bytes.Repeat([]byte{0x5A}, filler))

			second := &Batch{Sequence: 2}
			second.AppendSet([]byte("after"), []byte("boundary"))

			path := writeBatches(t, []*Batch{first, second})
			got, err := readAll(t, path)
			if err != nil {
				t.Fatalf("gap %d: read: %v", gap, err)
			}
			if len(got) != 2 {
				t.Fatalf("gap %d: read %d batches, want 2", gap, len(got))
			}
			if r := got[1].Records[0]; string(r.Key) != "after" || string(r.Value) != "boundary" {
				t.Errorf("gap %d: second batch = %+v, want SET after boundary", gap, r)
			}

			// The padding must be zero bytes: §2.1 relies on type 0 being
			// invalid so a reader can recognise padding unambiguously.
			assertPaddingIsZero(t, path)
		})
	}
}

// blockFillerSize computes the value length that leaves exactly gap bytes free
// at the end of the first block. It encodes an empty batch to measure the
// batch and record overhead rather than hardcoding it, so the test survives an
// encoding change.
func blockFillerSize(t *testing.T, gap int) int {
	t.Helper()

	probe := &Batch{Sequence: 1}
	probe.AppendSet([]byte("pad"), nil)
	overhead := len(probe.Encode(nil))

	filler := BlockSize - HeaderSize - overhead - gap
	if filler < 0 {
		t.Fatalf("cannot leave a %d byte gap: overhead %d exceeds the block", gap, overhead)
	}
	return filler
}

// assertPaddingIsZero checks every byte of trailing padding in each full block.
func assertPaddingIsZero(t *testing.T, path string) {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}

	for start := 0; start+BlockSize <= len(data); start += BlockSize {
		block := data[start : start+BlockSize]
		// Walk the fragments in this block to find where padding begins.
		off := 0
		for off+HeaderSize <= BlockSize {
			length := int(binary.LittleEndian.Uint16(block[off+4 : off+6]))
			typ := block[off+6]
			if typ == 0 {
				break // padding starts here
			}
			off += HeaderSize + length
		}
		for i := off; i < BlockSize; i++ {
			if block[i] != 0 {
				t.Fatalf("padding at block offset %d is %#x, want zero", i, block[i])
			}
		}
	}
}

// TestChecksumCoversTypeAndPayloadOnly pins the exact covered range from §2.1.
// A checksum over the wrong bytes still round-trips through its own reader,
// so only an independent computation catches it.
func TestChecksumCoversTypeAndPayloadOnly(t *testing.T) {
	path := writeBatches(t, []*Batch{singleSet(1, "k", "v")})

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	if len(data) < HeaderSize {
		t.Fatalf("file is %d bytes, too short for a header", len(data))
	}

	stored := binary.LittleEndian.Uint32(data[0:4])
	length := int(binary.LittleEndian.Uint16(data[4:6]))
	typ := data[6]

	if FragmentType(typ) != FragmentFull {
		t.Errorf("type = %d, want FragmentFull (%d)", typ, FragmentFull)
	}
	if HeaderSize+length > len(data) {
		t.Fatalf("declared length %d runs past the %d byte file", length, len(data))
	}

	// The checksum covers the type byte then the payload, and neither the CRC
	// field nor the length field.
	want := crc32.Checksum(data[6:6+1+length], castagnoli)
	if stored != want {
		t.Errorf("stored crc = %#08x, want %#08x over type+payload", stored, want)
	}
}

// TestTenThousandRandomRecords is T2.1's done-when condition: 10,000 records
// of randomly varying size, including records larger than a block, write and
// read back byte-identical.
func TestTenThousandRandomRecords(t *testing.T) {
	if testing.Short() {
		t.Skip("writes tens of megabytes; skipped under -short")
	}

	const count = 10000
	rng := rand.New(rand.NewSource(20260819))

	batches := make([]*Batch, 0, count)
	for i := 0; i < count; i++ {
		b := &Batch{Sequence: uint64(i + 1)}

		// Mostly small, with a deliberate tail of multi-block records so the
		// fragmentation path is exercised thousands of times rather than once.
		var size int
		switch n := rng.Intn(100); {
		case n < 80:
			size = rng.Intn(128)
		case n < 97:
			size = rng.Intn(4096)
		default:
			size = BlockSize + rng.Intn(2*BlockSize)
		}

		value := make([]byte, size)
		if _, err := rng.Read(value); err != nil {
			t.Fatalf("rand: %v", err)
		}
		key := []byte("key-" + itoa(int64(i)))

		if rng.Intn(10) == 0 {
			b.AppendDelete(key)
		} else {
			b.AppendSet(key, value)
		}
		batches = append(batches, b)
	}

	path := writeBatches(t, batches)
	got, err := readAll(t, path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(got) != count {
		t.Fatalf("read %d batches, want %d", len(got), count)
	}

	for i, want := range batches {
		g := got[i]
		if g.Sequence != want.Sequence {
			t.Fatalf("batch %d: Sequence = %d, want %d", i, g.Sequence, want.Sequence)
		}
		if len(g.Records) != len(want.Records) {
			t.Fatalf("batch %d: %d records, want %d", i, len(g.Records), len(want.Records))
		}
		for j := range want.Records {
			wr, gr := want.Records[j], g.Records[j]
			if gr.Kind != wr.Kind {
				t.Fatalf("batch %d record %d: kind = %v, want %v", i, j, gr.Kind, wr.Kind)
			}
			if !bytes.Equal(gr.Key, wr.Key) {
				t.Fatalf("batch %d record %d: key = %q, want %q", i, j, gr.Key, wr.Key)
			}
			if wr.Kind == KindSet && !bytes.Equal(gr.Value, wr.Value) {
				t.Fatalf("batch %d record %d: value differs (%d bytes, want %d)",
					i, j, len(gr.Value), len(wr.Value))
			}
		}
	}
}

// TestOffsetTracksBytesWritten checks the Syncable contract: Offset must
// reflect every byte handed to the OS, since Syncer compares waiter offsets
// against it to decide who a given fsync covered.
func TestOffsetTracksBytesWritten(t *testing.T) {
	f, path := tempWAL(t)
	w := NewWriter(f)

	var last int64
	for i := 0; i < 50; i++ {
		end, err := w.Write(singleSet(uint64(i+1), "k", "value"))
		if err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
		if end <= last {
			t.Fatalf("write %d: end offset %d did not advance past %d", i, end, last)
		}
		if got := w.Offset(); got != end {
			t.Errorf("write %d: Offset() = %d, want %d", i, got, end)
		}
		last = end
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if st.Size() != last {
		t.Errorf("file is %d bytes, but the last offset was %d", st.Size(), last)
	}
}

// TestWriterSatisfiesSyncable is a compile-time check that the T2.1 writer
// plugs into the T2.2 syncer without an adapter.
func TestWriterSatisfiesSyncable(t *testing.T) {
	f, _ := tempWAL(t)
	var s Syncable = NewWriter(f)
	if s.Offset() != 0 {
		t.Errorf("Offset() on a fresh writer = %d, want 0", s.Offset())
	}
}
