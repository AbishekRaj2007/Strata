package wal

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// BlockSize is the fixed physical block a WAL file is packed into (§2.1). The
// final block of a file may be short.
const (
	BlockSize      = 32768
	HeaderSize     = 7 // crc32c(4) + length(2) + type(1)
	MaxPayloadSize = BlockSize - HeaderSize
)

// FragmentType identifies a fragment's position within the record it carries
// (§2.1). Zero is deliberately not a valid type, which is what makes zero
// padding unambiguous to a reader.
type FragmentType uint8

const (
	FragmentFull   FragmentType = 1
	FragmentFirst  FragmentType = 2
	FragmentMiddle FragmentType = 3
	FragmentLast   FragmentType = 4
)

// Reader replays the fragments in one WAL file and reassembles them into
// batches. The whole file is read into memory at construction: recovery only
// ever reads closed, static files, and holding the file in memory is what
// makes "is there valid data after this point" answerable without a second
// pass over the file.
type Reader struct {
	data    []byte
	pos     int64
	readErr error
}

// NewReader prepares f for replay from its current position.
func NewReader(f *os.File) *Reader {
	data, err := io.ReadAll(f)
	return &Reader{data: data, readErr: err}
}

// Next returns the next batch. At a clean end of log it returns io.EOF: §2.3
// makes the distinction between a clean tail (the expected result of a crash
// mid-write) and real corruption a correctness requirement, not a stylistic
// one, so callers must not treat the two interchangeably.
func (r *Reader) Next() (Batch, error) {
	payload, err := r.NextRecord()
	if err != nil {
		return Batch{}, err
	}
	return decodeOrCorrupt(payload)
}

// NextRecord returns the next record's reassembled payload without
// interpreting it, and reports a clean end of log as io.EOF on the same terms
// as Next.
//
// This is the read half of the framing docs/format.md §4 has the manifest
// reuse: manifest replay reassembles records exactly as WAL recovery does and
// then decodes each payload as a version edit rather than a batch. The
// clean-tail rules in §2.3 are the framing's, not the batch codec's, so a
// manifest truncated mid-edit gets the same treatment for free -- the last
// incomplete record is end of log, not corruption.
func (r *Reader) NextRecord() ([]byte, error) {
	if r.readErr != nil {
		return nil, fmt.Errorf("wal: reading log: %w", r.readErr)
	}

	var payload []byte
	started := false

	for {
		typ, frag, err := r.nextFragment()
		if err != nil {
			if started && errors.Is(err, io.EOF) {
				// A FIRST or MIDDLE with no follower is the expected shape of
				// a crash mid-batch (§2.3), not corruption.
				return nil, io.EOF
			}
			return nil, err
		}

		switch typ {
		case FragmentFull:
			if started {
				return nil, fmt.Errorf("wal: FULL fragment follows an unfinished FIRST: %w", ErrCorrupt)
			}
			return frag, nil

		case FragmentFirst:
			if started {
				return nil, fmt.Errorf("wal: FIRST fragment follows an unfinished FIRST: %w", ErrCorrupt)
			}
			payload = append(payload, frag...)
			started = true

		case FragmentMiddle:
			if !started {
				return nil, fmt.Errorf("wal: orphaned MIDDLE fragment: %w", ErrCorrupt)
			}
			payload = append(payload, frag...)

		case FragmentLast:
			if !started {
				return nil, fmt.Errorf("wal: orphaned LAST fragment: %w", ErrCorrupt)
			}
			payload = append(payload, frag...)
			return payload, nil
		}
	}
}

// decodeOrCorrupt wraps a payload-decode failure: it happens behind a
// checksum that already proved the bytes arrived intact, so it is always
// corruption, never a candidate for tail leniency (§2.3).
func decodeOrCorrupt(payload []byte) (Batch, error) {
	b, err := DecodeBatch(payload)
	if err != nil {
		return Batch{}, err
	}
	return b, nil
}

// nextFragment reads one physical fragment, skipping zero padding as it goes.
// It reports the two situations §2.3 draws a hard line between:
//
//   - Structural incompleteness (not enough bytes left in the file for a
//     header, or for the payload a header declares) can only happen at the
//     true end of the data, so it is unconditionally io.EOF.
//   - A structurally complete but semantically broken fragment (bad checksum,
//     invalid type) is ambiguous on its own -- it is what a torn write looks
//     like, but it is also what corruption looks like. The only way to tell
//     them apart is to check whether anything valid follows.
func (r *Reader) nextFragment() (FragmentType, []byte, error) {
	for {
		if r.pos >= int64(len(r.data)) {
			return 0, nil, io.EOF
		}

		blockOff := r.pos % BlockSize
		avail := int64(BlockSize) - blockOff

		if avail < HeaderSize {
			r.pos += avail
			continue
		}
		if r.pos+HeaderSize > int64(len(r.data)) {
			return 0, nil, io.EOF // truncated header at the tail
		}

		header := r.data[r.pos : r.pos+HeaderSize]
		length := int64(binary.LittleEndian.Uint16(header[4:6]))
		typ := header[6]

		if typ == 0 {
			end := r.pos + avail
			if end > int64(len(r.data)) {
				end = int64(len(r.data))
			}
			if allZero(r.data[r.pos:end]) {
				r.pos += avail
				continue
			}
			// Type 0 with non-zero bytes behind it is not genuine padding: a
			// real writer never emits it. Treat the declared length as best
			// available information about how far it reaches.
			return r.ambiguous(length, fmt.Errorf("wal: fragment type 0 outside padding at offset %d: %w", r.pos, ErrCorrupt))
		}

		if length > MaxPayloadSize || r.pos+HeaderSize+length > int64(len(r.data)) {
			return 0, nil, io.EOF // declared length runs past EOF, at the tail
		}
		if typ < byte(FragmentFull) || typ > byte(FragmentLast) {
			return r.ambiguous(length, fmt.Errorf("wal: invalid fragment type %d at offset %d: %w", typ, r.pos, ErrCorrupt))
		}

		end := r.pos + HeaderSize + length
		checksum := binary.LittleEndian.Uint32(header[0:4])
		computed := crc32.Checksum(r.data[r.pos+6:end], crcTable)
		if checksum != computed {
			return r.ambiguous(length, fmt.Errorf("wal: checksum mismatch at offset %d: %w", r.pos, ErrCorrupt))
		}

		payload := r.data[r.pos+HeaderSize : end]
		r.pos = end
		return FragmentType(typ), payload, nil
	}
}

// ambiguous resolves a structurally-complete-but-semantically-broken fragment
// starting at r.pos with the given declared length: corruption if the rest of
// the file holds anything recoverable, a clean end of log if it does not.
func (r *Reader) ambiguous(length int64, corrupt error) (FragmentType, []byte, error) {
	after := r.pos + HeaderSize + length
	if hasValidFragmentAfter(r.data, after) {
		return 0, nil, corrupt
	}
	return 0, nil, io.EOF
}

// hasValidFragmentAfter reports whether a structurally sound, checksum-valid
// fragment exists anywhere from pos onward. It tolerates the same padding and
// skips past other broken fragments by their declared length, since a broken
// fragment's header can still be trusted to say how much space it occupies.
// It is a heuristic scan, not a full replay: it only needs to answer whether
// resumption is possible, not what the resumed data means.
func hasValidFragmentAfter(data []byte, pos int64) bool {
	for pos < int64(len(data)) {
		blockOff := pos % BlockSize
		avail := int64(BlockSize) - blockOff
		if avail < HeaderSize {
			pos += avail
			continue
		}
		if pos+HeaderSize > int64(len(data)) {
			return false
		}

		header := data[pos : pos+HeaderSize]
		length := int64(binary.LittleEndian.Uint16(header[4:6]))
		typ := header[6]

		if typ == 0 {
			end := pos + avail
			if end > int64(len(data)) {
				end = int64(len(data))
			}
			if allZero(data[pos:end]) {
				pos += avail
				continue
			}
			if length > MaxPayloadSize || pos+HeaderSize+length > int64(len(data)) {
				return false
			}
			pos += HeaderSize + length
			continue
		}

		if length > MaxPayloadSize || pos+HeaderSize+length > int64(len(data)) {
			return false
		}
		if typ < byte(FragmentFull) || typ > byte(FragmentLast) {
			pos += HeaderSize + length
			continue
		}

		end := pos + HeaderSize + length
		checksum := binary.LittleEndian.Uint32(header[0:4])
		computed := crc32.Checksum(data[pos+6:end], crcTable)
		if checksum != computed {
			pos = end
			continue
		}
		return true
	}
	return false
}

func allZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

// Recover replays every WAL file in dir in ascending file-number order,
// calling apply for each record in order. It returns the highest sequence
// number observed, which restores the engine's sequence counter.
//
// A torn tail is only ever legitimate in the highest-numbered file -- the one
// that was open when the process died. An earlier file ending mid-record is
// corruption regardless of shape (§2.3): a later file only exists because the
// earlier one was completed and rotated away from.
func Recover(dir string, apply func(seq uint64, rec Record) error) (uint64, error) {
	files, err := walFiles(dir)
	if err != nil {
		return 0, err
	}

	var highest uint64
	for i, wf := range files {
		isNewest := i == len(files)-1

		h, err := recoverFile(wf.path, isNewest, apply)
		if err != nil {
			return highest, err
		}
		if h > highest {
			highest = h
		}
	}
	return highest, nil
}

func recoverFile(path string, isNewest bool, apply func(seq uint64, rec Record) error) (uint64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("wal: open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	r := NewReader(f)
	var highest uint64

	for {
		b, err := r.Next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				if !isNewest && r.pos < int64(len(r.data)) {
					return highest, fmt.Errorf("wal: %s ends mid-record but is not the newest WAL file: %w", path, ErrCorrupt)
				}
				return highest, nil
			}
			return highest, fmt.Errorf("wal: recover %s: %w", path, err)
		}

		for i, rec := range b.Records {
			seq := b.Sequence + uint64(i)
			if err := apply(seq, rec); err != nil {
				return highest, fmt.Errorf("wal: apply record from %s: %w", path, err)
			}
			if seq > highest {
				highest = seq
			}
		}
	}
}

type walFile struct {
	num  uint64
	path string
}

// walFiles lists dir's WAL segments in ascending file-number order. Sorting
// by parsed number rather than directory or mtime order matters: nothing
// guarantees readdir returns names in numeric order once file numbers exceed
// a single digit width.
func walFiles(dir string) ([]walFile, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("wal: read dir %s: %w", dir, err)
	}

	var files []walFile
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".wal") {
			continue
		}
		num, err := strconv.ParseUint(strings.TrimSuffix(name, ".wal"), 10, 64)
		if err != nil {
			continue
		}
		files = append(files, walFile{num: num, path: filepath.Join(dir, name)})
	}

	sort.Slice(files, func(i, j int) bool { return files[i].num < files[j].num })
	return files, nil
}
