package cache

import (
	"math/rand"
	"sync"
	"testing"
)

func block(size int, fill byte) []byte {
	b := make([]byte, size)
	for i := range b {
		b[i] = fill
	}
	return b
}

// trackedFiles counts the distinct file numbers the shards' deletion index
// still knows about. It is the leak the whole task is about: a cache can
// report zero bytes and zero entries while its bookkeeping keeps growing.
//
// The count is deduplicated across shards, because one file's blocks spread
// over every shard and a raw sum would report sixteen times the truth.
func (c *Cache) trackedFiles() int {
	seen := make(map[uint64]struct{})
	for i := range c.shards {
		s := &c.shards[i]
		s.mu.Lock()
		for file := range s.byFile {
			seen[file] = struct{}{}
		}
		s.mu.Unlock()
	}
	return len(seen)
}

// residentFiles counts the distinct file numbers with at least one block
// still in the LRU.
//
// This is the measurement that actually detects a retention leak. Bytes are
// bounded by capacity by construction, and the deletion index can be cleared
// without the blocks being dropped, so neither of those alone proves anything.
// What matters is whether blocks of files that no longer exist are still
// taking up space a live file could use.
func (c *Cache) residentFiles() int {
	seen := make(map[uint64]struct{})
	for i := range c.shards {
		s := &c.shards[i]
		s.mu.Lock()
		for k := range s.index {
			seen[k.FileNumber] = struct{}{}
		}
		s.mu.Unlock()
	}
	return len(seen)
}

func TestGetAndPutRoundTrip(t *testing.T) {
	c := New(1 << 20)
	k := Key{FileNumber: 7, BlockOffset: 4096}

	if _, ok := c.Get(k); ok {
		t.Fatal("empty cache reported a hit")
	}
	c.Put(k, block(128, 0xAB))

	got, ok := c.Get(k)
	if !ok {
		t.Fatal("Get missed a block that was just put")
	}
	if len(got) != 128 || got[0] != 0xAB {
		t.Fatalf("Get returned %d bytes starting %#x, want 128 bytes of 0xAB", len(got), got[0])
	}

	st := c.Stats()
	if st.Hits != 1 || st.Misses != 1 {
		t.Fatalf("Stats: hits=%d misses=%d, want 1 and 1", st.Hits, st.Misses)
	}
	if st.Bytes != 128 || st.Entries != 1 {
		t.Fatalf("Stats: bytes=%d entries=%d, want 128 and 1", st.Bytes, st.Entries)
	}
}

func TestOffsetAloneDoesNotIdentifyABlock(t *testing.T) {
	// Two files with a block at the same offset. If the key dropped the file
	// number this would return the wrong table's bytes -- and the checksum
	// would pass, because those bytes are a valid block, just not this one.
	c := New(1 << 20)
	c.Put(Key{FileNumber: 1, BlockOffset: 4096}, block(64, 0x11))
	c.Put(Key{FileNumber: 2, BlockOffset: 4096}, block(64, 0x22))

	got, ok := c.Get(Key{FileNumber: 1, BlockOffset: 4096})
	if !ok || got[0] != 0x11 {
		t.Fatalf("file 1 at offset 4096 returned %#x, want 0x11", got[0])
	}
	got, ok = c.Get(Key{FileNumber: 2, BlockOffset: 4096})
	if !ok || got[0] != 0x22 {
		t.Fatalf("file 2 at offset 4096 returned %#x, want 0x22", got[0])
	}
}

func TestPutCopiesTheCallersBuffer(t *testing.T) {
	// The read path hands Put the buffer it just read into and is free to
	// reuse it. If the cache aliased it, a later read would silently rewrite
	// a cached block under every concurrent reader of it.
	c := New(1 << 20)
	buf := block(64, 0x01)
	k := Key{FileNumber: 1, BlockOffset: 0}
	c.Put(k, buf)

	for i := range buf {
		buf[i] = 0xFF
	}

	got, ok := c.Get(k)
	if !ok {
		t.Fatal("Get missed")
	}
	if got[0] != 0x01 {
		t.Fatalf("cached block was mutated through the caller's buffer: got %#x, want 0x01", got[0])
	}
}

func TestEvictionIsLeastRecentlyUsed(t *testing.T) {
	// One shard's worth of capacity, addressed through a single shard by
	// pinning the key so the test measures the policy rather than the
	// hashing.
	const blockSize = 1000
	c := New(NumShards * 3 * blockSize)

	// Find three keys that land on the same shard, so all three compete.
	var keys []Key
	target := c.shardFor(Key{FileNumber: 1, BlockOffset: 0})
	for off := uint64(0); len(keys) < 4; off += 4096 {
		k := Key{FileNumber: 1, BlockOffset: off}
		if c.shardFor(k) == target {
			keys = append(keys, k)
		}
	}

	// Shrink that shard so exactly two entries fit.
	target.mu.Lock()
	target.capacity = 2 * blockSize
	target.mu.Unlock()

	c.Put(keys[0], block(blockSize, 0))
	c.Put(keys[1], block(blockSize, 1))

	// Touch keys[0], making keys[1] the least recently used.
	if _, ok := c.Get(keys[0]); !ok {
		t.Fatal("keys[0] should still be resident")
	}

	c.Put(keys[2], block(blockSize, 2))

	if _, ok := c.Get(keys[1]); ok {
		t.Error("keys[1] survived; the least recently used entry was not the one evicted")
	}
	if _, ok := c.Get(keys[0]); !ok {
		t.Error("keys[0] was evicted despite having been touched most recently")
	}
	if _, ok := c.Get(keys[2]); !ok {
		t.Error("keys[2] was evicted immediately after insertion")
	}
}

func TestCapacityIsRespected(t *testing.T) {
	const (
		capacity  = 1 << 20
		blockSize = 4096
	)
	c := New(capacity)
	for i := 0; i < 10_000; i++ {
		c.Put(Key{FileNumber: uint64(i % 20), BlockOffset: uint64(i) * blockSize}, block(blockSize, byte(i)))
	}

	st := c.Stats()
	if st.Bytes > capacity {
		t.Fatalf("cache holds %d bytes over a %d-byte capacity", st.Bytes, capacity)
	}
	if st.Evicted == 0 {
		t.Fatal("40 MB was inserted into a 1 MB cache with no evictions recorded")
	}
	t.Logf("after 10,000 inserts: %d bytes, %d entries, %d evicted", st.Bytes, st.Entries, st.Evicted)
}

func TestAnOversizedBlockDoesNotEmptyTheShard(t *testing.T) {
	// A block bigger than a shard's whole budget cannot be made to fit. The
	// cache must stop at "only the oversized block remains" rather than loop
	// forever trying to get under the limit.
	c := New(NumShards * 1024)
	k := Key{FileNumber: 1, BlockOffset: 0}
	c.Put(k, block(64*1024, 0x7F))

	if _, ok := c.Get(k); !ok {
		t.Fatal("the oversized block evicted itself")
	}
	if st := c.Stats(); st.Entries != 1 {
		t.Fatalf("Entries = %d, want 1", st.Entries)
	}
}

func TestEvictFileDropsEveryBlockOfThatFile(t *testing.T) {
	const blocksPerFile = 200
	c := New(64 << 20)

	for file := uint64(1); file <= 5; file++ {
		for b := 0; b < blocksPerFile; b++ {
			c.Put(Key{FileNumber: file, BlockOffset: uint64(b) * 4096}, block(1024, byte(file)))
		}
	}
	before := c.Stats()
	if before.Entries != 5*blocksPerFile {
		t.Fatalf("Entries = %d, want %d", before.Entries, 5*blocksPerFile)
	}

	if dropped := c.EvictFile(3); dropped != blocksPerFile {
		t.Fatalf("EvictFile(3) dropped %d blocks, want %d", dropped, blocksPerFile)
	}

	for b := 0; b < blocksPerFile; b++ {
		if _, ok := c.Get(Key{FileNumber: 3, BlockOffset: uint64(b) * 4096}); ok {
			t.Fatalf("block %d of the deleted file 3 is still cached", b)
		}
	}
	// Every other file is untouched.
	for _, file := range []uint64{1, 2, 4, 5} {
		for b := 0; b < blocksPerFile; b++ {
			if _, ok := c.Get(Key{FileNumber: file, BlockOffset: uint64(b) * 4096}); !ok {
				t.Fatalf("evicting file 3 also dropped block %d of file %d", b, file)
			}
		}
	}

	after := c.Stats()
	if want := before.Bytes - blocksPerFile*1024; after.Bytes != want {
		t.Fatalf("Bytes = %d after evicting file 3, want %d", after.Bytes, want)
	}
	if c.trackedFiles() != 4 {
		t.Fatalf("the deletion index still tracks %d files, want 4", c.trackedFiles())
	}
}

// TestCompactionHeavyRunKeepsMemoryFlat is the second half of T5.2's Done-when
// condition: a long compaction-heavy run shows flat cache memory usage.
//
// The shape is the one that actually leaks. Compaction retires input files
// continuously, and their blocks never become least-recently-used by access --
// nothing reads a deleted file again, so its blocks sit at the LRU tail behind
// live data while eviction works around them. Without EvictFile the byte count
// stays pinned at capacity holding data no reader can ever want, and the
// deletion index grows without bound.
func TestCompactionHeavyRunKeepsMemoryFlat(t *testing.T) {
	const (
		capacity      = 8 << 20
		blocksPerFile = 64
		blockSize     = 4096
		rounds        = 2000
		liveFiles     = 8
	)
	c := New(capacity)

	warm := func(file uint64) {
		for b := 0; b < blocksPerFile; b++ {
			k := Key{FileNumber: file, BlockOffset: uint64(b) * blockSize}
			if _, ok := c.Get(k); !ok {
				c.Put(k, block(blockSize, byte(file)))
			}
		}
	}

	for file := uint64(1); file <= liveFiles; file++ {
		warm(file)
	}

	maxBytes := int64(0)
	maxTracked, maxResident := 0, 0
	for r := 0; r < rounds; r++ {
		newFile := uint64(liveFiles + r + 1)
		warm(newFile)

		// The compaction commit: the oldest input is gone, so its blocks are
		// dropped with it.
		c.EvictFile(uint64(r + 1))

		st := c.Stats()
		if st.Bytes > maxBytes {
			maxBytes = st.Bytes
		}
		if n := c.trackedFiles(); n > maxTracked {
			maxTracked = n
		}
		if n := c.residentFiles(); n > maxResident {
			maxResident = n
		}
	}

	st := c.Stats()
	t.Logf("after %d rounds: %d bytes (peak %d, capacity %d), %d entries, %d files resident (peak %d), %d tracked (peak %d)",
		rounds, st.Bytes, maxBytes, int64(capacity), st.Entries, c.residentFiles(), maxResident, c.trackedFiles(), maxTracked)

	if maxBytes > capacity {
		t.Errorf("peak cache size %d exceeded the %d-byte capacity", maxBytes, capacity)
	}
	// The byte count cannot detect this leak on its own: it is bounded by
	// capacity whether the resident blocks are useful or dead. The resident
	// file count can. At most liveFiles + 1 files should ever have blocks in
	// the cache -- the working set plus the one being warmed -- and a cache
	// that retains compacted-away files grows this without bound until
	// eviction starts throwing out live data to make room for dead data.
	if maxResident > liveFiles+2 {
		t.Errorf("blocks of %d distinct files were resident at once over %d compactions, want at most %d; the cache is retaining files that no longer exist", maxResident, rounds, liveFiles+2)
	}
	// The deletion index is separately unbounded, and would grow one entry
	// per compaction if EvictFile stopped clearing it.
	if maxTracked > liveFiles+2 {
		t.Errorf("the deletion index peaked at %d files over %d compactions; it is growing with compaction throughput", maxTracked, rounds)
	}
}

// TestZipfianWorkloadExceedsNinetyPercentHitRate is the first half of T5.2's
// Done-when condition.
//
// Zipf is the right distribution to demand this of: real key access is
// heavily skewed, and a cache that cannot exploit skew is not worth its
// memory. A uniform workload over data larger than the cache would show a hit
// rate near the capacity ratio no matter how good the policy is, so it would
// measure the workload rather than the cache.
func TestZipfianWorkloadExceedsNinetyPercentHitRate(t *testing.T) {
	const (
		blocks    = 20_000
		blockSize = 4096
		capacity  = 16 << 20 // room for ~4,000 of the 20,000 blocks
		accesses  = 500_000
	)
	c := New(capacity)

	rng := rand.New(rand.NewSource(20250908))
	zipf := rand.NewZipf(rng, 1.2, 1, blocks-1)

	payload := block(blockSize, 0x5A)
	for i := 0; i < accesses; i++ {
		k := Key{FileNumber: 1, BlockOffset: zipf.Uint64() * blockSize}
		if _, ok := c.Get(k); !ok {
			c.Put(k, payload)
		}
	}

	st := c.Stats()
	t.Logf("zipf(s=1.2) over %d blocks, %d-byte cache: %.2f%% hit rate (%d hits, %d misses, %d resident)",
		blocks, int64(capacity), st.HitRate()*100, st.Hits, st.Misses, st.Entries)

	if st.HitRate() < 0.90 {
		t.Errorf("hit rate %.2f%% is below the 90%% T5.2 requires", st.HitRate()*100)
	}
	if st.Bytes > capacity {
		t.Errorf("cache holds %d bytes over a %d-byte capacity", st.Bytes, capacity)
	}
}

func TestConcurrentAccessIsRaceFree(t *testing.T) {
	const (
		goroutines = 32
		iterations = 5_000
	)
	c := New(1 << 20)
	payload := block(512, 0xC3)

	var wg sync.WaitGroup
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(g)))
			for i := 0; i < iterations; i++ {
				k := Key{
					FileNumber:  uint64(rng.Intn(16)),
					BlockOffset: uint64(rng.Intn(500)) * 4096,
				}
				switch rng.Intn(10) {
				case 0:
					c.EvictFile(k.FileNumber)
				case 1, 2:
					c.Put(k, payload)
				default:
					if b, ok := c.Get(k); ok && len(b) != len(payload) {
						t.Errorf("Get returned %d bytes, want %d", len(b), len(payload))
						return
					}
				}
			}
		}(g)
	}
	wg.Wait()

	// The accounting has to survive the churn: bytes and entries must still
	// agree with what the shards actually hold.
	st := c.Stats()
	if st.Bytes < 0 {
		t.Fatalf("byte accounting went negative: %d", st.Bytes)
	}
	if want := int64(st.Entries) * int64(len(payload)); st.Bytes != want {
		t.Fatalf("Bytes = %d but %d entries of %d bytes should be %d", st.Bytes, st.Entries, len(payload), want)
	}
}

func TestNilCacheIsUsable(t *testing.T) {
	// An engine configured without a cache passes nil rather than branching
	// at every call site.
	var c *Cache
	if _, ok := c.Get(Key{}); ok {
		t.Error("a nil cache reported a hit")
	}
	c.Put(Key{}, []byte("x"))
	if n := c.EvictFile(1); n != 0 {
		t.Errorf("EvictFile on a nil cache dropped %d blocks", n)
	}
	if st := c.Stats(); st != (Stats{}) {
		t.Errorf("Stats on a nil cache = %+v, want the zero value", st)
	}
}
