package engine

import (
	"errors"
	"fmt"
)

// Size limits from docs/format.md §0.1, enforced at the protocol boundary
// before a write reaches the WAL.
const (
	MaxKeySize   = 64 << 10
	MaxValueSize = 64 << 20
)

// ErrNotFound reports that a key holds no value. It is an error rather than a
// bool because the LSM engine has to distinguish three outcomes that a bool
// cannot: the key is absent, the key is present, and the read itself failed on
// a checksum mismatch or an I/O error. Returning (value, bool) would force
// that third case to be a panic or a silent zero value.
var ErrNotFound = errors.New("key not found")

// ErrClosed reports use of an engine after Close.
var ErrClosed = errors.New("engine is closed")

// ErrKeyTooLarge and ErrValueTooLarge report a write that exceeds the format's
// size limits. They are rejected here rather than deeper so that an oversized
// write never reaches the WAL.
var (
	ErrKeyTooLarge   = errors.New("key exceeds maximum size")
	ErrValueTooLarge = errors.New("value exceeds maximum size")
)

// Stats reports engine state for the INFO command. Fields are added as the
// subsystems that populate them arrive; a zero field means "not implemented
// yet" rather than "measured as zero", which is why INFO labels the counts it
// prints as approximate.
type Stats struct {
	// Keys is the number of live keys. Approximate once compaction runs,
	// because counting exactly would mean a full scan.
	Keys uint64

	// SyncPolicy names the WAL durability mode in force: always, interval,
	// or never. Empty until the WAL exists (T2.2).
	SyncPolicy string

	// Memtable reports the memtable set's state, or nil for an engine that
	// has no memtable. A pointer rather than a value because "this engine
	// keeps no memtable" and "the memtable is empty" are different facts, and
	// INFO must not print a write-stall count for an engine that cannot
	// stall.
	Memtable *RotationStats
}

// ScanResult is one page of a Scan. Cursor is the value to pass to the next
// call, and is zero when the iteration is complete.
type ScanResult struct {
	Keys   [][]byte
	Cursor uint64
}

// Engine is the storage engine's public surface. This interface is designed
// for the LSM engine of Phase 3 onward, not for the map that implements it in
// Phase 1: every method returns an error because every method will eventually
// touch a disk that can fail.
//
// Implementations must be safe for concurrent use.
type Engine interface {
	// Put stores value under key, replacing any existing value. It returns
	// only after the write is durable to the degree the sync policy
	// requires -- an acknowledged Put is one that survives a crash.
	Put(key, value []byte) error

	// Get returns the value stored under key. It returns ErrNotFound if the
	// key holds no value, wrapping it so callers use errors.Is.
	//
	// The returned slice is owned by the caller and must not alias engine
	// state, so that a concurrent compaction cannot mutate it underfoot.
	Get(key []byte) ([]byte, error)

	// Delete removes key. Deleting an absent key is not an error; the
	// reported bool says whether a value was actually removed, which DEL
	// needs for its count.
	Delete(key []byte) (bool, error)

	// Scan returns up to count keys at or after the cursor, in key order,
	// along with the cursor for the next page. A zero returned cursor means
	// the iteration finished.
	Scan(cursor uint64, count int) (ScanResult, error)

	// Stats reports current engine state for INFO.
	Stats() (Stats, error)

	// Close releases all resources. It is idempotent, and after it returns
	// every other method reports ErrClosed.
	Close() error
}

// ValidateKey reports whether a key is within the format's limits. The empty
// key is legal (docs/format.md §0.1).
func ValidateKey(key []byte) error {
	if len(key) > MaxKeySize {
		return fmt.Errorf("%w: %d bytes exceeds %d", ErrKeyTooLarge, len(key), MaxKeySize)
	}
	return nil
}

// ValidateValue reports whether a value is within the format's limits.
func ValidateValue(value []byte) error {
	if len(value) > MaxValueSize {
		return fmt.Errorf("%w: %d bytes exceeds %d", ErrValueTooLarge, len(value), MaxValueSize)
	}
	return nil
}
