package engine

import (
	"fmt"
	"sort"
	"testing"
)

// scanAll pages through the whole keyspace and returns every key seen.
func scanAll(t *testing.T, e *LSM, pageSize int) []string {
	t.Helper()

	var seen []string
	var cursor []byte
	for i := 0; ; i++ {
		if i > 10_000 {
			t.Fatal("Scan did not terminate")
		}
		res, err := e.Scan(cursor, pageSize)
		if err != nil {
			t.Fatalf("Scan: %v", err)
		}
		for _, k := range res.Keys {
			seen = append(seen, string(k))
		}
		if res.Cursor == nil {
			return seen
		}
		cursor = res.Cursor
	}
}

func TestScanReturnsEveryKeyInOrder(t *testing.T) {
	e := openLSM(t, t.TempDir(), 1<<20)
	defer e.Close()

	var want []string
	for i := 0; i < 50; i++ {
		k := fmt.Sprintf("key-%03d", i)
		if err := e.Put([]byte(k), []byte("v")); err != nil {
			t.Fatalf("Put: %v", err)
		}
		want = append(want, k)
	}
	sort.Strings(want)

	got := scanAll(t, e, 7) // a page size that does not divide the count
	if len(got) != len(want) {
		t.Fatalf("scanned %d keys, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("key %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestScanHidesDeletedKeys checks tombstone suppression reaches SCAN, and
// that a delete also hides the older versions it shadows.
func TestScanHidesDeletedKeys(t *testing.T) {
	e := openLSM(t, t.TempDir(), 1<<20)
	defer e.Close()

	for i := 0; i < 20; i++ {
		if err := e.Put([]byte(fmt.Sprintf("k%02d", i)), []byte("v")); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	for i := 0; i < 20; i += 2 {
		if _, err := e.Delete([]byte(fmt.Sprintf("k%02d", i))); err != nil {
			t.Fatalf("Delete: %v", err)
		}
	}

	got := scanAll(t, e, 5)
	if len(got) != 10 {
		t.Fatalf("scanned %d keys, want 10 after deleting half", len(got))
	}
	for _, k := range got {
		var n int
		if _, err := fmt.Sscanf(k, "k%d", &n); err != nil {
			t.Fatalf("unexpected key %q", k)
		}
		if n%2 == 0 {
			t.Errorf("deleted key %q appeared in a scan", k)
		}
	}
}

// TestScanCursorSurvivesAFlush is the reason plan.md §7.5 specifies a
// key-based cursor. A flush between two pages rewrites which files hold what;
// a positional cursor would point somewhere else afterwards, silently
// skipping or repeating keys. A key still names the same point in the
// ordering.
func TestScanCursorSurvivesAFlush(t *testing.T) {
	e := openLSM(t, t.TempDir(), 1<<30) // flush only when asked
	defer e.Close()

	var want []string
	for i := 0; i < 40; i++ {
		k := fmt.Sprintf("key-%03d", i)
		if err := e.Put([]byte(k), []byte("v")); err != nil {
			t.Fatalf("Put: %v", err)
		}
		want = append(want, k)
	}
	sort.Strings(want)

	// First page, entirely from the memtable.
	first, err := e.Scan(nil, 10)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(first.Keys) != 10 || first.Cursor == nil {
		t.Fatalf("first page = %d keys, cursor %v", len(first.Keys), first.Cursor)
	}

	// Everything moves to an SSTable underneath the paging client.
	if err := e.set.rotate(); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if err := e.flusher.DrainQueue(); err != nil {
		t.Fatalf("DrainQueue: %v", err)
	}
	if e.vs.Current().NumFiles(0) == 0 {
		t.Fatal("nothing flushed; the test would prove nothing")
	}

	// Resume with the cursor issued before the flush.
	seen := make([]string, 0, len(want))
	for _, k := range first.Keys {
		seen = append(seen, string(k))
	}
	cursor := first.Cursor
	for i := 0; cursor != nil; i++ {
		if i > 100 {
			t.Fatal("Scan did not terminate after the flush")
		}
		res, err := e.Scan(cursor, 10)
		if err != nil {
			t.Fatalf("Scan after flush: %v", err)
		}
		for _, k := range res.Keys {
			seen = append(seen, string(k))
		}
		cursor = res.Cursor
	}

	if len(seen) != len(want) {
		t.Fatalf("saw %d keys across the flush, want %d -- keys were skipped or repeated", len(seen), len(want))
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Fatalf("key %d = %q, want %q", i, seen[i], want[i])
		}
	}
}

// TestScanAcrossMemtablesAndTables checks a scan merges what is on disk with
// what is still in memory, preferring the newer version of a key that is in
// both.
func TestScanAcrossMemtablesAndTables(t *testing.T) {
	e := openLSM(t, t.TempDir(), 1<<30)
	defer e.Close()

	for i := 0; i < 10; i++ {
		if err := e.Put([]byte(fmt.Sprintf("k%02d", i)), []byte("old")); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	if err := e.set.rotate(); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if err := e.flusher.DrainQueue(); err != nil {
		t.Fatalf("DrainQueue: %v", err)
	}

	// Overwrite half in a fresh memtable, and delete one of the flushed keys.
	for i := 0; i < 10; i += 2 {
		if err := e.Put([]byte(fmt.Sprintf("k%02d", i)), []byte("new")); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	if _, err := e.Delete([]byte("k07")); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	got := scanAll(t, e, 4)
	if len(got) != 9 {
		t.Fatalf("scanned %v (%d keys), want 9", got, len(got))
	}
	for _, k := range got {
		if k == "k07" {
			t.Error("deleted key k07 appeared in the scan")
		}
	}

	// Each key must appear exactly once even though it exists in two sources.
	seen := map[string]int{}
	for _, k := range got {
		seen[k]++
	}
	for k, n := range seen {
		if n != 1 {
			t.Errorf("key %q appeared %d times, want once", k, n)
		}
	}
}

func TestScanOnAnEmptyEngine(t *testing.T) {
	e := openLSM(t, t.TempDir(), 1<<20)
	defer e.Close()

	res, err := e.Scan(nil, 10)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(res.Keys) != 0 || res.Cursor != nil {
		t.Errorf("Scan of an empty engine = %+v, want no keys and a nil cursor", res)
	}
}

func TestScanPastTheEnd(t *testing.T) {
	e := openLSM(t, t.TempDir(), 1<<20)
	defer e.Close()

	if err := e.Put([]byte("a"), []byte("v")); err != nil {
		t.Fatalf("Put: %v", err)
	}

	res, err := e.Scan([]byte("zzzz"), 10)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(res.Keys) != 0 || res.Cursor != nil {
		t.Errorf("Scan past the end = %+v, want no keys and a nil cursor", res)
	}
}
