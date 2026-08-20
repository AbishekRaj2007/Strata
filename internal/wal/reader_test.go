package wal

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// isCleanEOF reports whether err is the clean end-of-log signal rather than
// corruption. §2.3 makes this distinction a correctness requirement: treating
// corruption as a clean end silently discards acknowledged writes, and
// treating a truncated tail as corruption rejects healthy databases.
func isCleanEOF(err error) bool {
	return errors.Is(err, io.EOF)
}

// buildWAL writes batches to a new file and returns the path and raw bytes.
func buildWAL(t *testing.T, batches []*Batch) (string, []byte) {
	t.Helper()

	path := writeBatches(t, batches)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	return path, data
}

// writeRaw writes exact bytes to a new WAL file, for tests that construct
// malformed framing directly.
func writeRaw(t *testing.T, data []byte) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "000001.wal")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write raw: %v", err)
	}
	return path
}

// frame builds one well-formed fragment with a correct checksum.
func frame(typ FragmentType, payload []byte) []byte {
	out := make([]byte, HeaderSize+len(payload))
	binary.LittleEndian.PutUint16(out[4:6], uint16(len(payload)))
	out[6] = byte(typ)
	copy(out[7:], payload)
	binary.LittleEndian.PutUint32(out[0:4], crc32.Checksum(out[6:], castagnoli))
	return out
}

// TestTruncateAtEveryOffset is T2.3's done-when condition, and plan.md notes
// it is twenty lines that find real bugs. Truncating a healthy WAL at every
// byte offset must always produce either full recovery or clean recovery of a
// prefix — never a panic, never a partial record, never a spurious corruption
// error.
//
// Every offset matters because the interesting failures are all at boundaries:
// mid-header, mid-payload, mid-checksum, and exactly at a block edge.
func TestTruncateAtEveryOffset(t *testing.T) {
	batches := []*Batch{
		singleSet(1, "alpha", "one"),
		singleSet(2, "beta", "two"),
	}
	// A multi-block record guarantees FIRST/MIDDLE/LAST sequences are present,
	// so truncation lands inside a fragmented record as well as between them.
	big := &Batch{Sequence: 3}
	big.AppendSet([]byte("large"), make([]byte, BlockSize+1024))
	batches = append(batches, big, singleSet(4, "gamma", "three"))

	_, full := buildWAL(t, batches)

	for cut := 0; cut <= len(full); cut++ {
		path := writeRaw(t, full[:cut])

		got, err := readAll(t, path)
		if err != nil {
			t.Fatalf("truncated at %d/%d: %v; want clean recovery of a prefix",
				cut, len(full), err)
		}

		// Whatever survived must be a prefix of what was written: recovery may
		// stop early, but it must never invent or reorder records.
		if len(got) > len(batches) {
			t.Fatalf("truncated at %d: recovered %d batches, more than the %d written",
				cut, len(got), len(batches))
		}
		for i, b := range got {
			if b.Sequence != batches[i].Sequence {
				t.Fatalf("truncated at %d: batch %d sequence = %d, want %d",
					cut, i, b.Sequence, batches[i].Sequence)
			}
			if len(b.Records) != len(batches[i].Records) {
				t.Fatalf("truncated at %d: batch %d has %d records, want %d — a partial record was returned",
					cut, i, len(b.Records), len(batches[i].Records))
			}
		}
	}
}

// TestCleanEndOfLog covers the four §2.3 situations that are the expected
// result of a crash mid-write and must be accepted silently.
func TestCleanEndOfLog(t *testing.T) {
	_, healthy := buildWAL(t, []*Batch{singleSet(1, "k", "v")})

	tests := []struct {
		name string
		data []byte
	}{
		{
			// Fewer than 7 bytes remain, so no header can be read.
			name: "truncated header",
			data: append(append([]byte{}, healthy...), 0x01, 0x02, 0x03),
		},
		{
			// A header claiming more payload than the file contains.
			name: "declared length runs past EOF",
			data: append(append([]byte{}, healthy...), frame(FragmentFull, make([]byte, 500))[:20]...),
		},
		{
			name: "checksum failure in the final fragment",
			data: corruptLastFragment(healthy),
		},
		{
			// A FIRST fragment whose LAST never arrived.
			name: "FIRST with no follower",
			data: append(append([]byte{}, healthy...), frame(FragmentFirst, []byte("orphan"))...),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := readAll(t, writeRaw(t, tt.data))
			if err != nil {
				t.Fatalf("got error %v, want a clean end of log", err)
			}
			// The healthy record before the damage must still be recovered.
			if len(got) != 1 {
				t.Errorf("recovered %d batches, want the 1 valid record before the tail damage", len(got))
			}
		})
	}
}

// corruptLastFragment flips a payload byte in the final fragment, leaving the
// header intact so the failure is a checksum mismatch rather than a framing
// error.
func corruptLastFragment(data []byte) []byte {
	out := append([]byte{}, data...)
	if len(out) > 0 {
		out[len(out)-1] ^= 0xFF
	}
	return out
}

// TestCorruptionIsReportedNotSkipped covers the other half of §2.3: damage
// with valid data after it is real corruption. Silently skipping it loses
// acknowledged writes without telling anyone, which is the failure mode this
// project exists to avoid.
func TestCorruptionIsReportedNotSkipped(t *testing.T) {
	// Two healthy records; the first is damaged, so the second proves the
	// damage was mid-log rather than at the tail.
	_, healthy := buildWAL(t, []*Batch{
		singleSet(1, "first", "value-one"),
		singleSet(2, "second", "value-two"),
	})

	tests := []struct {
		name string
		data []byte
	}{
		{
			name: "checksum failure with valid records after it",
			data: flipByteInFirstPayload(healthy),
		},
		{
			name: "invalid fragment type",
			data: withFragmentType(healthy, 5),
		},
		{
			name: "fragment type zero outside padding",
			data: withFragmentType(healthy, 0),
		},
		{
			name: "orphaned LAST fragment",
			data: append(frame(FragmentLast, []byte("no first")), healthy...),
		},
		{
			name: "orphaned MIDDLE fragment",
			data: append(frame(FragmentMiddle, []byte("no first")), healthy...),
		},
		{
			name: "FIRST immediately followed by FIRST",
			data: append(
				append(frame(FragmentFirst, []byte("a")), frame(FragmentFirst, []byte("b"))...),
				frame(FragmentLast, []byte("c"))...,
			),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := readAll(t, writeRaw(t, tt.data))
			if err == nil {
				t.Fatal("recovery succeeded, want corruption to be reported")
			}
			if isCleanEOF(err) {
				t.Fatalf("error = %v, want corruption rather than a clean end of log", err)
			}
			if !errors.Is(err, ErrCorrupt) {
				t.Errorf("error = %v, want it to wrap ErrCorrupt", err)
			}
		})
	}
}

// flipByteInFirstPayload damages the first fragment's payload, leaving its
// header valid so the checksum is what fails.
func flipByteInFirstPayload(data []byte) []byte {
	out := append([]byte{}, data...)
	if len(out) > HeaderSize {
		out[HeaderSize] ^= 0xFF
	}
	return out
}

// withFragmentType rewrites the first fragment's type byte and repairs the
// checksum, so the type is what the reader rejects rather than a mismatch.
func withFragmentType(data []byte, typ byte) []byte {
	out := append([]byte{}, data...)
	if len(out) <= HeaderSize {
		return out
	}
	length := int(binary.LittleEndian.Uint16(out[4:6]))
	if HeaderSize+length > len(out) {
		return out
	}
	out[6] = typ
	binary.LittleEndian.PutUint32(out[0:4], crc32.Checksum(out[6:6+1+length], castagnoli))
	return out
}

// TestMalformedBatchPayloadIsCorruption covers the §2.3 rule that a payload
// failing to decode behind a *valid* checksum is corruption, not truncation:
// the checksum proves the bytes arrived intact, so the writer produced
// something invalid.
func TestMalformedBatchPayloadIsCorruption(t *testing.T) {
	tests := []struct {
		name    string
		payload []byte
	}{
		{
			// Claims one record but carries no record bytes.
			name:    "record count exceeds payload",
			payload: batchHeader(1, 1),
		},
		{
			// Claims zero records but carries trailing bytes.
			name:    "trailing bytes after the last record",
			payload: append(batchHeader(1, 0), 0xFF, 0xFF),
		},
		{
			name:    "truncated batch header",
			payload: []byte{1, 2, 3},
		},
		{
			// A key length larger than the remaining payload.
			name: "key length runs past the payload",
			payload: append(append(batchHeader(1, 1), byte(KindSet)),
				0xFF, 0xFF, 0xFF, 0x7F),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Framed with a correct checksum, so only the payload decode can fail.
			_, err := readAll(t, writeRaw(t, frame(FragmentFull, tt.payload)))
			if err == nil {
				t.Fatal("decode succeeded, want corruption")
			}
			if !errors.Is(err, ErrCorrupt) {
				t.Errorf("error = %v, want it to wrap ErrCorrupt", err)
			}
		})
	}
}

// batchHeader builds the fixed part of a §2.2 batch payload.
func batchHeader(seq uint64, count uint32) []byte {
	out := make([]byte, 12)
	binary.LittleEndian.PutUint64(out[0:8], seq)
	binary.LittleEndian.PutUint32(out[8:12], count)
	return out
}

// TestBatchRoundTripThroughEncoder checks Encode and DecodeBatch agree without
// involving the framing layer, so a failure localises to one of the two.
func TestBatchRoundTripThroughEncoder(t *testing.T) {
	b := &Batch{Sequence: 42}
	b.AppendSet([]byte("k1"), []byte("v1"))
	b.AppendDelete([]byte("k2"))
	b.AppendSet([]byte("k3"), nil)

	got, err := DecodeBatch(b.Encode(nil))
	if err != nil {
		t.Fatalf("DecodeBatch: %v", err)
	}
	if got.Sequence != 42 {
		t.Errorf("Sequence = %d, want 42", got.Sequence)
	}
	if len(got.Records) != 3 {
		t.Fatalf("len(Records) = %d, want 3", len(got.Records))
	}
	if got.Records[1].Kind != KindDelete {
		t.Errorf("record 1 kind = %v, want KindDelete", got.Records[1].Kind)
	}
	// An empty value is distinct from a delete, exactly as in RESP.
	if got.Records[2].Kind != KindSet || len(got.Records[2].Value) != 0 {
		t.Errorf("record 2 = %+v, want an empty SET", got.Records[2])
	}
}

// TestRecoverReplaysFilesInOrder covers §2.3: WAL files replay in ascending
// file-number order, and the sequence counter is restored to the highest
// sequence seen.
func TestRecoverReplaysFilesInOrder(t *testing.T) {
	dir := t.TempDir()

	// Deliberately created out of order so a reader sorting by mtime or by
	// readdir order rather than by file number fails here.
	for _, spec := range []struct {
		name string
		seq  uint64
		key  string
	}{
		{"000007.wal", 30, "third"},
		{"000002.wal", 10, "first"},
		{"000004.wal", 20, "second"},
	} {
		f, err := os.Create(filepath.Join(dir, spec.name))
		if err != nil {
			t.Fatalf("create %s: %v", spec.name, err)
		}
		w := NewWriter(f)
		if _, err := w.Write(singleSet(spec.seq, spec.key, "v")); err != nil {
			t.Fatalf("write %s: %v", spec.name, err)
		}
		if err := w.Close(); err != nil {
			t.Fatalf("close %s: %v", spec.name, err)
		}
	}

	var seen []string
	highest, err := Recover(dir, func(seq uint64, rec Record) error {
		seen = append(seen, fmt.Sprintf("%d:%s", seq, rec.Key))
		return nil
	})
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}

	want := []string{"10:first", "20:second", "30:third"}
	if len(seen) != len(want) {
		t.Fatalf("replayed %v, want %v", seen, want)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Errorf("replay %d = %s, want %s", i, seen[i], want[i])
		}
	}
	if highest != 30 {
		t.Errorf("highest sequence = %d, want 30", highest)
	}
}

// TestTruncationInAnEarlierFileIsCorruption covers the last §2.3 rule: only
// the highest-numbered WAL may end mid-record. An earlier file that is
// truncated means damage, because a later file exists only if the earlier one
// was completed.
func TestTruncationInAnEarlierFileIsCorruption(t *testing.T) {
	dir := t.TempDir()

	first := filepath.Join(dir, "000001.wal")
	f, err := os.Create(first)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	w := NewWriter(f)
	if _, err := w.Write(singleSet(1, "a", "one")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Chop the earlier file mid-record.
	data, err := os.ReadFile(first)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if err := os.WriteFile(first, data[:len(data)-2], 0o600); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	second := filepath.Join(dir, "000002.wal")
	f2, err := os.Create(second)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	w2 := NewWriter(f2)
	if _, err := w2.Write(singleSet(2, "b", "two")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := w2.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	_, err = Recover(dir, func(uint64, Record) error { return nil })
	if err == nil {
		t.Fatal("Recover succeeded, want corruption from the truncated earlier file")
	}
	if !errors.Is(err, ErrCorrupt) {
		t.Errorf("error = %v, want it to wrap ErrCorrupt", err)
	}
}

// TestRecoverPropagatesApplyError checks that a failure in the apply callback
// stops recovery rather than being swallowed, since apply is what populates
// the memtable in T3.5.
func TestRecoverPropagatesApplyError(t *testing.T) {
	dir := t.TempDir()
	f, err := os.Create(filepath.Join(dir, "000001.wal"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	w := NewWriter(f)
	if _, err := w.Write(singleSet(1, "k", "v")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	sentinel := errors.New("memtable full")
	if _, err := Recover(dir, func(uint64, Record) error { return sentinel }); !errors.Is(err, sentinel) {
		t.Errorf("Recover error = %v, want it to wrap the apply error", err)
	}
}

// FuzzFramingReader asserts the reader never panics on arbitrary bytes. A WAL
// is read at startup, before anything else works, so a panic here is an
// unrecoverable database rather than a failed request.
func FuzzFramingReader(f *testing.F) {
	_, healthy := buildWAL(&testing.T{}, []*Batch{singleSet(1, "k", "v")})
	f.Add(healthy)
	f.Add(frame(FragmentFull, []byte("payload")))
	f.Add(frame(FragmentFirst, []byte("a")))
	f.Add(frame(FragmentMiddle, []byte("b")))
	f.Add(frame(FragmentLast, []byte("c")))
	f.Add(make([]byte, BlockSize))
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, data []byte) {
		path := filepath.Join(t.TempDir(), "fuzz.wal")
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Skip()
		}
		file, err := os.Open(path)
		if err != nil {
			t.Skip()
		}
		defer func() { _ = file.Close() }()

		r := NewReader(file)
		for i := 0; i < 1000; i++ {
			if _, err := r.Next(); err != nil {
				return
			}
		}
	})
}
