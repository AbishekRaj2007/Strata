# Concurrency model

T7.4's audit: every piece of shared mutable state in Strata, what protects
it, and the argument for why each access is safe. This document is meant to
match the code exactly rather than describe an intended design — where the
two would differ, the code wins and this file is wrong until it is fixed.

## How to verify this document instead of trusting it

```
go test ./... -race -count=1                        # zero race findings
go test ./test/stress -race -stress.duration=1h -stress.clients=100  # the audit run
go test ./test/model -run TestModelSoak -model.ops=10000000          # T7.1's soak, same binary under contention
```

`test/stress/mixed_test.go` is the concurrency-specific test: 100 real RESP
clients against a real `engine.LSM` and `server.Server`, with a compaction
geometry (`L0Trigger: 2`, small memtable threshold) deliberately small enough
that flushes and compactions run continuously through the whole window
rather than only at the start. That is T7.4's trap addressed directly: *"the
race detector only finds races on paths actually executed... contrive tests
that force compaction and reads to overlap rather than hoping they happen
to."* A 30-second run at 150 clients produces on the order of 1.7M operations
with the block cache, memtable rotation, the flusher and the compactor all
active simultaneously.

## Inventory

### `internal/engine`

**`memtableSet`** (`rotation.go`) — `mu sync.RWMutex`, `cond *sync.Cond`.

Guards the active memtable slot, the immutable queue, the sequence counter,
and stall accounting. `Add` (the whole write path: rotation check, sequence
assignment, WAL append, the conditional fsync under `SyncAlways`, and the
memtable insert) holds the **full** write lock for its entire duration,
serialising every writer through one critical section. `Get` takes the read
lock.

This is deliberately not a fine-grained lock, and the reason is an ordering
argument rather than laziness: sequence numbers must reach the WAL in the
order they were assigned, because recovery replays a file in offset order. If
two writers could assign 5 and 6 and then append 6 before 5, replay would
apply the older write last and silently resurrect it. Serialising the whole
path — not just sequence assignment — is what makes that impossible by
construction rather than by careful interleaving.

**Contention finding.** The stress run's mutex profile puts essentially all
lock-hold time here: `sync.(*RWMutex).Unlock` inside `memtableSet.Add`
accounts for ~99% of measured mutex delay under `SyncAlways` (the write
path's default), and the block profile shows the matching ~98% of blocked
time in `sync.(*Mutex).Lock` waiting to enter it. This is expected, not a
bug — `SyncAlways` means an fsync syscall happens *inside* the locked
section, and every other writer queues behind it. `plan.md`'s T2.2 entry describes the
fix already built for this (`wal.Syncer`, a leader-follower group-commit
coordinator) and why it is not wired onto this path yet: doing so safely
requires a per-slot quiescence mechanism so a memtable cannot be handed to
the flusher while a write that targets it is still between its WAL append
and its memtable insert. That is real, scoped, un-started work, not a gap
this document should paper over.

**`LSM`** (`lsm.go`) — `mu sync.Mutex` guards `closed` and `fatal`; `amplMu
sync.Mutex` guards the amplification measurement (`logicalLive`,
`measured`); `userBytes` and `gets` are lock-free `atomic.Uint64` counters
read by `Stats`/`Amplification` without blocking the hot path that
increments them.

`checkOpen` and `guard` take `mu` for a pointer-sized check-and-set; nothing
expensive happens under it. `fatal` latches the first unrecoverable failure
(currently only `wal.ErrSyncFailed`) so that every operation after it
refuses rather than pretending the engine is healthy — the safety argument
is that the flag is checked at the top of every public method through
`checkOpen`, so no caller can observe state produced after the poison.

**`dirLock`** (`lock.go`) — not a Go-level primitive at all: an OS-level
`flock(2)` on `<dir>/LOCK`, held for the process's lifetime and released
automatically by the kernel on exit even if the process is killed. It
guards against two *processes* sharing one data directory, which no
in-process mutex can do. Deliberately routed around `vfs.FS`: it is a
process-level guard, not a durability operation, so it has no business going
through the fault injector.

**`Flusher`** (`flush.go`) — `flushMu sync.Mutex` serialises whole flushes
end to end (build the SSTable, two fsyncs, install the version, then
discard the memtable and delete its WAL); `mu sync.Mutex` guards the
smaller `err`/`flushed`/`bytesWritten` bookkeeping reported to `Stats`.

The safety argument for `flushMu` is explicit in its own comment: `Oldest`
and `Discard` (the two halves of one flush) are each individually safe, but
a flush spans both with I/O in between, and two flushes interleaved there
would pick the same memtable and the second `Discard` would name one
already gone. Flushes are inherently serial — queue order is what keeps
sequence numbers ascending across the tables a flusher produces — so the
lock costs nothing that was not already true.

### `internal/manifest`

**`VersionSet`** (`versionset.go`) — `current atomic.Pointer[Version]`;
`mu sync.Mutex` guards reference counts (`fileRefs`, `obsolete`); `commitMu
sync.Mutex` serialises whole commits; `nextFile atomic.Uint64`.

This is the most carefully load-bearing lock split in the codebase, and its
own doc comment states the argument precisely: installation is a single
atomic pointer store, which is what makes the **read path lock-free** — a
reader loads `current` once and works from the version it got, while a
compaction concurrently builds and installs a replacement, and neither
waits for the other. `mu` exists because *installing* is read-modify-write
(load, `Apply`, store), and two concurrent installers without it would each
derive from the same base and one would silently discard the other's work.
`commitMu` is separate from `mu` for a reason spelled out in the code: it is
held across the manifest fsync, and holding `mu` (which `Acquire` takes)
across a disk write would block every reader for the duration of that
write. Two committers exist today — the flusher committing an L0 table, and
the compactor committing a merge — so this split is exercised, not
theoretical.

### `internal/compaction`

**`Picker`** (`picker.go`) — `mu sync.Mutex` guards the per-level
round-robin pointers that stop compaction from hammering one key range
repeatedly. Called from both the compactor goroutine and the scheduler
(to decide whether to stall), so it has to be safe from more than one
caller; the lock is held only long enough to read or advance a pointer.

**`Scheduler`** (`scheduler.go`) — `runMu sync.Mutex` serialises whole
compactions (`Pick`, `Run`, `Commit` are each individually safe, but a
compaction spans all three and two overlapping ones would pick from the
same version and the second commit would name inputs the first already
deleted); `mu sync.Mutex` + `cond *sync.Cond` guard `stopped`, `err`, and
`Stats`, and is what a stalled writer waits on. `runMu` matters specifically
because compaction has two drivers — the background goroutine and any
caller of `Drain` (`FLUSHDB`/`COMPACT`) — so "compaction is serial" is an
invariant the lock enforces rather than an accident of there being one
caller.

### `internal/cache`

**`Cache`** (sharded across `shard`s) — each `shard.mu sync.Mutex` guards
that shard's LRU list and index; `hits`/`misses`/`evicted` are lock-free
atomics so a stats read never contends with the LRU lock a real request
needs. Sharding bounds lock contention under concurrent block reads: two
gets that hash to different shards never wait on each other.

**Checksum-on-every-read is part of this section's safety argument, not a
separate one.** `sstable.Table.loadBlock` re-verifies a block's checksum on
every call, cache hit included (`internal/sstable/table.go`) — cached bytes
sit in process memory for as long as the cache holds them, and a bit flip
in memory is exactly the kind of corruption a cache-bypass check would
miss. This is what stops a concurrency bug in a *different* part of the
process (a stray write through a dangling slice, say) from turning into a
silently wrong answer instead of a checksum error.

### `internal/wal`

**`Syncer`** (`syncer.go`) — `mu sync.Mutex` + `cond *sync.Cond` guard
`syncing`, `synced`, `generation`, `closed`, `syncErr`, and the throughput
counters. Fully built, unit-tested (including the falsely-signalled-writer
regression test T2.2's trap calls for), and **not currently on the live
write path** — `memtableSet.Add` fsyncs directly rather than through a
`Syncer` today; see the contention finding above. Documenting it here
rather than omitting it is deliberate: a concurrency document that only
lists what is wired in would miss the most interesting piece of built,
tested, unused synchronization in the codebase.

### `internal/memtable`

**`SkipList`** — lock-free reads via atomic forward-pointer loads
(`atomic.Pointer[node]`); `Insert` is explicitly **not** safe for concurrent
callers — its own doc comment says so — and relies on the caller serialising
writers, which `memtableSet.mu` does. Level-0 pointers are linked before
higher levels on insert, so a concurrent reader that follows a higher-level
pointer to a new node always finds that node's own level-0 pointer already
in place; it can never walk off the end into a half-linked node.

### `internal/server`

**`Server`** (`server.go`) — `mu sync.Mutex` guards the `conns` map only;
each connection's own I/O happens outside it, so a slow client cannot hold
up `Shutdown` enumerating connections. `wg sync.WaitGroup` counts in-flight
connection goroutines, and is what `Shutdown` waits on to know every
command has finished before it returns. `closing`, `connCount`, `idle`, and
`quitting` are lock-free atomics read and written from both the accept loop
and per-connection goroutines without contending a mutex for single-word
state.

## What the stress run actually exercises

`TestMixedWorkloadStress` opens a real `engine.LSM` (not the in-memory
stand-in) with a 64 KiB memtable threshold and `L0Trigger: 2`, starts a real
`server.Server` on a loopback port, and drives up to 150 concurrent RESP
clients issuing a weighted mix (40% `SET`, 35% `GET`, 10% `DEL`, 7%
`EXISTS`, 8% `SCAN`) against a 500-key keyspace small enough that clients
collide on the same keys rather than working disjoint ranges. `Compaction:
Options{Verify: true}` runs the invariant checker after every committed
compaction during the run, so a tree-structure bug surfaces as a failure at
a specific compaction rather than as a wrong answer discovered later.

After the run it shuts the server down, closes the engine, and — in the
same process, which is why this lives in a Go test rather than driving a
separate `strata-server` binary the way `test/crash` does — compares
`runtime.NumGoroutine()` and the `/proc/self/fd` count against the values
captured before `Open`, allowing a small settle tolerance rather than an
exact match. A growth past that tolerance is what a leaked goroutine or file
descriptor looks like; the same run under `-race` is what a genuine data
race looks like.

## Known gap

Group commit (T2.2) is unwired, as stated above: every writer under
`SyncAlways` pays a full fsync inside `memtableSet.mu`, and the stress
run's own profile is the evidence. This is a documented performance
limitation, not a correctness one — the current design's single lock is
exactly what keeps the WAL-order and flush-eligibility invariants trivially
true. Wiring the syncer in without a per-slot quiescence mechanism would
reopen a window where the flusher could serialize a memtable to an SSTable
before a concurrently-committing write has landed in it, which is a
real correctness regression, not a hypothetical one.
