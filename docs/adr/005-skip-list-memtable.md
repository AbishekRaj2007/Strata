# ADR-005: Skip list memtable

**Status:** Accepted

## Context

The memtable absorbs every write and serves the newest data on every read. It needs three things at once: fast ordered insert, concurrent reads while writes are in flight, and an in-order iterator for flushing to an SSTable, since SSTables are sorted by definition.

Reads hit the memtable on every single lookup, so a design that blocks readers during writes puts a lock on the hottest path in the engine.

## Decision

A skip list with probabilistic level assignment (p = 0.25, maximum 12 levels), ordered by the ADR-004 comparator, with lock-free reads via atomic loads on forward pointers and a single writer.

## Alternatives considered

**`map[string][]byte` plus a sort at flush time.** By far the simplest, and faster for point lookups — O(1) against O(log n). Rejected because flushing then requires collecting and sorting every key at once, which is an O(n log n) latency spike and an allocation of the whole key set precisely when the engine is already under memory pressure. It also needs an `RWMutex` around the map, putting a lock on every read. Worth benchmarking against in T3.1 anyway, to confirm the ordered iterator earns its cost rather than assuming it.

**Red-black or AVL tree.** Ordered, O(log n), and deterministic rather than probabilistic. Rejected on concurrency: rebalancing rewrites pointers across the structure, so a reader can traverse into a subtree mid-rotation. Making that safe requires either locking readers out or a substantially harder lock-free design than a skip list's.

**B-tree.** Better cache locality than a skip list, which matters more than the asymptotics suggest. Rejected for the same reason as the balanced trees — node splits mutate shared structure — and because it is a larger implementation for a component that is discarded every 4 MB.

**Adaptive radix tree.** What some modern engines use, with excellent lookup performance on string keys. Rejected as too large a build for a twelve-week project, and hard to explain quickly under questioning.

## Consequences

**Accepted:** memory overhead of roughly 1.33 pointers per node on average at p = 0.25, plus per-node allocation. A skip list is materially less memory-efficient than a sorted array, which is part of why the memtable threshold is only 4 MB.

**Accepted:** probabilistic balance means worst-case O(n) lookup, with vanishing probability. Acceptable for a structure that is bounded at 4 MB and discarded, but it would not be for an on-disk index.

**Accepted:** the correctness of lock-free reads depends on getting the insert order right. A new node must be linked at level 0 **before** any higher level, or a reader following a high-level pointer reaches a node not yet reachable at the bottom, and observes a corrupt list. The race detector will not reliably catch this — it depends on interleaving that may not occur during a given test run. This is the single most subtle piece of code in Phase 3, and T3.1 requires an explicit memory-ordering review.

**Gained:** readers never block, on the hottest path in the engine.

**Gained:** the forward iterator that flushing needs falls out of the structure for free, in sorted order, with no additional allocation.

**Gained:** it is small enough to write on a whiteboard, which matters given it is on the hand-write list in plan.md §4.
