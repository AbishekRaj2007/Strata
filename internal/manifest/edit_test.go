package manifest

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
)

func TestEditRoundTrip(t *testing.T) {
	var e VersionEdit
	e.AddFile(0, &FileMetadata{
		Number: 7, Size: 4096,
		Smallest: []byte("apple"), Largest: []byte("mango"),
		SmallestSeq: 10, LargestSeq: 99,
	})
	e.AddFile(3, &FileMetadata{
		Number: 8, Size: 1 << 20,
		Smallest: []byte("nectarine"), Largest: []byte("zucchini"),
		SmallestSeq: 100, LargestSeq: 200,
	})
	e.DeleteFile(2, 5)
	e.DeleteFile(2, 6)
	e.SetLogNumber(12)
	e.SetNextFileNumber(13)
	e.SetLastSequence(200)

	got, err := DecodeEdit(e.Encode(nil))
	if err != nil {
		t.Fatalf("DecodeEdit: %v", err)
	}

	if len(got.Added) != 2 || len(got.Deleted) != 2 {
		t.Fatalf("got %d added and %d deleted, want 2 and 2", len(got.Added), len(got.Deleted))
	}
	if got.Added[0].Level != 0 || got.Added[0].Meta.Number != 7 {
		t.Errorf("first addition = %+v", got.Added[0])
	}
	if !bytes.Equal(got.Added[0].Meta.Smallest, []byte("apple")) {
		t.Errorf("smallest = %q, want %q", got.Added[0].Meta.Smallest, "apple")
	}
	if got.Added[1].Meta.Size != 1<<20 {
		t.Errorf("size = %d, want %d", got.Added[1].Meta.Size, 1<<20)
	}
	if got.Deleted[0] != (DeletedFile{Level: 2, Number: 5}) {
		t.Errorf("first deletion = %+v", got.Deleted[0])
	}
	for _, tc := range []struct {
		name string
		got  *uint64
		want uint64
	}{
		{"LogNumber", got.LogNumber, 12},
		{"NextFileNumber", got.NextFileNumber, 13},
		{"LastSequence", got.LastSequence, 200},
	} {
		if tc.got == nil {
			t.Errorf("%s is nil, want %d", tc.name, tc.want)
		} else if *tc.got != tc.want {
			t.Errorf("%s = %d, want %d", tc.name, *tc.got, tc.want)
		}
	}
}

// TestZeroCountersSurviveRoundTrip is why the counters are pointers. A last
// sequence of zero is a real value for a fresh database, and encoding "set to
// zero" the same way as "not set" loses it on replay.
func TestZeroCountersSurviveRoundTrip(t *testing.T) {
	var e VersionEdit
	e.SetLogNumber(0)
	e.SetNextFileNumber(0)
	e.SetLastSequence(0)

	got, err := DecodeEdit(e.Encode(nil))
	if err != nil {
		t.Fatalf("DecodeEdit: %v", err)
	}
	if got.LogNumber == nil || *got.LogNumber != 0 {
		t.Errorf("LogNumber = %v, want a set zero", got.LogNumber)
	}
	if got.NextFileNumber == nil || *got.NextFileNumber != 0 {
		t.Errorf("NextFileNumber = %v, want a set zero", got.NextFileNumber)
	}
	if got.LastSequence == nil || *got.LastSequence != 0 {
		t.Errorf("LastSequence = %v, want a set zero", got.LastSequence)
	}
}

func TestUnsetCountersDecodeAsNil(t *testing.T) {
	var e VersionEdit
	e.DeleteFile(1, 4)

	got, err := DecodeEdit(e.Encode(nil))
	if err != nil {
		t.Fatalf("DecodeEdit: %v", err)
	}
	if got.LogNumber != nil || got.NextFileNumber != nil || got.LastSequence != nil {
		t.Errorf("counters = %v/%v/%v, want all nil", got.LogNumber, got.NextFileNumber, got.LastSequence)
	}
}

// TestDeletionsEncodeBeforeAdditions pins the ordering replay depends on. A
// compaction rewriting a file into the level it came from would otherwise
// delete the file it had just added.
func TestDeletionsEncodeBeforeAdditions(t *testing.T) {
	var e VersionEdit
	e.AddFile(1, meta(9, "a", "b"))
	e.DeleteFile(1, 9)

	kind := EditKind(binary.LittleEndian.Uint32(e.Encode(nil)))
	if kind != EditDeleteFile {
		t.Errorf("first encoded edit is %s, want %s", kind, EditDeleteFile)
	}
}

func TestEmptyEdit(t *testing.T) {
	var e VersionEdit
	if !e.IsEmpty() {
		t.Error("a fresh edit reports non-empty")
	}
	if len(e.Encode(nil)) != 0 {
		t.Error("an empty edit encoded to bytes")
	}

	got, err := DecodeEdit(nil)
	if err != nil {
		t.Fatalf("DecodeEdit(nil): %v", err)
	}
	if !got.IsEmpty() {
		t.Error("decoding an empty payload produced a non-empty edit")
	}

	e.SetLastSequence(0)
	if e.IsEmpty() {
		t.Error("an edit setting a counter to zero reports empty")
	}
}

func TestEmptyKeyBoundsRoundTrip(t *testing.T) {
	var e VersionEdit
	e.AddFile(0, &FileMetadata{Number: 1, Smallest: []byte(""), Largest: []byte("")})

	got, err := DecodeEdit(e.Encode(nil))
	if err != nil {
		t.Fatalf("DecodeEdit: %v", err)
	}
	if len(got.Added[0].Meta.Smallest) != 0 || len(got.Added[0].Meta.Largest) != 0 {
		t.Errorf("bounds = %q/%q, want empty", got.Added[0].Meta.Smallest, got.Added[0].Meta.Largest)
	}
}

// TestDecodedKeysDoNotAliasThePayload matters because the payload is a reused
// framing block. A version holding a slice into it would see its keys change
// underneath it on the next read.
func TestDecodedKeysDoNotAliasThePayload(t *testing.T) {
	var e VersionEdit
	e.AddFile(0, meta(1, "alpha", "omega"))
	payload := e.Encode(nil)

	got, err := DecodeEdit(payload)
	if err != nil {
		t.Fatalf("DecodeEdit: %v", err)
	}
	for i := range payload {
		payload[i] = 0xff
	}
	if !bytes.Equal(got.Added[0].Meta.Smallest, []byte("alpha")) {
		t.Errorf("smallest = %q after overwriting the payload; it aliased the buffer", got.Added[0].Meta.Smallest)
	}
}

func TestDecodeRejectsMalformed(t *testing.T) {
	var valid VersionEdit
	valid.AddFile(0, meta(1, "a", "z"))
	validBytes := valid.Encode(nil)

	tests := []struct {
		name    string
		payload []byte
	}{
		{"unknown edit type", binary.LittleEndian.AppendUint32(nil, 99)},
		{"edit type zero", binary.LittleEndian.AppendUint32(nil, 0)},
		{"truncated edit type", []byte{1, 0}},
		{"level out of range", func() []byte {
			p := binary.LittleEndian.AppendUint32(nil, uint32(EditDeleteFile))
			p = binary.LittleEndian.AppendUint32(p, NumLevels)
			return binary.LittleEndian.AppendUint64(p, 1)
		}()},
		{"truncated DELETE_FILE", binary.LittleEndian.AppendUint32(nil, uint32(EditDeleteFile))},
		{"truncated counter", binary.LittleEndian.AppendUint32(nil, uint32(EditSetLastSequence))},
		{"inverted key bounds", func() []byte {
			var e VersionEdit
			e.AddFile(0, &FileMetadata{Number: 1, Smallest: []byte("z"), Largest: []byte("a")})
			return e.Encode(nil)
		}()},
		{"key length past the end", func() []byte {
			p := binary.LittleEndian.AppendUint32(nil, uint32(EditAddFile))
			p = binary.LittleEndian.AppendUint32(p, 0)
			p = binary.LittleEndian.AppendUint64(p, 1)
			p = binary.LittleEndian.AppendUint64(p, 0)
			return binary.AppendUvarint(p, 9999)
		}()},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := DecodeEdit(tc.payload); !errors.Is(err, ErrCorrupt) {
				t.Errorf("DecodeEdit = %v, want an ErrCorrupt", err)
			}
		})
	}

	// Every truncation of a valid payload must be corruption, never a silent
	// partial decode.
	for n := 1; n < len(validBytes); n++ {
		if _, err := DecodeEdit(validBytes[:n]); err == nil {
			t.Errorf("DecodeEdit of a %d-byte prefix succeeded, want an error", n)
		}
	}
}

// TestDecodeRejectsNonMinimalUvarint covers a case binary.Uvarint accepts on
// its own: 0x80 0x00 decodes to zero in two bytes where one would do. Two
// encodings of the same value mean two valid byte representations of the same
// manifest, which docs/format.md §0 forbids.
func TestDecodeRejectsNonMinimalUvarint(t *testing.T) {
	// ADD_FILE header, then a two-byte encoding of a zero-length smallest key.
	p := binary.LittleEndian.AppendUint32(nil, uint32(EditAddFile))
	p = binary.LittleEndian.AppendUint32(p, 0)
	p = binary.LittleEndian.AppendUint64(p, 1)
	p = binary.LittleEndian.AppendUint64(p, 0)
	p = append(p, 0x80, 0x00)

	if _, err := DecodeEdit(p); !errors.Is(err, ErrCorrupt) {
		t.Errorf("DecodeEdit with a non-minimal uvarint = %v, want an ErrCorrupt", err)
	}

	// The minimal encoding of the same value must still be accepted.
	q := binary.LittleEndian.AppendUint32(nil, uint32(EditAddFile))
	q = binary.LittleEndian.AppendUint32(q, 0)
	q = binary.LittleEndian.AppendUint64(q, 1)
	q = binary.LittleEndian.AppendUint64(q, 0)
	q = append(q, 0x00, 0x00)
	q = binary.LittleEndian.AppendUint64(q, 0)
	q = binary.LittleEndian.AppendUint64(q, 0)

	if _, err := DecodeEdit(q); err != nil {
		t.Errorf("DecodeEdit with minimal uvarints = %v, want nil", err)
	}
}

func TestEditKindString(t *testing.T) {
	tests := []struct {
		kind EditKind
		want string
	}{
		{EditAddFile, "ADD_FILE"},
		{EditDeleteFile, "DELETE_FILE"},
		{EditSetLogNumber, "SET_LOG_NUMBER"},
		{EditSetNextFileNumber, "SET_NEXT_FILE_NUMBER"},
		{EditSetLastSequence, "SET_LAST_SEQUENCE"},
		{EditKind(42), "EditKind(42)"},
	}
	for _, tc := range tests {
		if got := tc.kind.String(); got != tc.want {
			t.Errorf("EditKind(%d) = %q, want %q", uint32(tc.kind), got, tc.want)
		}
	}
}
