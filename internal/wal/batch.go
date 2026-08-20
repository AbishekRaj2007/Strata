package wal

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// ErrCorrupt is the class every real-damage error wraps. Distinct from a clean
// end of log (io.EOF); that distinction is the whole of docs/format.md §2.3.
var ErrCorrupt = errors.New("wal corruption")

type Kind uint8

const (
	KindSet    Kind = 0
	KindDelete Kind = 1
)

// Record is one key operation within a batch.
type Record struct {
	Kind  Kind
	Key   []byte
	Value []byte // nil and unread when Kind is KindDelete
}

// Batch is one atomically-logged group of records. Only the first record's
// sequence is stored; the ith record's sequence is Sequence + i.
type Batch struct {
	Sequence uint64
	Records  []Record
}

func (b *Batch) AppendSet(key, value []byte) {
	b.Records = append(b.Records, Record{Kind: KindSet, Key: key, Value: value})
}

func (b *Batch) AppendDelete(key []byte) {
	b.Records = append(b.Records, Record{Kind: KindDelete, Key: key, Value: nil})
}

// Encode appends the §2.2 wire form to dst and returns it.
func (b *Batch) Encode(dst []byte) []byte {
	dst = binary.LittleEndian.AppendUint64(dst, b.Sequence)
	dst = binary.LittleEndian.AppendUint32(dst, uint32(len(b.Records)))

	for _, rec := range b.Records {
		dst = append(dst, byte(rec.Kind))
		dst = binary.AppendUvarint(dst, uint64(len(rec.Key)))
		dst = append(dst, rec.Key...)

		// A DELETE carries no value field at all -- not a zero-length one --
		// so a reader must branch on kind before reading further.
		if rec.Kind == KindSet {
			dst = binary.AppendUvarint(dst, uint64(len(rec.Value)))
			dst = append(dst, rec.Value...)
		}
	}

	return dst
}

// DecodeBatch parses the §2.2 wire form. Both a short payload and trailing
// bytes are corruption: the checksum already proved the bytes arrived intact,
// so anything malformed means the writer emitted something invalid.
func DecodeBatch(payload []byte) (Batch, error) {
	if len(payload) < 12 {
		return Batch{}, fmt.Errorf("wal: batch header is %d bytes, want 12: %w", len(payload), ErrCorrupt)
	}

	b := Batch{Sequence: binary.LittleEndian.Uint64(payload[0:8])}
	count := binary.LittleEndian.Uint32(payload[8:12])
	rest := payload[12:]

	for i := uint32(0); i < count; i++ {
		if len(rest) < 1 {
			return Batch{}, fmt.Errorf("wal: payload ended before record %d: %w", i, ErrCorrupt)
		}
		kind := Kind(rest[0])
		rest = rest[1:]
		if kind != KindSet && kind != KindDelete {
			return Batch{}, fmt.Errorf("wal: record %d has kind %d: %w", i, kind, ErrCorrupt)
		}

		key, rest2, err := readField(rest)
		if err != nil {
			return Batch{}, fmt.Errorf("wal: record %d key: %w", i, err)
		}
		rest = rest2

		rec := Record{Kind: kind, Key: key}

		// Only a SET carries a value field; a DELETE ends after its key.
		if kind == KindSet {
			val, rest3, err := readField(rest)
			if err != nil {
				return Batch{}, fmt.Errorf("wal: record %d value: %w", i, err)
			}
			rec.Value = val
			rest = rest3
		}

		b.Records = append(b.Records, rec)
	}

	// Trailing bytes mean the writer emitted something invalid: the checksum
	// already proved these bytes arrived intact.
	if len(rest) != 0 {
		return Batch{}, fmt.Errorf("wal: %d bytes trail the last record: %w", len(rest), ErrCorrupt)
	}

	return b, nil

}

// readField reads a uvarint length followed by that many bytes. The length is
// bounds-checked before slicing: a corrupt payload can claim any size, and
// slicing on it panics instead of returning an error.
func readField(p []byte) (field []byte, rest []byte, err error) {
	n, rest, err := readUvarint(p)
	if err != nil {
		return nil, nil, err
	}
	if uint64(len(rest)) < n {
		return nil, nil, fmt.Errorf("wal: field length %d exceeds %d remaining: %w", n, len(rest), ErrCorrupt)
	}
	return rest[:n], rest[n:], nil
}

// readUvarint reads one uvarint and returns it with the remaining payload.
// binary.Uvarint reports n <= 0 for malformed or truncated input, which is
// how a short payload announces itself.
func readUvarint(p []byte) (val uint64, rest []byte, err error) {
	v, n := binary.Uvarint(p)
	if n <= 0 {
		return 0, nil, fmt.Errorf("wal: malformed uvarint: %w", ErrCorrupt)
	}
	return v, p[n:], nil
}

// String names the fragment type for test failure messages, where "FULL"
// reads better than "1".
func (f FragmentType) String() string {
	switch f {
	case FragmentFull:
		return "FULL"
	case FragmentFirst:
		return "FIRST"
	case FragmentMiddle:
		return "MIDDLE"
	case FragmentLast:
		return "LAST"
	default:
		return fmt.Sprintf("FragmentType(%d)", uint8(f))
	}
}
