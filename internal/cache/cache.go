package cache

import (
	"container/list"
	"sync"
	"sync/atomic"
)

const (
	// DefaultCapacityBytes is the cache size the engine uses unless
	// configured otherwise.
	DefaultCapacityBytes int64 = 64 << 20

	// NumShards splits the cache into independently locked segments.
	//
	// Sixteen is chosen so that the lock is not the bottleneck at realistic
	// reader counts while the per-shard bookkeeping stays small. The shard
	// index is the hash modulo this value, taken after a finalising mix, so
	// the low bits it selects on are as well distributed as the high ones.
	NumShards = 16
)

// Key identifies one cached block: the file it came from and its byte offset
// within that file.
//
// The offset alone would not do. File numbers are never reused, but two
// different files can and routinely do have a block at the same offset, so a
// key without the file number would serve one table's bytes for another's --
// and the checksum would pass, because the bytes are individually valid.
type Key struct {
	FileNumber  uint64
	BlockOffset uint64
}

// Stats reports cache behaviour for INFO.
type Stats struct {
	Hits     uint64
	Misses   uint64
	Bytes    int64
	Entries  int
	Capacity int64
	Evicted  uint64
}

// HitRate returns hits as a fraction of total lookups, or 0 if there have
// been none.
func (s Stats) HitRate() float64 {
	total := s.Hits + s.Misses
	if total == 0 {
		return 0
	}
	return float64(s.Hits) / float64(total)
}

// entry is one cached block. The bytes are immutable once inserted: every
// reader shares the same backing array, so nothing may write to it.
type entry struct {
	key   Key
	block []byte
}

// shard is one independently locked LRU.
type shard struct {
	mu       sync.Mutex
	capacity int64
	bytes    int64
	order    *list.List // front is most recently used
	index    map[Key]*list.Element

	// byFile lets EvictFile find a file's blocks in O(blocks of that file)
	// instead of scanning the whole shard. Without it, dropping a compacted
	// file's blocks would cost a full cache walk per deleted file, so it
	// would be skipped under load -- which is exactly how the leak this
	// index exists to prevent gets shipped.
	byFile map[uint64]map[Key]struct{}

	hits    atomic.Uint64
	misses  atomic.Uint64
	evicted atomic.Uint64
}

// Cache is a sharded LRU of SSTable data blocks, bounded by total bytes.
//
// It caches blocks rather than key-value pairs. A block is the unit the
// checksum covers and the unit a read already pays for, so block granularity
// gives spatial locality for free: fetching one key warms its neighbours,
// which is what a range scan and a skewed point-read workload both want. A
// per-key cache would store the same bytes with more overhead, lose that
// locality, and no longer line up with the checksum boundary.
type Cache struct {
	shards   [NumShards]shard
	capacity int64
}

// New returns a cache holding at most capacityBytes of block data. A
// non-positive capacity selects DefaultCapacityBytes.
func New(capacityBytes int64) *Cache {
	if capacityBytes <= 0 {
		capacityBytes = DefaultCapacityBytes
	}
	c := &Cache{capacity: capacityBytes}

	// The per-shard capacity is the total split evenly. Blocks hash across
	// shards uniformly, so an even split is the right allocation; a shard
	// that ran a little hot evicts a little earlier, which is a fairness
	// question rather than a correctness one.
	perShard := capacityBytes / NumShards
	if perShard < 1 {
		perShard = 1
	}
	for i := range c.shards {
		c.shards[i].capacity = perShard
		c.shards[i].order = list.New()
		c.shards[i].index = make(map[Key]*list.Element)
		c.shards[i].byFile = make(map[uint64]map[Key]struct{})
	}
	return c
}

// shardFor mixes both halves of the key so that one file's blocks spread
// across every shard.
//
// Hashing on the file number alone would put a whole table in one shard,
// which reintroduces the lock contention sharding exists to remove: a hot
// table would serialise every reader on a single mutex.
func (c *Cache) shardFor(k Key) *shard {
	h := k.FileNumber*0x9E3779B97F4A7C15 ^ (k.BlockOffset+1)*0xBF58476D1CE4E5B9
	h ^= h >> 33
	h *= 0xFF51AFD7ED558CCD
	h ^= h >> 33
	return &c.shards[h%NumShards]
}

// Get returns the cached block for k, if present, and marks it most recently
// used.
//
// The returned slice is the cache's own copy and is shared with every other
// reader of the same block. Callers must treat it as read-only.
func (c *Cache) Get(k Key) ([]byte, bool) {
	if c == nil {
		return nil, false
	}
	s := c.shardFor(k)

	s.mu.Lock()
	el, ok := s.index[k]
	if !ok {
		s.mu.Unlock()
		s.misses.Add(1)
		return nil, false
	}
	s.order.MoveToFront(el)
	block := el.Value.(*entry).block
	s.mu.Unlock()

	s.hits.Add(1)
	return block, true
}

// Put stores a copy of block under k, evicting least-recently-used entries
// until the shard is within capacity.
//
// The copy is not optional: the caller's buffer is a read buffer it is free
// to reuse, and the cache hands its slice to concurrent readers.
func (c *Cache) Put(k Key, block []byte) {
	if c == nil {
		return
	}
	s := c.shardFor(k)
	stored := append([]byte(nil), block...)

	s.mu.Lock()
	defer s.mu.Unlock()

	if el, ok := s.index[k]; ok {
		e := el.Value.(*entry)
		s.bytes += int64(len(stored)) - int64(len(e.block))
		e.block = stored
		s.order.MoveToFront(el)
	} else {
		el := s.order.PushFront(&entry{key: k, block: stored})
		s.index[k] = el
		s.bytes += int64(len(stored))

		files, ok := s.byFile[k.FileNumber]
		if !ok {
			files = make(map[Key]struct{})
			s.byFile[k.FileNumber] = files
		}
		files[k] = struct{}{}
	}

	// A single block larger than a shard's whole capacity would otherwise
	// spin this loop to empty and then sit there over budget. Evicting down
	// to the point where only the oversized block remains is the best
	// available answer, and the loop's len check is what stops it.
	for s.bytes > s.capacity && s.order.Len() > 1 {
		s.removeLocked(s.order.Back())
		s.evicted.Add(1)
	}
}

// EvictFile drops every cached block belonging to fileNumber and reports how
// many were dropped.
//
// This is the path that decides whether the cache leaks. Compaction deletes
// input files continuously; if their blocks stayed resident, memory would
// grow in proportion to compaction throughput and the LRU would never reclaim
// them, because a deleted file is never read again and so never becomes least
// recently used by access -- it just sits at the tail behind whatever is
// still live. The symptom only appears under sustained write load, which is
// why it has to be handled here rather than left to the eviction policy.
func (c *Cache) EvictFile(fileNumber uint64) int {
	if c == nil {
		return 0
	}
	dropped := 0
	for i := range c.shards {
		s := &c.shards[i]
		s.mu.Lock()
		for k := range s.byFile[fileNumber] {
			if el, ok := s.index[k]; ok {
				s.removeLocked(el)
				dropped++
			}
		}
		delete(s.byFile, fileNumber)
		s.mu.Unlock()
	}
	return dropped
}

// removeLocked drops one element. The caller holds s.mu.
func (s *shard) removeLocked(el *list.Element) {
	e := el.Value.(*entry)
	s.order.Remove(el)
	delete(s.index, e.key)
	s.bytes -= int64(len(e.block))

	if files, ok := s.byFile[e.key.FileNumber]; ok {
		delete(files, e.key)
		if len(files) == 0 {
			delete(s.byFile, e.key.FileNumber)
		}
	}
}

// Stats aggregates every shard.
//
// The counters are read shard by shard without a global lock, so a snapshot
// taken during heavy traffic can be very slightly inconsistent between
// fields. That is the right trade for a statistics call: INFO must never be
// able to stall the read path.
func (c *Cache) Stats() Stats {
	if c == nil {
		return Stats{}
	}
	st := Stats{Capacity: c.capacity}
	for i := range c.shards {
		s := &c.shards[i]
		st.Hits += s.hits.Load()
		st.Misses += s.misses.Load()
		st.Evicted += s.evicted.Load()

		s.mu.Lock()
		st.Bytes += s.bytes
		st.Entries += s.order.Len()
		s.mu.Unlock()
	}
	return st
}
