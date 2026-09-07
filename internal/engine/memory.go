package engine

import (
	"bytes"
	"sort"
	"sync"
)

// Memory is an in-memory Engine backed by a map. It exists so that Phase 1 can
// exercise the protocol, dispatch, and connection lifecycle against a real
// implementation of the final interface; the LSM engine replaces it in Phase 3
// without any call site changing.
//
// It is not durable. Nothing here survives a restart, and it makes no attempt
// to bound memory.
type Memory struct {
	mu     sync.RWMutex
	data   map[string][]byte
	closed bool
}

// NewMemory returns an empty in-memory engine.
func NewMemory() *Memory {
	return &Memory{data: make(map[string][]byte)}
}

// Put stores value under key. The value is copied, because the caller's slice
// is a connection read buffer that is reused for the next command.
func (m *Memory) Put(key, value []byte) error {
	if err := ValidateKey(key); err != nil {
		return err
	}
	if err := ValidateValue(value); err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return ErrClosed
	}

	m.data[string(key)] = bytes.Clone(value)
	return nil
}

// Get returns a copy of the value stored under key.
func (m *Memory) Get(key []byte) ([]byte, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.closed {
		return nil, ErrClosed
	}

	v, ok := m.data[string(key)]
	if !ok {
		return nil, ErrNotFound
	}

	// Copying under the read lock is what lets the caller hold the result
	// while another goroutine overwrites the key.
	return bytes.Clone(v), nil
}

// Delete removes key, reporting whether it was present.
func (m *Memory) Delete(key []byte) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return false, ErrClosed
	}

	k := string(key)
	if _, ok := m.data[k]; !ok {
		return false, nil
	}
	delete(m.data, k)
	return true, nil
}

// Scan returns up to count keys at or after cursor in key order.
//
// The cursor is an index into the sorted key set, which is only coherent
// because this implementation sorts the whole keyspace on every call. That is
// O(n log n) per page and deliberately temporary: the LSM engine scans an
// already-ordered structure and encodes the last key returned instead
// (plan.md §7.5).
func (m *Memory) Scan(cursor []byte, count int) (ScanResult, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.closed {
		return ScanResult{}, ErrClosed
	}

	if count <= 0 {
		count = 10
	}

	keys := make([]string, 0, len(m.data))
	for k := range m.data {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	// The cursor is the last key returned, so the next page begins at the
	// first key strictly greater than it. Resolving it by search rather than
	// by remembered offset is what makes a cursor survive the map changing
	// between calls.
	start := 0
	if cursor != nil {
		start = sort.SearchStrings(keys, string(cursor))
		for start < len(keys) && keys[start] <= string(cursor) {
			start++
		}
	}
	if start >= len(keys) {
		return ScanResult{}, nil
	}

	end := start + count
	if end > len(keys) {
		end = len(keys)
	}

	page := make([][]byte, 0, end-start)
	for _, k := range keys[start:end] {
		page = append(page, []byte(k))
	}

	// A nil cursor means the iteration is complete.
	var next []byte
	if end < len(keys) {
		next = append([]byte(nil), keys[end-1]...)
	}
	return ScanResult{Keys: page, Cursor: next}, nil
}

// Stats reports the live key count, which is exact for this implementation.
func (m *Memory) Stats() (Stats, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.closed {
		return Stats{}, ErrClosed
	}

	return Stats{Keys: uint64(len(m.data)), SyncPolicy: "none (in-memory)"}, nil
}

// Flush drops every key. It backs FLUSHDB and is not part of the Engine
// interface, since the LSM engine implements it by other means (T6.x).
func (m *Memory) Flush() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return ErrClosed
	}

	m.data = make(map[string][]byte)
	return nil
}

// Close releases the map. It is idempotent.
func (m *Memory) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.closed = true
	m.data = nil
	return nil
}
