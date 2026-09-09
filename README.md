# Strata

A persistent, log-structured key-value store with a Redis-compatible wire protocol. Written in Go, with no storage-engine dependencies.

> **Status: in development, Phase 1 nearly complete.** The on-disk format is specified, and the RESP2 codec, network layer, command dispatch, and connection lifecycle are written and green under the race detector against an in-memory map. No storage engine exists yet: nothing is durable, and nothing survives a restart. Benchmark numbers will appear here when they have been measured, and not before.

## What it will be

Strata stores string keys mapped to string values, persists them to disk, survives process crashes, and handles datasets far larger than available RAM.

It speaks RESP2 — the Redis wire protocol — so `redis-cli`, standard Redis client libraries, and `redis-benchmark` work against it unmodified.

Internally it is a log-structured merge-tree: writes go to a write-ahead log and a sorted in-memory buffer, which is flushed to immutable sorted files on disk; a background compactor merges those files to reclaim space and keep reads fast.

## Non-goals

Stated explicitly, because scope is a design decision:

- **No replication or clustering.** Single node.
- **No multi-key transactions.** Single-key atomicity only.
- **No Redis data types beyond strings.** No lists, sets, hashes, sorted sets, or streams.
- **No Redis feature parity.** No pub/sub, no Lua scripting.
- **Not faster than RocksDB.** This is built to be understood and measured, not to win benchmarks against production engines with years of tuning behind them.
- **Linux only.** `fsync` semantics differ enough elsewhere to be a distraction.

## Building

Requires Go 1.26 or later.

```sh
make build      # build bin/strata-server and bin/strata-cli
make test       # run all tests
make race       # run tests under the race detector
make check      # what CI runs: vet + test
make help       # list all targets
```

## Tuning defaults

Every default below traces to a measurement in [docs/benchmarks.md](docs/benchmarks.md), which records the hardware, the reproduction command, three runs and the variance.

| Parameter | Default | Why |
|---|---|---|
| Bloom bits per key | 10 | 0.83% false positive rate for 1.25 bytes per key — the knee of the measured cost curve |
| Bloom probes, k | 7 | Derived as (m/n)·ln2 and confirmed by measurement: 3.27% false positives at k=2, 0.83% at k=7, 5.50% at k=20 |
| SSTable block size | 4 KiB | **Provisional.** For random point reads, 1 KiB measured 1.5× faster with 4× less read amplification. Retained pending a range-scan benchmark. |
| Block cache | 64 MiB | 95.6% hit rate on a 78 MiB working set at Zipf s=1.05; quadrupling it to 256 MiB buys 0.6 points |

Absent-key lookups are **10.6× faster** with the bloom filter than without, measured against the identical read path with the filter bypassed.

## Documentation

| Document | Contents |
|---|---|
| [plan.md](plan.md) | The full project plan: phases, tasks, and the reasoning behind them |
| [docs/format.md](docs/format.md) | Binding on-disk format specification — WAL, SSTable, manifest |
| [docs/adr/](docs/adr/) | Architecture decision records, with alternatives and accepted costs |
| [docs/benchmarks.md](docs/benchmarks.md) | Measured results, with hardware and reproduction commands |
| [CLAUDE.md](CLAUDE.md) | Working conventions for this repository |

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
```

`internal/` is deliberate: Go forbids external modules from importing it, which states that these are implementation details rather than a public API.

## On AI assistance

The storage engine is hand-written: the skip list, the bloom filter, the block encoder and decoder, the k-way merge iterator, the compaction picker and executor, and WAL record framing and recovery. Those are the substance of the project, and generating them would defeat its purpose.

The RESP2 codec in `internal/resp/` is AI-implemented, against a test suite and fuzz target that were written first, by hand. [ADR-009](docs/adr/009-resp-codec-is-not-hand-write-surface.md) records that decision and what it costs. Repository scaffolding, the Makefile, CI configuration, and the benchmark harness are AI-assisted as well.

## License

MIT. See [LICENSE](LICENSE).
