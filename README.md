# Strata

[![CI](https://github.com/AbishekRaj2007/Strata/actions/workflows/ci.yml/badge.svg)](https://github.com/AbishekRaj2007/Strata/actions/workflows/ci.yml)
[![npm version](https://img.shields.io/npm/v/@abishekraj2007/strata.svg)](https://www.npmjs.com/package/@abishekraj2007/strata)
[![Go version](https://img.shields.io/github/go-mod/go-version/AbishekRaj2007/Strata)](go.mod)
[![license](https://img.shields.io/github/license/AbishekRaj2007/Strata)](LICENSE)
[![platform](https://img.shields.io/badge/platform-linux%20x64%20%7C%20arm64-blue)](#non-goals)

A persistent, log-structured key-value store with a Redis-compatible wire protocol. Written in Go, with no storage-engine dependencies — the skip list, SSTable format, compaction, and WAL are all hand-implemented rather than wrapping RocksDB or Pebble.

`redis-cli`, standard Redis client libraries, and `redis-benchmark`/`valkey-benchmark` work against it unmodified.

**Contents:** [Benchmarks](#benchmarks) · [Architecture](#architecture) · [Quick start](#quick-start) · [Features](#features) · [Non-goals](#non-goals) · [Design decisions](#design-decisions) · [What I learned](#what-i-learned-including-what-went-wrong) · [Documentation](#documentation) · [Building](#building) · [Layout](#layout) · [On AI assistance](#on-ai-assistance)

## Benchmarks

Measured on an 11th Gen Intel i5-11400H, 12 logical cores, 7.5 GiB RAM, NVMe/ext4, `go1.26.4`. Full hardware, sync policy, and exact reproduction command for every row: [docs/benchmarks.md](docs/benchmarks.md). Medians of at least three runs; variance and machine conditions (this is a live desktop, not a dedicated bench box) are recorded there, never rounded away.

| Workload | Throughput | p99 | Notes |
|---|---|---|---|
| SET, unpipelined, `sync=interval` | 43,917 ops/sec | 2.80 ms | clears its 30k+ target |
| SET, unpipelined, `sync=always` | 8,912.7 ops/sec (32 clients) | — | group commit: 15.2× over 1-client (584.7 ops/sec) |
| GET, absent key, blooms on | 673,854 ops/sec | 1.72 ms | 10.6× faster than blooms off (T5.3) |
| GET, mixed 80/20, 20 clients | 21,662–38,372 ops/sec | 2.05–5.90 ms | varies with `GOGC`, see below |

Space amplification after writing 1 GB and overwriting every key once, settled: **1.45×** (under the 2× target). Bloom filter: 0.83% measured false-positive rate at 10 bits/key, within 2× of the 0.82% theoretical prediction, zero false negatives across 1M probes.

`GOGC=400` gives +72% throughput and cuts p99 2.6× over the `GOGC=100` default on this machine's mixed workload, at roughly 4× peak RSS — a trade only available with memory to spare, which `GOMEMLIMIT` correctly vetoes when it isn't. Full data: [docs/gc-characterization.md](docs/gc-characterization.md).

## Architecture

```
        client (redis-cli, go-redis, redis-benchmark)
                        │  RESP2
                        ▼
            ┌─────────────────────┐
            │   internal/server    │  accept loop, connection
            │   internal/resp      │  lifecycle, command dispatch
            └──────────┬───────────┘
                        ▼
            ┌─────────────────────┐
            │   internal/engine     │  Put / Get / Delete / Scan
            └──┬────────────────┬──┘
               │ write path     │ read path
               ▼                ▼
      ┌────────────────┐  ┌──────────────────────────┐
      │  memtable (skip │  │ block cache → bloom filter │
      │  list) + WAL    │  │  → k-way merge iterator    │
      └────────┬────────┘  └─────────────┬─────────────┘
               │ flush                    │ reads
               ▼                          ▼
      ┌─────────────────────────────────────────────┐
      │        SSTables on disk, organised in         │
      │        levels (internal/manifest tracks        │
      │        which files exist and their version)    │
      └──────────────────────┬────────────────────────┘
                              │
                              ▼
                 internal/compaction picks and
                 merges files leveled-style,
                 committed atomically via the
                 manifest's fsync
```

A write appends to the write-ahead log and inserts into an in-memory skip list (the memtable); once the memtable crosses a size threshold it rotates to immutable and a background flusher writes it out as a sorted, block-encoded, checksummed SSTable, installing it into the current version via a manifest edit. A read checks the active memtable, then immutable memtables newest-first, then on-disk SSTables level by level — each level's bloom filter and cached blocks skip most of the I/O a naive scan would cost. A background compactor merges overlapping files leveled-style (ADR-002), reclaiming space and bounding how many files a read has to consult, with every state transition committed at the point the manifest's fsync returns — never before, never ambiguously.

## Quick start

```sh
git clone <this repo> && cd Strata
make build
./bin/strata-server -data-dir ./data &
redis-cli -p 6380 SET foo bar
redis-cli -p 6380 GET foo
```

Or, without cloning, via npm (linux/x64 and linux/arm64; see ADR-010):

```sh
npm install -g @abishekraj2007/strata
strata-server -data-dir ./data &
redis-cli -p 6380 SET foo bar
```

## Features

- Full `Put`/`Get`/`Delete`/`Scan` semantics over RESP2, including `SCAN`'s cursor protocol
- Crash safety: an acknowledged write survives `kill -9` at any point, verified by a fault-injection sweep that fails every I/O call individually and a randomised crash harness run as a CI job
- Leveled compaction with atomic version installation, backpressure/write-stall reporting under sustained overload, and continuous invariant checking after every compaction in test builds
- Bloom filters and a sharded block cache tuned from measurement, not guesswork
- A 50,000-operation reference-model test (scaling to 10M for the full sweep) checking every operation against an independent in-memory model, shrinking any divergence to a minimal reproducing sequence

## Non-goals

Stated explicitly, because scope is a design decision:

- **No replication or clustering.** Single node.
- **No multi-key transactions.** Single-key atomicity only.
- **No Redis data types beyond strings.** No lists, sets, hashes, sorted sets, or streams.
- **No Redis feature parity.** No pub/sub, no Lua scripting.
- **Not faster than RocksDB.** This is built to be understood and measured, not to win benchmarks against production engines with years of tuning behind them.
- **Linux only.** `fsync` semantics differ enough elsewhere to be a distraction.

## Design decisions

Each of these traded something away on purpose; the ADR states what and why.

| Decision | ADR |
|---|---|
| Go, not Rust — GC pauses accepted and quantified rather than assumed away | [ADR-001](docs/adr/001-go-not-rust.md) |
| Leveled compaction, not size-tiered — bounded read amplification over write amplification | [ADR-002](docs/adr/002-leveled-not-size-tiered.md) |
| RESP2, not RESP3 — protocol simplicity over newer client features | [ADR-003](docs/adr/003-resp2-not-resp3.md) |
| A sequence number on every write — MVCC ordering without locking readers out of writers | [ADR-004](docs/adr/004-sequence-number-per-write.md) |
| Skip list memtable — simplicity and lock-free reads over a marginally faster balanced tree | [ADR-005](docs/adr/005-skip-list-memtable.md) |
| Block-based SSTables, 4 KiB blocks — read amplification vs. index size | [ADR-006](docs/adr/006-block-based-sstables.md) |
| CRC32C on every block and WAL record — checksums are not optional, everywhere | [ADR-007](docs/adr/007-crc32c-everywhere.md) |
| Manifest as an append-only edit log — a durable, replayable commit point for every version change | [ADR-008](docs/adr/008-manifest-as-edit-log.md) |
| The RESP2 codec is not hand-write surface — AI-implemented against a hand-written test suite | [ADR-009](docs/adr/009-resp-codec-is-not-hand-write-surface.md) |

## What I learned (including what went wrong)

- **`memtableSet.Add`'s single `RWMutex` is the dominant cost, not GC and not RESP parsing.** A contention profile under mixed load put 83–97% of all block/mutex-wait samples on that one lock, persisting even under `sync=interval` — proof the cost is the lock granularity itself, not fsync sitting inside the critical section. Sharding the memtable, not micro-optimising allocations, is the next real lever ([docs/profiles/t8.2-report.md](docs/profiles/t8.2-report.md)).
- **A profile-shaped hypothesis can still be wrong about which of two related things dominates.** RESP parsing and skip-list insertion were both predicted as top allocators; skip-list insertion turned out to allocate more *bytes* per operation while RESP parsing accounted for more *allocation count* overall — the two metrics disagreed, and only the profile could say by how much.
- **Not every allocation is a bug waiting for `sync.Pool`.** An attempt to reuse the RESP reader's bulk-string buffer broke correctness immediately: unlike two other buffer-reuse wins that worked (the WAL encode buffer, the command-args slice), that buffer's contents are still owned by the caller — stored in the engine — at the moment the next read would need to reuse it. Documented with the failure and the reason in [docs/optimizations.md](docs/optimizations.md), because the failed attempt is more instructive than the ones that worked.
- **`sync=always`'s 744 ops/sec looked like a regression and was actually a wiring gap.** Group commit (`wal.Syncer`) existed and was independently benchmarked at a 5× speedup, but `memtableSet.Add` wasn't calling it yet — once wired on, the same benchmark measured a 15.2× improvement at 32 clients. The lesson wasn't "durability is slow," it was "read the call graph before believing the number."
- **Benchmarking on a live desktop is its own finding.** GET throughput on this machine showed a 30× spread between two runs of the identical command a minute apart, driven by background load this repository doesn't control. `docs/benchmarks.md` records that explicitly rather than publishing a false-precision median, because a number without its machine conditions is not reproducible.
- **This sandboxed execution environment kills CPU profiling outright.** Both the live pprof HTTP endpoint under load and `go test -cpuprofile` with no load at all are reliably `SIGKILL`ed, isolated to the `SIGPROF`/`setitimer` mechanism specifically — heap, mutex, and block profiling all work fine. T8.2's CPU-consumer ranking is disclosed as hypothesis-only rather than faked from a profile that couldn't be captured.
- **Fixed, and worth explaining why it happened.** `test/fault`'s fault-injection sweep caught an acknowledged write coming back holding a different key's value after an injected fsync failure. Root cause: a failed `fsync` does not remove the bytes a preceding `write()` already placed in the file — they are, by content and checksum, indistinguishable from a genuinely durable record — and nothing was truncating them away. The WAL writer already had a `Rollback` mechanism built for exactly this (and `internal/manifest` already used it), but it was never wired onto the WAL's own write path. The fix truncates the WAL back to the last confirmed-synced offset before the engine latches itself as unrecoverable, done under the same lock that already serialises every write to that file so it can't race a concurrent writer. `docs/interview-prep.md` has the full account.

## Documentation

| Document | Contents |
|---|---|
| [plan.md](plan.md) | The full project plan: phases, tasks, and the reasoning behind them |
| [docs/format.md](docs/format.md) | Binding on-disk format specification — WAL, SSTable, manifest |
| [docs/adr/](docs/adr/) | Architecture decision records, with alternatives and accepted costs |
| [docs/benchmarks.md](docs/benchmarks.md) | Measured results, with hardware and reproduction commands |
| [docs/profiles/t8.2-report.md](docs/profiles/t8.2-report.md) | Allocation and contention profiling report |
| [docs/optimizations.md](docs/optimizations.md) | The T8.3 optimisation cycle, including the one that failed |
| [docs/gc-characterization.md](docs/gc-characterization.md) | GC pause distribution and its measured cost to p99 |
| [docs/concurrency.md](docs/concurrency.md) | Concurrency audit: ownership, synchronisation, known contention |
| [CLAUDE.md](CLAUDE.md) | Working conventions for this repository |

### Further reading

The design draws on:

- *Designing Data-Intensive Applications* (Kleppmann) — the LSM-tree and B-tree tradeoff chapter
- The original [LevelDB](https://github.com/google/leveldb) and [RocksDB](https://github.com/facebook/rocksdb) source, for block format and compaction picker precedent
- The [Bigtable](https://research.google/pubs/pub27898/) paper, for the SSTable concept this project's format is a simplification of

## Building

Requires Go 1.26 or later.

```sh
make build      # build bin/strata-server and bin/strata-cli
make test       # run all tests
make race       # run tests under the race detector
make check      # what CI runs: vet + test
make help       # list all targets
```

## Layout

```
cmd/strata-server/    server binary: flags, config, signals, startup
cmd/strata-cli/       dev tooling: dump tables, replay the manifest, check invariants
internal/resp/        RESP2 protocol codec
internal/server/      accept loop, connection lifecycle, command dispatch
internal/engine/      public Put/Get/Delete/Scan API and orchestration
internal/memtable/    skip list
internal/wal/         write-ahead log writer, reader, recovery
internal/sstable/     table builder, reader, block encoding, iterators
internal/bloom/       bloom filter
internal/manifest/    versions, version edits, version set
internal/compaction/  compaction picker and executor
internal/cache/       block cache
internal/log/         structured logging interface
test/                 crash harness, reference model tests, workload generators
npm/                  npm packaging for `npm install -g @abishekraj2007/strata` (ADR-010)
```

`internal/` is deliberate: Go forbids external modules from importing it, which states that these are implementation details rather than a public API.

## On AI assistance

The storage engine is hand-written: the skip list, the bloom filter, the block encoder and decoder, the k-way merge iterator, the compaction picker and executor, and WAL record framing and recovery. Those are the substance of the project, and generating them would defeat its purpose.

The RESP2 codec in `internal/resp/` is AI-implemented, against a test suite and fuzz target that were written first, by hand. [ADR-009](docs/adr/009-resp-codec-is-not-hand-write-surface.md) records that decision and what it costs. Repository scaffolding, the Makefile, CI configuration, and the benchmark harness are AI-assisted as well. The Phase 8 profiling, optimisation, and GC-characterisation work — and this README — were produced with an AI coding agent (Claude Code) working directly against `plan.md`'s task specifications, measuring before writing conclusions in every case documented above.

## License

MIT. See [LICENSE](LICENSE).
