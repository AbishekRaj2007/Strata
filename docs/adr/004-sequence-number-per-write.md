# ADR-004: Sequence number on every write

**Status:** Accepted

## Context

The same user key exists in many places at once: the active memtable, several immutable memtables, multiple overlapping L0 files, and one file per level below. A read has to determine which of those is current, and compaction has to determine which to keep and which to discard.

Without a total order over writes, "newest wins" is a heuristic assembled from file numbers, level positions, and flush timing. Every one of those breaks somewhere — file numbers say nothing about relative age of two keys inside merged files, and level position is a consequence of compaction scheduling rather than write order.

## Decision

Every write is assigned a monotonically increasing 64-bit sequence number at the point of entry, before the WAL append. The sequence travels with the key through every layer: WAL record, memtable entry, SSTable entry, compaction output. The comparator is `(user_key ascending, sequence descending)` everywhere.

## Alternatives considered

**Timestamps.** Wall-clock time as the ordering key. Rejected decisively: clocks go backwards on NTP adjustment, have insufficient resolution to distinguish writes microseconds apart, and would make correctness depend on system configuration. This is a well-known source of real data loss in distributed stores, and there is no reason to import the problem into a single-node engine.

**File number plus in-file position.** Order by which file the entry is in, then where. Rejected because compaction merges entries from several files into one, at which point the ordering information the scheme depended on has been destroyed.

**Level position alone.** Treat a shallower level as newer. Rejected because it is only accidentally true. It holds for a key that has been steadily compacted downward, and fails as soon as a key is written, compacted to L3, and written again — and the failure is silent.

**32-bit sequence numbers.** Half the per-entry overhead. Rejected: at 100k writes/sec a 32-bit counter wraps in under twelve hours, and wraparound in an ordering key means arbitrary corruption.

## Consequences

**Accepted:** 8 bytes per entry in the memtable, the WAL, and every SSTable. On a 100-byte entry that is 8% overhead, paid on every copy at every level. Prefix compression (ADR-006) recovers some of it on the key side, but the sequence itself is incompressible and this is a real, permanent cost.

**Accepted:** the sequence counter must be restored correctly on startup, to the highest value seen across all replayed WAL files. Restoring it too low means new writes get sequence numbers that sort *older* than existing data, so a fresh write is invisible behind an old one. This is a silent data-loss bug and is why T2.3 makes counter restoration an explicit requirement.

**Gained:** "newest wins" becomes a comparison rather than an inference. The read path stops at the first match, and the merge iterator keeps the first entry per user key, with no special cases.

**Gained:** snapshot reads become nearly free later — read at sequence `N` by skipping entries with a higher sequence. Not in scope for v1, but the format supports it without change, which is worth stating when asked how snapshots would be added.

**Gained:** tombstone dropping has a precise rule to reference. A tombstone may only be discarded when no snapshot sequence requires it, and that condition is expressible because sequences are a total order.
