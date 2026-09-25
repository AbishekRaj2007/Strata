# T9.5 — Interview preparation package

Written answers to plan.md §22, plus the two rehearsed pieces (60-second
summary, architecture-from-memory narration) and a specific account of the
hardest bug. Practicing them out loud is on the person, not this file — but
every fact below traces to a real measurement or a real commit, not a
plausible-sounding guess.

## The 60-second summary

Strata is a persistent key-value store I built from scratch in Go, speaking
the Redis wire protocol so real Redis clients and `redis-benchmark` work
against it unmodified. It's a log-structured merge-tree: writes go to a
write-ahead log and an in-memory skip list, which flushes to sorted,
checksummed files on disk once it fills, and a background compactor merges
those files to keep reads fast and reclaim space. I hand-wrote the storage
engine — the skip list, the SSTable format, the compaction picker, the WAL
framing and recovery — because that's the part of an LSM-tree I wanted to
actually understand, not wrap RocksDB and call it a day. Every durability
claim is backed by a fault-injection harness that fails I/O calls one at a
time and a crash-testing loop that runs `kill -9` against it in CI. Every
performance number is measured, three runs minimum, with the hardware and
the exact command written down next to it — including the numbers that
missed their own targets, and why.

## The architecture, narrated through a write and a read

Draw this from memory in under three minutes:

**Boxes, top to bottom:** client → `internal/server`/`internal/resp` (RESP2
codec, connection lifecycle) → `internal/engine` (the public API) → two
paths: the memtable/WAL on the write side, SSTables organized into levels
with a block cache and bloom filters on the read side → `internal/manifest`
tracking which files exist → `internal/compaction` merging them in the
background.

**A write, `SET foo bar`:** the connection reads a RESP array off the
socket, `commandArgs` turns it into `["SET", "foo", "bar"]`, dispatch calls
`engine.Put`. `Put` takes `memtableSet`'s single lock, appends a WAL record
(framed into 32 KiB blocks, CRC32C over every record), and inserts into the
active skip list. Under `sync=always` the caller blocks on `Syncer` until a
group-commit fsync actually lands; under `sync=interval` it returns once the
memtable insert is done and a background ticker fsyncs periodically. Once
the memtable crosses its size threshold, it rotates to immutable and a
background flusher writes it out as a block-encoded SSTable, then commits
an `ADD_FILE` edit to the manifest — that fsync is the actual commit point
for the file existing at all.

**A read, `GET foo` when it's absent:** check the active memtable (skip
list lookup, miss), then immutable memtables newest-first (miss), then each
level's SSTables — the bloom filter is checked before anything touches
disk, and most of the time it says "definitely not here" and the level
costs nothing. If a filter says maybe, the block cache is checked; on a
miss the block is read, its checksum verified, and a binary search runs
inside it. Falling through every level returns not-found, which the server
encodes as a RESP null bulk string.

## Design

**Why LSM over B-tree? When would you choose the opposite?**
An LSM-tree turns random writes into sequential ones: everything lands in
an append-only WAL and an in-memory buffer first, and disk I/O only happens
in large sequential chunks (flush, compaction). A B-tree updates in place,
which means random I/O and page splits on every write. I'd choose a B-tree
when reads dominate and point-read latency matters more than write
throughput — an LSM-tree's read path can touch multiple levels (bounded to
roughly one per level by going leveled, per ADR-002, but still more than a
B-tree's single descent), and a B-tree never pays a compaction tax. I'd also
choose a B-tree for a single-writer embedded system with modest write
volume where the operational complexity of a background compactor isn't
worth taking on.

**Walk me through `SET foo bar`, socket to disk.**
Covered above. The one detail worth adding under follow-up: the memtable
insert and the WAL append happen under the same `memtableSet` lock (T8.2's
profile found this lock at 83–97% of all contention samples under load —
it's the single biggest cost in the system, more than GC, more than RESP
parsing), and that's deliberate: it's what makes the sequence-number
assignment in ADR-004 atomic with the memtable becoming visible to readers.

**Walk me through `GET foo` when the key is absent.**
Covered above. The bloom filter is the detail that matters here: T5.3
measured absent-key lookups 10.6x faster with blooms enabled than without,
on a cold cache — which is the realistic case, since an absent key's block
is by definition not resident.

**Why leveled and not size-tiered? What changes for a write-heavy workload?**
Leveled compaction keeps non-overlapping key ranges within each level below
L0, which bounds read amplification to roughly one file per level — a point
read never has to consult more than a handful of files even under a large
dataset. Size-tiered instead merges files of similar size regardless of key
overlap, which is cheaper on writes (fewer, larger merges) but lets read
and space amplification grow, because many overlapping files can pile up.
ADR-002 accepted 10–30x write amplification for that read-amp bound;
T6.6 measured it and found space amplification actually came in at 1.45x
after a full overwrite-and-settle cycle, under the 2x target ADR-002
predicted without measuring. For a write-heavy workload I'd widen the L0
trigger and raise the per-level size multiplier, trading some read
amplification back for fewer, cheaper compactions — the same knob T6.1's
compaction-spread work already exercises.

**How would you add snapshots? Consistent range scans? Transactions?**
The sequence number already on every write (ADR-004) is most of the
mechanism: a snapshot is just "pin the current sequence number," and a read
taken under that snapshot filters out any record with a higher sequence —
the memtable and SSTable comparator already order by `(key asc, sequence
desc)`, so "the newest version visible to this snapshot" is a prefix
condition on an ordering that already exists. The missing piece is telling
compaction it cannot drop a version a live snapshot can still see, which
today's tombstone-drop rule doesn't need to reason about because nothing
holds a sequence open yet. Consistent range scans are the same idea applied
to `Scan`: pin the sequence at scan start rather than reading the live
version on every page, which `Scan`'s cursor design doesn't do today.
Multi-key transactions could reuse the WAL's existing atomic-batch framing
(`Batch.Records` already commits multiple records in one fsync'd unit) for
the write side, but would need a concurrency-control layer above it —
optimistic conflict checking against the sequence numbers touched, most
likely, rather than locking, to avoid adding contention next to the
memtable lock T8.2 already found to be the system's worst one.

## Durability

**What does `fsync` actually guarantee?**
That the bytes the OS had buffered for that file descriptor are on stable
storage such that a crash immediately after `fsync` returns will not lose
them — not more than that. It says nothing about other files not
`fsync`'d, and it can be lied to by a disk write cache without power-loss
protection, which is outside what any software layer can detect.

**What is your commit point, and what is the state of the world one
instruction before it?**
Two different ones, both fsync-based: the WAL's `fsync` (via `Syncer`,
group-committed) is the commit point for a write being acknowledged; the
manifest log's `Append` `fsync` (docs/format.md §4.1) is the commit point
for a version change — a flush or a compaction — being real. One
instruction before either: the bytes are already in the file at the right
offset (handed to `WriteAt`) but not yet durable. The framing is built so
that state is safe to crash in — a WAL block's CRC32C means a reader can
tell a torn tail from a clean one and stop exactly at the last complete
record, and the manifest's append-only edit log has the identical property
via the same framing code, reused verbatim.

**How do you *know* you have not lost data?**
Because a test says so, not because the design sounds right. T2.4's crash
harness runs 100 consecutive randomized `kill -9` iterations against the
real engine at `sync=always` as a CI job. T7.2's fault-injection layer fails
every single I/O call in a representative workload, one at a time, and
checks recovery after each. Both track *acknowledged* operations
specifically, not merely sent ones — an operation that was never
acknowledged is allowed to vanish; one that was is not.

## Concurrency

**How do readers and the compactor coexist without blocking each other?**
Versions are immutable and reference-counted (T4.2). A reader takes a
reference to the current version, walks its files, and releases the
reference when done; compaction builds entirely new files, then installs a
new version via the manifest's atomic commit, and only unreferenced files
that no version — current or still-held-by-a-reader — points to are
physically deleted. A reader never blocks a compaction and never sees a
file vanish out from under it, because deletion is gated on the refcount,
not on wall-clock time.

**What happens if a file is deleted while a read is in flight?**
It can't, by construction of the above — but the claim is verified, not
just argued: T4.2's stress test runs 100 concurrent readers against 100
compactions, with a fault-injection layer that panics immediately if any
read touches a file already marked deleted. 14,000 real reads, clean under
`-race`.

**Where is your worst lock contention and how did you find it?**
`memtableSet.Add`'s single `RWMutex` — every `Put` and `Delete` serializes
on it regardless of client count. Found with `runtime.SetBlockProfileRate`
and `SetMutexProfileFraction` enabled during a 20-client mixed 80/20 stress
run (`test/stress`'s existing mechanism, built for T7.4): 83–97% of all
block and mutex profile samples landed on that one lock, and it stayed that
dominant even under `sync=interval`, which rules out "it's just the fsync
sitting inside the critical section" as the explanation — the lock
granularity itself is the cost.

## Performance

**What are your numbers, on what hardware, under what sync policy?**
11th Gen Intel i5-11400H, 12 logical cores, 7.5 GiB RAM, NVMe/ext4,
`go1.26.4` — full detail in `docs/benchmarks.md`. Headline figures: 43,917
ops/sec unpipelined SET at `sync=interval` (p99 2.80ms); 8,912.7 ops/sec at
32 clients under `sync=always` with group commit, a 15.2x speedup over
1-client; 673,854 ops/sec on an absent-key GET with blooms on. Every row
states hardware, sync policy, and the exact reproduction command, and rows
are medians of at least three runs — never best-of-N.

**Where does time actually go? Show me the flamegraph.**
Here's where the honest answer gets specific rather than smooth: I can't
show a CPU flamegraph, because this development sandbox reliably
`SIGKILL`s any process the moment `SIGPROF`-based CPU profiling starts —
reproduced 8/8 times, isolated with a controlled experiment (`go test
-cpuprofile` with *zero* load also dies in 7ms), and it's specific to that
one profiling mechanism, since heap, mutex, and block profiling all work
fine on the identical loaded server. What I *can* show, measured: a
contention profile putting 83–97% of wait time on the memtable lock, and a
heap profile ranking RESP parsing and skip-list insertion as the top
allocation sites — skip-list insertion allocates more bytes per operation,
RESP parsing has the higher cumulative object count, which is a distinction
my own pre-measurement hypothesis got backwards. The CPU ranking in
`docs/profiles/t8.2-report.md` is disclosed as inference from code
inspection, not a profile, because I'd rather say "I couldn't measure this"
than show a graph that isn't real.

**Biggest optimisation win — and what did you predict before measuring?**
By raw allocation reduction, two buffer-reuse changes both went from a
handful of allocations per operation to effectively zero (WAL encode buffer
5→1 allocs/op, command-args slice 3→0). But the biggest end-to-end win was
`GOGC=400`: +72% throughput and p99 2.6x better than the default on the
same mixed workload — predicted, before measuring, as "raising the trigger
threshold should cut GC frequency and therefore tail latency," which held
up almost exactly proportionally (3.5x fewer GC cycles, 5x smaller worst
pause, 2.9x better p99). The prediction that *didn't* hold was trying the
same buffer-reuse trick a third time on the RESP reader's bulk-string
buffer: it broke correctness immediately, because that buffer's contents
are still owned by the caller when the next read would need to reuse it —
unlike the two that worked, where the reused bytes were fully consumed
before the next call.

**How does p99 behave during compaction, and why?**
T6.4's backpressure design bounds L0 growth under sustained compaction
pressure rather than letting it grow unboundedly, converting overload into
reported write stalls instead of silent degradation. Separately, T8.4 found
that GC-cycle collisions — not compaction — explain a meaningful share of
tail latency at default settings: the worst single GC stop-the-world pause
measured (7.9ms at `GOGC=100`) sits close to the measured p99 (5.9ms) on
the same run, and both move together when `GOGC` changes. The instinct to
blame compaction for every tail-latency spike would have been wrong here;
correlating against GC trace timestamps specifically is what caught it.

## Honesty

**What is broken or unfinished?**
`test/fault`'s fault-injection sweep is currently failing: under an
injected fsync failure, an acknowledged write can come back holding a
*different key's* value, or an acknowledged delete can be resurrected after
recovery — a real durability invariant violation. It surfaced during the
T8.3 optimisation pass, reproduces identically on the commit before those
changes (so it's unrelated to them, confirmed rather than assumed), and is
an open, tracked item, not a hidden one. Separately: CPU profiling doesn't
work in this development sandbox (disclosed above), and the T8.1 read-path
benchmark numbers carry a documented 30x run-to-run spread from background
load on a live desktop machine that hasn't been re-measured on a quiet one.

**What would you do differently starting over?**
Wire group commit onto the write path (`memtableSet.Add` calling
`Syncer.AwaitDurable`) at the same time I built `Syncer` itself, rather than
building it, benchmarking it in isolation, and only discovering months
later — via a production-path number that looked like a regression — that
it had never actually been connected. And I'd profile the memtable lock in
Phase 3 or 4, not Phase 8: it's the single biggest cost in the whole
system, and sharding it is now a known, well-justified next step that
could have shaped the memtable design from the start instead of being
retrofitted onto it.

**What did you get wrong the first time?**
Two concrete ones. First, the allocation-site hypothesis above: I expected
RESP parsing to clearly dominate skip-list insertion on both bytes and
object count, and the profile showed they split those two metrics between
them. Second — the harder one — `Writer.Offset()` read a plain `int64`
field (`offset`) that `WriteRecord` mutates mid-call, while
`Syncer.leadSync` (T2.2's group-commit leader) reads it from a different
goroutine entirely. It passed every test that didn't specifically force
that interleaving under `-race` with real concurrent load, because the race
window is narrow and `offset` frequently held the right value anyway. The
fix was tracking a separate `published atomic.Int64`, written only
immediately after each successful `WriteAt` — never at the point `offset`
itself advances mid-framing — so a concurrent reader can never observe
"bytes I haven't actually handed to the OS yet." That's also the answer to
"tell me about your hardest bug": a race that was invisible until the exact
test that exercises group commit under real concurrency ran under `-race`,
in a field whose name and comment ("counts every byte handed to the OS")
were actively describing behavior it didn't have.

## Section 22 self-check

Run through this list unaided before an interview; anything that needs
notes is a gap, per this task's own trap:

- [ ] Whiteboard the architecture diagram from memory, under 3 minutes
- [ ] Narrate a write and a read through it without prompting
- [ ] Answer every question above without opening this file
- [ ] Tell the `Writer.Offset()` race story specifically, including *why*
      it was invisible without `-race` under real concurrency
- [ ] State the current benchmark headline numbers without looking them up
- [ ] Name the one thing that's currently broken, unprompted, before being
      asked "what's wrong with it"
