# ADR-002: Leveled compaction, not size-tiered

**Status:** Accepted

## Context

An LSM-tree accumulates immutable files. Compaction decides which files merge and where the output goes, and that policy determines all three amplification factors — read, write, and space. It is the single most consequential design choice in the engine.

The two established families are leveled and size-tiered. They sit at opposite ends of the same trade-off, and choosing without working out what the rejected option actually gives you is how people end up unable to answer the obvious follow-up question.

## Decision

Leveled compaction. L0 holds freshly flushed tables that may overlap; L1 and below hold tables with strictly non-overlapping key ranges, each level targeting roughly 10× the bytes of the one above.

## Alternatives considered

**Size-tiered compaction.** Groups files of similar size and merges them into one larger file, the way Cassandra does by default. Worked through properly, the trade is:

| | Leveled | Size-tiered |
|---|---|---|
| Read amplification | One file per level below L0 | Every file in every tier may contain the key |
| Write amplification | 10–30×, data rewritten at each level | ~2–5×, each file written far fewer times |
| Space amplification | Under 2× | Up to 2× *per tier*, and a full merge transiently needs space equal to the data being merged |

Size-tiered is the correct choice for a write-heavy, append-mostly workload — time-series ingestion being the canonical case. Rejected here because a Redis-compatible KV store is read-heavy by nature, and because the worst case is genuinely bad: a large tier merge can transiently double disk usage, which conflicts directly with goal G5 in plan.md §2.

**Tiered-plus-leveled hybrid**, as in RocksDB's `kCompactionStyleLevel` with tiered L0. Rejected as a v1 choice for complexity reasons, not merit — it is close to what the design already does by treating L0 specially, and it is the natural direction if the write path later proves to be the bottleneck.

**FIFO compaction.** Drops the oldest files outright. Rejected immediately: it discards live data, which makes it a cache eviction policy rather than a durable store.

## Consequences

**Accepted:** 10–30× write amplification, in exchange for at most one file read per level below L0. On an NVMe drive with far more sequential write bandwidth than the workload needs, this is the right side of the trade, and T6.6 measures the actual figure rather than assuming the range.

**Accepted:** compaction is a continuous background cost rather than a periodic one. Foreground write latency depends on compaction keeping pace, which is why write stalls (T6.4) are a designed feature and not a failure mode.

**Accepted:** the L0 special case adds real complexity. Because L0 files come from independent memtable flushes their ranges overlap arbitrarily, so every L0 file must be checked on every read, and the L0 expansion rule during compaction iterates to a fixed point. This is the most intricate part of the picker.

**Gained:** space amplification under 2×, which is directly measurable and is one of the definition-of-done criteria.

**Gained:** the non-overlap invariant below L0 makes reads binary-searchable per level, and gives the invariant checker in T6.5 something concrete to assert continuously.
