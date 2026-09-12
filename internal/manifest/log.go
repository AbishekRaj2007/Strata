package manifest

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/AbishekRaj2007/Strata/internal/wal"
)

// Log appends version edits to a MANIFEST file.
//
// The framing is the WAL's, reused verbatim per docs/format.md §4: the same
// 32 KiB blocks, the same 7-byte fragment header, the same CRC32C. Only the
// payload differs, carrying an encoded [VersionEdit] rather than a batch.
//
// A Log is not safe for concurrent use, and the framing writer beneath it
// would interleave two callers' records in the file rather than merely racing.
// Once more than one goroutine can commit -- the flusher and the compactor,
// from Phase 6 -- every append must go through [VersionSet.Commit], which
// serialises the append and the install together. Direct calls to Append are
// for startup, before either goroutine exists.
type Log struct {
	f    *os.File
	w    *wal.Writer
	name string
}

// CreateLog creates dir/MANIFEST-NNNNNN and returns a Log appending to it.
//
// The directory is fsynced before the Log is returned: a file is not durable
// until the directory entry naming it is, and a manifest that vanishes on
// crash takes the whole database with it.
func CreateLog(dir string, number uint64) (*Log, error) {
	name := ManifestName(number)
	path := filepath.Join(dir, name)

	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("manifest: create %s: %w", path, err)
	}
	if err := SyncDir(dir); err != nil {
		_ = f.Close()
		return nil, err
	}

	return &Log{f: f, w: wal.NewWriter(f), name: name}, nil
}

// Name returns the manifest's file name, which is what belongs in CURRENT.
func (l *Log) Name() string { return l.name }

// Append writes e as a single record and fsyncs it.
//
// **That fsync is the commit point** (docs/format.md §4.1). One record holds
// every edit in e -- all its deletions and all its additions -- because the
// record is the atomic unit: a compaction replacing four inputs with two
// outputs must become visible all at once or not at all. Splitting those
// edits across records would create the third state §4.1 says must not exist,
// where a replay sees the inputs gone and the outputs missing.
//
// An empty edit is skipped rather than paying an fsync to change nothing.
func (l *Log) Append(e *VersionEdit) error {
	if e.IsEmpty() {
		return nil
	}

	if _, err := l.w.WriteRecord(e.Encode(nil)); err != nil {
		return fmt.Errorf("manifest: append edit: %w", err)
	}
	if err := l.f.Sync(); err != nil {
		return fmt.Errorf("manifest: sync %s: %w", l.name, err)
	}
	return nil
}

// Close closes the underlying file. Append already fsynced everything it
// committed, so Close adds no durability of its own.
func (l *Log) Close() error {
	if err := l.f.Close(); err != nil {
		return fmt.Errorf("manifest: close %s: %w", l.name, err)
	}
	return nil
}

// Recover rebuilds the current version by replaying the manifest CURRENT
// names, and returns a VersionSet holding it.
//
// Replay begins from the empty version and applies every edit in the order it
// appears (docs/format.md §4.2). Order is what makes it correct: an ADD_FILE
// followed later by a DELETE_FILE of the same number leaves the file absent,
// and accumulating the two into sets first would lose that.
//
// A truncated trailing record means the last edit never committed, so replay
// accepts everything before it and stops -- the same clean-tail rule §2.3
// gives the WAL, inherited from the shared framing. A checksum failure with
// valid records after it is corruption and is reported as such.
func Recover(dir string) (*VersionSet, error) {
	name, err := ReadCurrent(dir)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, name)

	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("manifest: open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	vs := NewVersionSet()
	r := wal.NewReader(f)

	for i := 0; ; i++ {
		payload, err := r.NextRecord()
		if errors.Is(err, io.EOF) {
			// Clean end of log, including the truncated-tail case: every
			// record before this point committed, and this one did not.
			break
		}
		if err != nil {
			return nil, fmt.Errorf("manifest: replaying %s: %w", name, err)
		}

		edit, err := DecodeEdit(payload)
		if err != nil {
			return nil, fmt.Errorf("manifest: record %d of %s: %w", i, name, err)
		}
		if _, err := vs.Apply(&edit); err != nil {
			return nil, fmt.Errorf("manifest: applying record %d of %s: %w", i, name, err)
		}
	}

	return vs, nil
}
