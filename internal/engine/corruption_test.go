package engine

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// T7.3's corruption sweep: flip bits in SSTables, truncate files at random
// offsets, delete a live SSTable, corrupt the manifest, corrupt CURRENT --
// and in every case demand a clear error rather than a panic or, worse, a
// silently wrong answer. A panic is caught by the test binary itself, so the
// assertion each case makes is narrower: either Get returns the value that
// was actually written, or it returns an error. It must never return
// something else.

// seedCorruptibleDB writes n small values through a tiny memtable threshold
// so the flusher produces several SSTables rather than one, then closes the
// engine so every table and the manifest are settled on disk. It returns the
// key/value pairs written, so a case can check the value it reads back
// against the value it wrote rather than merely against "no panic".
func seedCorruptibleDB(t *testing.T, dir string, n int) map[string]string {
	t.Helper()

	e := openLSM(t, dir, 2<<10) // small enough that n keys span several tables
	want := make(map[string]string, n)
	for i := 0; i < n; i++ {
		k := fmt.Sprintf("key-%04d", i)
		v := fmt.Sprintf("value-%04d-%s", i, strings.Repeat("x", 64))
		if err := e.Put([]byte(k), []byte(v)); err != nil {
			t.Fatalf("Put(%s): %v", k, err)
		}
		want[k] = v
	}
	if err := e.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return want
}

func sstFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	var out []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".sst") {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	return out
}

// checkNoWrongAnswers reopens dir and Gets every key in want, failing the
// test only if a Get returns a value that does not match what was written.
// An error is an acceptable outcome for a corrupted table; a wrong value
// never is. It returns whether the reopen itself succeeded, since a manifest
// or CURRENT corruption case expects Open to fail outright.
func checkNoWrongAnswers(t *testing.T, dir string, want map[string]string) (opened bool) {
	t.Helper()

	e, err := Open(Options{Dir: dir, Threshold: 2 << 10, MaxImmutable: 4})
	if err != nil {
		t.Logf("Open reported (acceptable for this case): %v", err)
		return false
	}
	defer e.Close()

	for k, v := range want {
		got, err := e.Get([]byte(k))
		if err != nil {
			continue // a clear error is an acceptable outcome
		}
		if string(got) != v {
			t.Errorf("Get(%s) = %q after corruption, want %q or an error -- silently wrong answer", k, got, v)
		}
	}
	return true
}

// flipRandomBytes flips n random bits across path's contents in place.
func flipRandomBytes(t *testing.T, path string, n int, rng *rand.Rand) {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	if len(data) == 0 {
		return
	}
	for i := 0; i < n; i++ {
		off := rng.Intn(len(data))
		data[off] ^= 0xFF
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}
}

// TestCorruptionSweep runs every corruption scenario T7.3 lists against a
// freshly seeded database, each in its own subtest with its own directory so
// one case's damage cannot bleed into another's.
func TestCorruptionSweep(t *testing.T) {
	rng := rand.New(rand.NewSource(2))

	t.Run("bit flip in one sstable", func(t *testing.T) {
		dir := t.TempDir()
		want := seedCorruptibleDB(t, dir, 40)

		tables := sstFiles(t, dir)
		if len(tables) == 0 {
			t.Fatal("no SSTables produced")
		}
		target := tables[rng.Intn(len(tables))]
		flipRandomBytes(t, target, 8, rng)

		checkNoWrongAnswers(t, dir, want)
	})

	t.Run("bit flip in every sstable", func(t *testing.T) {
		dir := t.TempDir()
		want := seedCorruptibleDB(t, dir, 60)

		for _, table := range sstFiles(t, dir) {
			flipRandomBytes(t, table, 4, rng)
		}

		checkNoWrongAnswers(t, dir, want)
	})

	t.Run("truncated sstable at random offsets", func(t *testing.T) {
		for trial := 0; trial < 10; trial++ {
			dir := t.TempDir()
			want := seedCorruptibleDB(t, dir, 40)

			tables := sstFiles(t, dir)
			target := tables[rng.Intn(len(tables))]

			info, err := os.Stat(target)
			if err != nil {
				t.Fatalf("Stat: %v", err)
			}
			if info.Size() == 0 {
				continue
			}
			cut := rng.Int63n(info.Size())
			if err := os.Truncate(target, cut); err != nil {
				t.Fatalf("Truncate: %v", err)
			}

			checkNoWrongAnswers(t, dir, want)
		}
	})

	t.Run("deleted sstable", func(t *testing.T) {
		dir := t.TempDir()
		want := seedCorruptibleDB(t, dir, 40)

		tables := sstFiles(t, dir)
		if len(tables) == 0 {
			t.Fatal("no SSTables produced")
		}
		if err := os.Remove(tables[rng.Intn(len(tables))]); err != nil {
			t.Fatalf("Remove: %v", err)
		}

		checkNoWrongAnswers(t, dir, want)
	})

	t.Run("corrupted manifest", func(t *testing.T) {
		dir := t.TempDir()
		want := seedCorruptibleDB(t, dir, 20)

		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("ReadDir: %v", err)
		}
		var manifestPath string
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), "MANIFEST-") {
				manifestPath = filepath.Join(dir, e.Name())
			}
		}
		if manifestPath == "" {
			t.Fatal("no MANIFEST file found")
		}
		flipRandomBytes(t, manifestPath, 16, rng)

		// A corrupted manifest may fail Open outright (acceptable: a clear
		// error) or, if the damage lands after the last record recovery
		// trusts, Open may succeed against a truncated-but-consistent
		// history. Either way, no wrong answer and no panic.
		checkNoWrongAnswers(t, dir, want)
	})

	t.Run("corrupted CURRENT", func(t *testing.T) {
		dir := t.TempDir()
		want := seedCorruptibleDB(t, dir, 20)

		current := filepath.Join(dir, "CURRENT")
		flipRandomBytes(t, current, 8, rng)

		opened := checkNoWrongAnswers(t, dir, want)
		t.Logf("reopen after CURRENT corruption: opened=%v", opened)
	})

	t.Run("garbage CURRENT naming a nonexistent manifest", func(t *testing.T) {
		dir := t.TempDir()
		seedCorruptibleDB(t, dir, 5)

		current := filepath.Join(dir, "CURRENT")
		if err := os.WriteFile(current, []byte("MANIFEST-999999\n"), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}

		if _, err := Open(Options{Dir: dir}); err == nil {
			t.Error("Open with CURRENT naming a nonexistent manifest succeeded, want a clear error")
		}
	})
}

// TestEdgeCaseKeysAndValues sweeps the boundary conditions T7.3 lists:
// empty key, empty value, maximum-size key and value, a key that is a strict
// prefix of another, all keys identical in a scan, and strictly sequential
// keys.
func TestEdgeCaseKeysAndValues(t *testing.T) {
	t.Run("empty key", func(t *testing.T) {
		e := openLSM(t, t.TempDir(), 1<<20)
		defer e.Close()

		if err := e.Put([]byte{}, []byte("v")); err != nil {
			t.Fatalf("Put(empty key): %v", err)
		}
		got, err := e.Get([]byte{})
		if err != nil {
			t.Fatalf("Get(empty key): %v", err)
		}
		if string(got) != "v" {
			t.Errorf("Get(empty key) = %q, want \"v\"", got)
		}
	})

	t.Run("empty value", func(t *testing.T) {
		e := openLSM(t, t.TempDir(), 1<<20)
		defer e.Close()

		if err := e.Put([]byte("k"), []byte{}); err != nil {
			t.Fatalf("Put(empty value): %v", err)
		}
		got, err := e.Get([]byte("k"))
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("Get = %q, want empty", got)
		}
	})

	t.Run("maximum size key and value", func(t *testing.T) {
		e := openLSM(t, t.TempDir(), 4<<20)
		defer e.Close()

		maxKey := make([]byte, MaxKeySize)
		for i := range maxKey {
			maxKey[i] = byte('a' + i%26)
		}
		if err := e.Put(maxKey, []byte("v")); err != nil {
			t.Fatalf("Put(max key): %v", err)
		}
		got, err := e.Get(maxKey)
		if err != nil || string(got) != "v" {
			t.Fatalf("Get(max key) = %q, %v, want \"v\", nil", got, err)
		}

		oversizedKey := make([]byte, MaxKeySize+1)
		if err := e.Put(oversizedKey, []byte("v")); err == nil {
			t.Error("Put(oversized key) succeeded, want ErrKeyTooLarge")
		}
	})

	t.Run("maximum size value", func(t *testing.T) {
		e := openLSM(t, t.TempDir(), 4<<20)
		defer e.Close()

		maxValue := make([]byte, 1<<20) // a slice of MaxValueSize: 64 MiB per case is too slow to run routinely
		for i := range maxValue {
			maxValue[i] = byte(i)
		}
		if err := e.Put([]byte("k"), maxValue); err != nil {
			t.Fatalf("Put(large value): %v", err)
		}
		got, err := e.Get([]byte("k"))
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if len(got) != len(maxValue) {
			t.Fatalf("Get returned %d bytes, want %d", len(got), len(maxValue))
		}

		oversizedValue := make([]byte, MaxValueSize+1)
		if err := e.Put([]byte("k2"), oversizedValue); err == nil {
			t.Error("Put(oversized value) succeeded, want ErrValueTooLarge")
		}
	})

	t.Run("prefix keys", func(t *testing.T) {
		e := openLSM(t, t.TempDir(), 1<<20)
		defer e.Close()

		keys := []string{"a", "ab", "abc", "abcd"}
		for _, k := range keys {
			if err := e.Put([]byte(k), []byte(k)); err != nil {
				t.Fatalf("Put(%s): %v", k, err)
			}
		}
		for _, k := range keys {
			got, err := e.Get([]byte(k))
			if err != nil || string(got) != k {
				t.Errorf("Get(%s) = %q, %v, want %q, nil", k, got, err, k)
			}
		}
	})

	t.Run("strictly sequential keys survive a flush", func(t *testing.T) {
		e := openLSM(t, t.TempDir(), 2<<10)
		defer e.Close()

		for i := 0; i < 500; i++ {
			k := fmt.Sprintf("%08d", i)
			if err := e.Put([]byte(k), []byte(k)); err != nil {
				t.Fatalf("Put(%s): %v", k, err)
			}
		}
		if err := e.Flush(); err != nil {
			t.Fatalf("Flush: %v", err)
		}
		for i := 0; i < 500; i++ {
			k := fmt.Sprintf("%08d", i)
			got, err := e.Get([]byte(k))
			if err != nil || string(got) != k {
				t.Errorf("Get(%s) = %q, %v, want %q, nil", k, got, err, k)
			}
		}
	})

	t.Run("all keys identical repeatedly overwritten", func(t *testing.T) {
		e := openLSM(t, t.TempDir(), 2<<10)
		defer e.Close()

		var last string
		for i := 0; i < 200; i++ {
			last = fmt.Sprintf("v%d", i)
			if err := e.Put([]byte("k"), []byte(last)); err != nil {
				t.Fatalf("Put: %v", err)
			}
		}
		got, err := e.Get([]byte("k"))
		if err != nil || string(got) != last {
			t.Errorf("Get = %q, %v, want %q, nil", got, err, last)
		}
	})
}

// TestSecondOpenOnALiveDirectoryFailsCleanly covers T7.3's "two processes
// opening the same directory" edge case: a directory already held by a live
// engine must reject a second Open with a clear error, never silently
// interleave writes into the same WAL and manifest.
func TestSecondOpenOnALiveDirectoryFailsCleanly(t *testing.T) {
	dir := t.TempDir()

	e := openLSM(t, dir, 1<<20)
	defer e.Close()

	if _, err := Open(Options{Dir: dir}); err == nil {
		t.Error("second Open on a live directory succeeded, want a clear error")
	}

	// The directory must still be fully usable through the engine that
	// really holds it.
	if err := e.Put([]byte("k"), []byte("v")); err != nil {
		t.Fatalf("Put after a rejected second Open: %v", err)
	}
}

// TestRestartWithZeroData covers restarting against a directory that was
// opened and closed but never written to.
func TestRestartWithZeroData(t *testing.T) {
	dir := t.TempDir()

	e := openLSM(t, dir, 1<<20)
	if err := e.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	reopened := openLSM(t, dir, 1<<20)
	defer reopened.Close()

	if _, err := reopened.Get([]byte("anything")); err == nil {
		t.Error("Get on an empty reopened database succeeded, want ErrNotFound")
	}
}
