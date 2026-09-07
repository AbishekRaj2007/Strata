package engine

import (
	"bytes"
	"errors"
	"fmt"
	"sync"
	"testing"
)

// compileTimeCheck fails the build if Memory drifts from the interface, which
// is the point of defining the interface before the real engine exists.
var _ Engine = (*Memory)(nil)

func TestPutGet(t *testing.T) {
	m := NewMemory()

	if err := m.Put([]byte("k"), []byte("v")); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, err := m.Get([]byte("k"))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got) != "v" {
		t.Errorf("Get = %q, want %q", got, "v")
	}
}

func TestGetMissingReportsNotFound(t *testing.T) {
	_, err := NewMemory().Get([]byte("absent"))
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Get = %v, want ErrNotFound", err)
	}
}

// TestEmptyValueIsNotMissing is the engine-level half of the null-versus-empty
// distinction: a key holding an empty value exists, and must not read back the
// same way as an absent key.
func TestEmptyValueIsNotMissing(t *testing.T) {
	m := NewMemory()
	if err := m.Put([]byte("k"), []byte{}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, err := m.Get([]byte("k"))
	if err != nil {
		t.Fatalf("Get of empty value = %v, want nil error", err)
	}
	if len(got) != 0 {
		t.Errorf("Get = %q, want empty", got)
	}
}

func TestEmptyKeyIsLegal(t *testing.T) {
	// docs/format.md §0.1 states the empty key is legal.
	m := NewMemory()
	if err := m.Put([]byte{}, []byte("v")); err != nil {
		t.Fatalf("Put with empty key: %v", err)
	}
	if _, err := m.Get([]byte{}); err != nil {
		t.Errorf("Get with empty key: %v", err)
	}
}

func TestPutOverwrites(t *testing.T) {
	m := NewMemory()
	mustPut(t, m, "k", "first")
	mustPut(t, m, "k", "second")

	got, err := m.Get([]byte("k"))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got) != "second" {
		t.Errorf("Get = %q, want %q", got, "second")
	}
}

// TestPutCopiesValue guards against the engine aliasing a connection's read
// buffer, which is reused for the next command.
func TestPutCopiesValue(t *testing.T) {
	m := NewMemory()
	buf := []byte("original")

	if err := m.Put([]byte("k"), buf); err != nil {
		t.Fatalf("Put: %v", err)
	}
	copy(buf, "mutated!")

	got, err := m.Get([]byte("k"))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(got) != "original" {
		t.Errorf("Get = %q, want %q; the engine aliased the caller's buffer", got, "original")
	}
}

// TestGetCopiesValue is the same guarantee in the other direction: mutating a
// returned slice must not corrupt what is stored.
func TestGetCopiesValue(t *testing.T) {
	m := NewMemory()
	mustPut(t, m, "k", "original")

	got, err := m.Get([]byte("k"))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	copy(got, "mutated!")

	again, err := m.Get([]byte("k"))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(again) != "original" {
		t.Errorf("stored value = %q after caller mutated its result", again)
	}
}

func TestDelete(t *testing.T) {
	m := NewMemory()
	mustPut(t, m, "k", "v")

	existed, err := m.Delete([]byte("k"))
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if !existed {
		t.Error("Delete of present key = false, want true")
	}

	if _, err := m.Get([]byte("k")); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get after Delete = %v, want ErrNotFound", err)
	}

	existed, err = m.Delete([]byte("k"))
	if err != nil {
		t.Errorf("Delete of absent key: %v", err)
	}
	if existed {
		t.Error("Delete of absent key = true, want false")
	}
}

func TestSizeLimits(t *testing.T) {
	m := NewMemory()

	err := m.Put(bytes.Repeat([]byte("k"), MaxKeySize+1), []byte("v"))
	if !errors.Is(err, ErrKeyTooLarge) {
		t.Errorf("oversized key = %v, want ErrKeyTooLarge", err)
	}

	err = m.Put([]byte("k"), bytes.Repeat([]byte("v"), MaxValueSize+1))
	if !errors.Is(err, ErrValueTooLarge) {
		t.Errorf("oversized value = %v, want ErrValueTooLarge", err)
	}

	if err := m.Put(bytes.Repeat([]byte("k"), MaxKeySize), []byte("v")); err != nil {
		t.Errorf("key at exactly the limit: %v", err)
	}
}

func TestScanPagesInKeyOrder(t *testing.T) {
	m := NewMemory()
	for _, k := range []string{"c", "a", "e", "b", "d"} {
		mustPut(t, m, k, "v")
	}

	var seen []string
	var cursor []byte
	for i := 0; ; i++ {
		if i > 10 {
			t.Fatal("Scan did not terminate")
		}

		res, err := m.Scan(cursor, 2)
		if err != nil {
			t.Fatalf("Scan: %v", err)
		}
		for _, k := range res.Keys {
			seen = append(seen, string(k))
		}
		if res.Cursor == nil {
			break
		}
		cursor = res.Cursor
	}

	want := []string{"a", "b", "c", "d", "e"}
	if len(seen) != len(want) {
		t.Fatalf("Scan returned %v, want %v", seen, want)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Fatalf("Scan returned %v, want %v", seen, want)
		}
	}
}

func TestScanEmptyEngine(t *testing.T) {
	res, err := NewMemory().Scan(nil, 10)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(res.Keys) != 0 || res.Cursor != nil {
		t.Errorf("Scan of empty engine = %+v, want no keys and a nil cursor", res)
	}
}

func TestScanCursorPastEnd(t *testing.T) {
	m := NewMemory()
	mustPut(t, m, "a", "v")

	res, err := m.Scan([]byte("zzzz"), 10)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(res.Keys) != 0 || res.Cursor != nil {
		t.Errorf("Scan past end = %+v, want no keys and a nil cursor", res)
	}
}

func TestStats(t *testing.T) {
	m := NewMemory()
	for i := range 3 {
		mustPut(t, m, fmt.Sprintf("k%d", i), "v")
	}

	s, err := m.Stats()
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if s.Keys != 3 {
		t.Errorf("Keys = %d, want 3", s.Keys)
	}
}

func TestFlush(t *testing.T) {
	m := NewMemory()
	mustPut(t, m, "k", "v")

	if err := m.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if _, err := m.Get([]byte("k")); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get after Flush = %v, want ErrNotFound", err)
	}
}

func TestClosedEngineRejectsEverything(t *testing.T) {
	m := NewMemory()
	mustPut(t, m, "k", "v")
	if err := m.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if err := m.Put([]byte("k"), []byte("v")); !errors.Is(err, ErrClosed) {
		t.Errorf("Put after Close = %v, want ErrClosed", err)
	}
	if _, err := m.Get([]byte("k")); !errors.Is(err, ErrClosed) {
		t.Errorf("Get after Close = %v, want ErrClosed", err)
	}
	if _, err := m.Delete([]byte("k")); !errors.Is(err, ErrClosed) {
		t.Errorf("Delete after Close = %v, want ErrClosed", err)
	}
	if _, err := m.Scan(nil, 10); !errors.Is(err, ErrClosed) {
		t.Errorf("Scan after Close = %v, want ErrClosed", err)
	}
	if _, err := m.Stats(); !errors.Is(err, ErrClosed) {
		t.Errorf("Stats after Close = %v, want ErrClosed", err)
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	m := NewMemory()
	if err := m.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := m.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
}

// TestConcurrentAccess runs under -race in CI, where it is the check that the
// lock actually covers every path rather than most of them.
func TestConcurrentAccess(t *testing.T) {
	m := NewMemory()
	const goroutines = 16
	const ops = 100

	var wg sync.WaitGroup
	for g := range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range ops {
				key := []byte(fmt.Sprintf("k%d", i%10))
				switch i % 4 {
				case 0:
					_ = m.Put(key, []byte(fmt.Sprintf("v%d", g)))
				case 1:
					_, _ = m.Get(key)
				case 2:
					_, _ = m.Delete(key)
				case 3:
					_, _ = m.Scan(nil, 5)
				}
			}
		}()
	}
	wg.Wait()
}

func mustPut(t *testing.T, m *Memory, key, value string) {
	t.Helper()
	if err := m.Put([]byte(key), []byte(value)); err != nil {
		t.Fatalf("Put(%q, %q): %v", key, value, err)
	}
}
