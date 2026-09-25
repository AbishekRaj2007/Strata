package manifest

import (
	"encoding/binary"
	"fmt"
)

// EditKind identifies a single edit within a manifest record
// (docs/format.md §4).
type EditKind uint32

// The edit kinds docs/format.md §4 defines.
const (
	EditAddFile           EditKind = 1
	EditDeleteFile        EditKind = 2
	EditSetLogNumber      EditKind = 3
	EditSetNextFileNumber EditKind = 4
	EditSetLastSequence   EditKind = 5
)

func (k EditKind) String() string {
	switch k {
	case EditAddFile:
		return "ADD_FILE"
	case EditDeleteFile:
		return "DELETE_FILE"
	case EditSetLogNumber:
		return "SET_LOG_NUMBER"
	case EditSetNextFileNumber:
		return "SET_NEXT_FILE_NUMBER"
	case EditSetLastSequence:
		return "SET_LAST_SEQUENCE"
	default:
		return fmt.Sprintf("EditKind(%d)", uint32(k))
	}
}

// AddedFile is one ADD_FILE edit: a file and the level it enters.
type AddedFile struct {
	Level int
	Meta  *FileMetadata
}

// DeletedFile is one DELETE_FILE edit. It names the file rather than
// describing it, because the version being edited already holds its metadata.
type DeletedFile struct {
	Level  int
	Number uint64
}

// VersionEdit is the set of changes carried by one manifest record.
//
// The record, not the individual edit, is the atomic unit (docs/format.md
// §4.1): a compaction replacing four inputs with two outputs is one edit
// containing four deletions and two additions, and the fsync that commits it
// makes all six visible together or none of them. That is what gives
// compaction exactly two states rather than three.
//
// The three counters are pointers so that "not set by this edit" is
// distinguishable from "set to zero". A zero last sequence is a real value for
// a fresh database, and treating it as absent would lose it on replay.
type VersionEdit struct {
	Added   []AddedFile
	Deleted []DeletedFile

	LogNumber      *uint64
	NextFileNumber *uint64
	LastSequence   *uint64
}

// SetLogNumber records the WAL file number that this version's memtable
// state begins at. Every WAL below it is redundant and may be deleted.
func (e *VersionEdit) SetLogNumber(n uint64) { e.LogNumber = &n }

// SetNextFileNumber records the file-number allocator's next value.
func (e *VersionEdit) SetNextFileNumber(n uint64) { e.NextFileNumber = &n }

// SetLastSequence records the highest sequence number assigned.
func (e *VersionEdit) SetLastSequence(n uint64) { e.LastSequence = &n }

// AddFile stages the addition of a file at a level.
func (e *VersionEdit) AddFile(level int, m *FileMetadata) {
	e.Added = append(e.Added, AddedFile{Level: level, Meta: m})
}

// DeleteFile stages the removal of a file from a level.
func (e *VersionEdit) DeleteFile(level int, number uint64) {
	e.Deleted = append(e.Deleted, DeletedFile{Level: level, Number: number})
}

// IsEmpty reports whether the edit would change nothing. An empty edit is
// legal to encode but pointless to append, and the writer skips it rather
// than paying an fsync for it.
func (e *VersionEdit) IsEmpty() bool {
	return len(e.Added) == 0 && len(e.Deleted) == 0 &&
		e.LogNumber == nil && e.NextFileNumber == nil && e.LastSequence == nil
}

// Encode appends the §4 wire form to dst and returns it.
//
// Deletions are written before additions. Replay is order-sensitive, and a
// compaction that rewrites a file into the same level would otherwise delete
// the file it had just added.
func (e *VersionEdit) Encode(dst []byte) []byte {
	for _, d := range e.Deleted {
		dst = binary.LittleEndian.AppendUint32(dst, uint32(EditDeleteFile))
		dst = binary.LittleEndian.AppendUint32(dst, uint32(d.Level))
		dst = binary.LittleEndian.AppendUint64(dst, d.Number)
	}

	for _, a := range e.Added {
		dst = binary.LittleEndian.AppendUint32(dst, uint32(EditAddFile))
		dst = binary.LittleEndian.AppendUint32(dst, uint32(a.Level))
		dst = binary.LittleEndian.AppendUint64(dst, a.Meta.Number)
		dst = binary.LittleEndian.AppendUint64(dst, a.Meta.Size)
		dst = appendField(dst, a.Meta.Smallest)
		dst = appendField(dst, a.Meta.Largest)
		dst = binary.LittleEndian.AppendUint64(dst, a.Meta.SmallestSeq)
		dst = binary.LittleEndian.AppendUint64(dst, a.Meta.LargestSeq)
	}

	if e.LogNumber != nil {
		dst = binary.LittleEndian.AppendUint32(dst, uint32(EditSetLogNumber))
		dst = binary.LittleEndian.AppendUint64(dst, *e.LogNumber)
	}
	if e.NextFileNumber != nil {
		dst = binary.LittleEndian.AppendUint32(dst, uint32(EditSetNextFileNumber))
		dst = binary.LittleEndian.AppendUint64(dst, *e.NextFileNumber)
	}
	if e.LastSequence != nil {
		dst = binary.LittleEndian.AppendUint32(dst, uint32(EditSetLastSequence))
		dst = binary.LittleEndian.AppendUint64(dst, *e.LastSequence)
	}

	return dst
}

// DecodeEdit parses one record's payload into a VersionEdit.
//
// Every failure is corruption. §4 gives version 1 no forward-compatibility
// provision, so an unknown edit type is damage rather than a newer writer, and
// trailing bytes mean the payload is not what its author wrote.
func DecodeEdit(payload []byte) (VersionEdit, error) {
	var e VersionEdit
	p := payload

	for len(p) > 0 {
		kind, rest, err := readUint32(p)
		if err != nil {
			return VersionEdit{}, fmt.Errorf("edit type: %w", err)
		}
		p = rest

		switch EditKind(kind) {
		case EditDeleteFile:
			var level uint32
			var number uint64
			if level, p, err = readUint32(p); err != nil {
				return VersionEdit{}, fmt.Errorf("DELETE_FILE level: %w", err)
			}
			if number, p, err = readUint64(p); err != nil {
				return VersionEdit{}, fmt.Errorf("DELETE_FILE number: %w", err)
			}
			if err := checkLevel(level); err != nil {
				return VersionEdit{}, err
			}
			e.Deleted = append(e.Deleted, DeletedFile{Level: int(level), Number: number})

		case EditAddFile:
			var level uint32
			m := &FileMetadata{}
			if level, p, err = readUint32(p); err != nil {
				return VersionEdit{}, fmt.Errorf("ADD_FILE level: %w", err)
			}
			if err := checkLevel(level); err != nil {
				return VersionEdit{}, err
			}
			if m.Number, p, err = readUint64(p); err != nil {
				return VersionEdit{}, fmt.Errorf("ADD_FILE number: %w", err)
			}
			if m.Size, p, err = readUint64(p); err != nil {
				return VersionEdit{}, fmt.Errorf("ADD_FILE size: %w", err)
			}
			if m.Smallest, p, err = readField(p); err != nil {
				return VersionEdit{}, fmt.Errorf("ADD_FILE smallest key: %w", err)
			}
			if m.Largest, p, err = readField(p); err != nil {
				return VersionEdit{}, fmt.Errorf("ADD_FILE largest key: %w", err)
			}
			if m.SmallestSeq, p, err = readUint64(p); err != nil {
				return VersionEdit{}, fmt.Errorf("ADD_FILE smallest sequence: %w", err)
			}
			if m.LargestSeq, p, err = readUint64(p); err != nil {
				return VersionEdit{}, fmt.Errorf("ADD_FILE largest sequence: %w", err)
			}
			if err := m.Validate(); err != nil {
				return VersionEdit{}, err
			}
			e.Added = append(e.Added, AddedFile{Level: int(level), Meta: m})

		case EditSetLogNumber, EditSetNextFileNumber, EditSetLastSequence:
			var v uint64
			if v, p, err = readUint64(p); err != nil {
				return VersionEdit{}, fmt.Errorf("%s: %w", EditKind(kind), err)
			}
			switch EditKind(kind) {
			case EditSetLogNumber:
				e.LogNumber = &v
			case EditSetNextFileNumber:
				e.NextFileNumber = &v
			case EditSetLastSequence:
				e.LastSequence = &v
			}

		default:
			return VersionEdit{}, fmt.Errorf("%w: unknown edit type %d", ErrCorrupt, kind)
		}
	}

	return e, nil
}

func checkLevel(level uint32) error {
	if level >= NumLevels {
		return fmt.Errorf("%w: level %d is outside 0..%d", ErrCorrupt, level, NumLevels-1)
	}
	return nil
}

func appendField(dst, field []byte) []byte {
	dst = binary.AppendUvarint(dst, uint64(len(field)))
	return append(dst, field...)
}

func readUint32(p []byte) (uint32, []byte, error) {
	if len(p) < 4 {
		return 0, nil, fmt.Errorf("%w: want 4 bytes, have %d", ErrCorrupt, len(p))
	}
	return binary.LittleEndian.Uint32(p), p[4:], nil
}

func readUint64(p []byte) (uint64, []byte, error) {
	if len(p) < 8 {
		return 0, nil, fmt.Errorf("%w: want 8 bytes, have %d", ErrCorrupt, len(p))
	}
	return binary.LittleEndian.Uint64(p), p[8:], nil
}

// readField reads a uvarint-prefixed byte string, copying it so the decoded
// edit does not alias the buffer it was read from -- that buffer is a reused
// framing block, and a version holding a slice into it would see its keys
// change under it.
func readField(p []byte) ([]byte, []byte, error) {
	n, rest, err := readUvarint(p)
	if err != nil {
		return nil, nil, err
	}
	if n > uint64(len(rest)) {
		return nil, nil, fmt.Errorf("%w: field length %d exceeds %d remaining", ErrCorrupt, n, len(rest))
	}
	field := make([]byte, n)
	copy(field, rest[:n])
	return field, rest[n:], nil
}

// readUvarint decodes a LEB128 uvarint, rejecting the non-minimal and
// overlong encodings docs/format.md §0 calls malformed.
//
// binary.Uvarint does not reject non-minimal encodings on its own: it decodes
// 0x80 0x00 to zero and reports two bytes consumed. Two encodings of the same
// value would give the same file two valid byte representations, so the length
// is compared against the minimal one rather than trusted.
func readUvarint(p []byte) (uint64, []byte, error) {
	v, n := binary.Uvarint(p)
	if n == 0 {
		return 0, nil, fmt.Errorf("%w: truncated uvarint", ErrCorrupt)
	}
	if n < 0 {
		return 0, nil, fmt.Errorf("%w: uvarint overflows 64 bits", ErrCorrupt)
	}

	var scratch [binary.MaxVarintLen64]byte
	if minimal := binary.PutUvarint(scratch[:], v); minimal != n {
		return 0, nil, fmt.Errorf("%w: uvarint for %d encoded in %d bytes, minimal is %d",
			ErrCorrupt, v, n, minimal)
	}
	return v, p[n:], nil
}
