package engine

import (
	"fmt"
	"testing"

	"github.com/AbishekRaj2007/Strata/internal/wal"
)

// TestBlockCacheServesRepeatedReads checks the cache is actually reached
// through the engine's read path, not merely constructed.
//
// The assertion is on hits from a warm read after a flush, because that is
// the only configuration where blocks are involved at all: a key still in the
// memtable never touches a table.
func TestBlockCacheServesRepeatedReads(t *testing.T) {
	e := openLSM(t, t.TempDir(), 4<<10)

	const keys = 500
	for i := 0; i < keys; i++ {
		if err := e.Put([]byte(fmt.Sprintf("key%05d", i)), []byte("value")); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	if err := e.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	// Read one key repeatedly. Every read after the first should find its
	// block resident.
	key := []byte("key00000")
	for i := 0; i < 100; i++ {
		if _, err := e.Get(key); err != nil {
			t.Fatalf("Get: %v", err)
		}
	}

	st, err := e.Stats()
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if st.BlockCache == nil {
		t.Fatal("Stats reported no block cache for an engine configured with one")
	}
	t.Logf("block cache: %d hits, %d misses, %.2f%% hit rate, %d bytes resident",
		st.BlockCache.Hits, st.BlockCache.Misses, st.BlockCache.HitRate()*100, st.BlockCache.Bytes)

	if st.BlockCache.Hits == 0 {
		t.Fatal("100 reads of one key produced no cache hits; the cache is not on the read path")
	}
	if st.BlockCache.HitRate() < 0.5 {
		t.Errorf("hit rate %.2f%% on a single hot key is too low to be a working cache", st.BlockCache.HitRate()*100)
	}
}

// TestCacheCanBeDisabled covers the tuning study's baseline configuration. A
// nil cache has to be a usable one, not a special case every call site
// branches on.
func TestCacheCanBeDisabled(t *testing.T) {
	dir := t.TempDir()
	e, err := Open(Options{
		Dir:             dir,
		Threshold:       4 << 10,
		MaxImmutable:    4,
		SyncPolicy:      wal.SyncAlways,
		BlockCacheBytes: -1,
	})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = e.Close() }()

	for i := 0; i < 500; i++ {
		if err := e.Put([]byte(fmt.Sprintf("key%05d", i)), []byte("value")); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}
	if err := e.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	for i := 0; i < 500; i++ {
		key := []byte(fmt.Sprintf("key%05d", i))
		got, err := e.Get(key)
		if err != nil {
			t.Fatalf("Get(%q) with no cache: %v", key, err)
		}
		if string(got) != "value" {
			t.Fatalf("Get(%q) = %q, want \"value\"", key, got)
		}
	}

	st, err := e.Stats()
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if st.BlockCache != nil {
		t.Errorf("Stats reported a block cache for an engine opened without one: %+v", *st.BlockCache)
	}
}

// TestBitsPerKeyOptionReachesTheTablesTheEngineWrites makes sure the tuning
// study's other knob is not silently dropped between Options and the flusher.
func TestBitsPerKeyOptionReachesTheTablesTheEngineWrites(t *testing.T) {
	sizes := map[int]int64{}

	for _, bits := range []int{4, 16} {
		dir := t.TempDir()
		e, err := Open(Options{
			Dir:          dir,
			Threshold:    1 << 20,
			MaxImmutable: 4,
			SyncPolicy:   wal.SyncAlways,
			BitsPerKey:   bits,
		})
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		for i := 0; i < 5_000; i++ {
			if err := e.Put([]byte(fmt.Sprintf("key%05d", i)), []byte("v")); err != nil {
				t.Fatalf("Put: %v", err)
			}
		}
		if err := e.Flush(); err != nil {
			t.Fatalf("Flush: %v", err)
		}

		var total int64
		for level := 0; level < 7; level++ {
			for _, fm := range e.vs.Current().Files(level) {
				total += int64(fm.Size)
			}
		}
		sizes[bits] = total
		if err := e.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	}

	// 5,000 keys at 16 bits carries 10,000 bytes of filter; at 4 bits, 2,500.
	// The difference has to show up in the files the engine actually wrote.
	if diff := sizes[16] - sizes[4]; diff < 7000 {
		t.Errorf("tables at 16 bits per key are only %d bytes larger than at 4; expected about 7,500 more of filter", diff)
	}
	t.Logf("total table bytes: %d at 4 bits per key, %d at 16", sizes[4], sizes[16])
}
