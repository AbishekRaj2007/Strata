# @abishekraj2007/strata

[![npm version](https://img.shields.io/npm/v/@abishekraj2007/strata.svg)](https://www.npmjs.com/package/@abishekraj2007/strata)
[![license](https://img.shields.io/npm/l/@abishekraj2007/strata.svg)](https://github.com/AbishekRaj2007/Strata/blob/main/LICENSE)
[![platform](https://img.shields.io/badge/platform-linux%20x64%20%7C%20arm64-blue)](#supported-platforms)

**Strata** is a persistent, log-structured key-value store with a
Redis-compatible wire protocol, written in Go with no storage-engine
dependencies — the skip list, SSTable format, compaction, and WAL are all
hand-implemented rather than wrapping RocksDB or Pebble.

This package distributes the **native `strata-server` and `strata-cli`
binaries** through npm so you can install and run Strata with tools you
already have. There is no Node.js runtime involved at request time — a thin
launcher script resolves the correct prebuilt binary for your platform and
execs it directly, forwarding signals so shutdown stays clean.

`redis-cli`, standard Redis client libraries, and `redis-benchmark` all work
against it unmodified.

---

## Contents

- [Why Strata](#why-strata)
- [Install](#install)
- [Quick start](#quick-start)
- [`strata-server` flags](#strata-server-flags)
- [`strata-cli` commands](#strata-cli-commands)
- [Architecture, in one picture](#architecture-in-one-picture)
- [Measured performance](#measured-performance)
- [Durability, verified not assumed](#durability-verified-not-assumed)
- [Non-goals](#non-goals)
- [Supported platforms](#supported-platforms)
- [How this package works](#how-this-package-works)
- [Full documentation](#full-documentation)

## Why Strata

Most embedded or self-hosted key-value stores ask you to either trust a
C/C++ dependency you can't easily audit, or hand-roll your own storage
layer from scratch. Strata is the middle path: a complete LSM-tree storage
engine — memtable, WAL, SSTables, leveled compaction, bloom filters, a
block cache — built from first principles in Go, with every durability and
performance claim backed by a real test or a real measurement rather than
an assumption. If you want a Redis-protocol server you can actually read
end to end in an afternoon, this is built for that.

## Install

```sh
npm install -g @abishekraj2007/strata
```

Or add it to a project:

```sh
npm install @abishekraj2007/strata
```

Installing pulls in exactly one of two tiny platform packages
(`@abishekraj2007/strata-linux-x64` or `@abishekraj2007/strata-linux-arm64`)
via npm's own `os`/`cpu` matching — not a postinstall script, not a runtime
download. Nothing executes at install time beyond what npm itself does.

## Quick start

```sh
# Start the server, data persisted to ./data
strata-server -addr :6380 -data-dir ./data &

# Talk to it with any Redis client
redis-cli -p 6380 SET foo bar
redis-cli -p 6380 GET foo
redis-cli -p 6380 SCAN 0

# Inspect what's actually on disk
strata-cli dump ./data/000001.sst
strata-cli validate ./data
```

Stop the server with `Ctrl-C` or `SIGTERM` — it drains in-flight
connections and flushes before exiting, the same clean shutdown path
`SIGTERM` triggers in production.

## `strata-server` flags

| Flag | Default | Meaning |
|---|---|---|
| `-addr` | `:6380` | Address to listen on |
| `-data-dir` | `./data` | Directory holding the WAL, SSTables, and manifest |
| `-sync` | `interval` | WAL sync policy: `always`, `interval`, or `never` — see [below](#durability-verified-not-assumed) |
| `-memtable-mb` | `4` | Memtable size threshold in megabytes before it rotates and flushes |
| `-cache-mb` | `64` | Block cache capacity in megabytes |
| `-log-level` | `info` | `debug`, `info`, `warn`, or `error` |
| `-pprof-addr` | *(disabled)* | Serve Go's `net/http/pprof` on this address when set |
| `-version` | — | Print the version and exit |

## `strata-cli` commands

A development and inspection tool — not required to run the server, but
useful for seeing exactly what's on disk:

| Command | What it does |
|---|---|
| `strata-cli dump <file.sst>` | Print an SSTable's contents in readable form |
| `strata-cli manifest <dir>` | Replay the manifest as a version history |
| `strata-cli levels <dir>` | Print the current level layout of a data directory |
| `strata-cli validate <dir>` | Check every structural invariant on a data directory |

## Architecture, in one picture

```
        client (redis-cli, go-redis, redis-benchmark)
                        │  RESP2
                        ▼
              accept loop, connection
              lifecycle, command dispatch
                        │
                        ▼
              Put / Get / Delete / Scan
               ┌────────┴────────┐
               │ write path      │ read path
               ▼                 ▼
      memtable (skip list)   block cache → bloom filter
         + write-ahead log   → k-way merge iterator
               │                 │
               ▼ flush           │ reads
      SSTables on disk, organised in levels, tracked
      by an append-only manifest edit log
                        │
                        ▼
         background compaction merges files
         leveled-style, committed atomically
         via the manifest's own fsync
```

A write appends to the write-ahead log and inserts into an in-memory skip
list; once that memtable fills, a background flusher writes it out as a
sorted, block-encoded, checksummed SSTable. A read checks the memtable,
then on-disk SSTables level by level — a bloom filter and the block cache
skip most of the I/O a naive scan would cost. Compaction merges files in
the background, with every state transition committed at the exact point
the manifest's fsync returns.

## Measured performance

Every number below traces to a reproducible command and stated hardware in
the main repository's [`docs/benchmarks.md`](https://github.com/AbishekRaj2007/Strata/blob/main/docs/benchmarks.md)
— measured on an 11th Gen Intel i5-11400H, 12 logical cores, NVMe/ext4,
medians of at least three runs, variance never rounded away.

| Workload | Throughput | p99 |
|---|---|---|
| SET, unpipelined, `sync=interval` | 43,917 ops/sec | 2.80 ms |
| SET, unpipelined, `sync=always`, 32 clients | 8,912.7 ops/sec | — (15.2× over 1-client, via group commit) |
| GET, absent key, blooms on | 673,854 ops/sec | 1.72 ms |

Space amplification after writing 1 GB and overwriting every key once,
settled: **1.45×** — under the 2× design target. `GOGC=400` measured +72%
throughput and 2.6× better p99 than the Go default on a mixed workload, at
the cost of roughly 4× peak memory — a trade this package leaves to you to
make via the `GOGC` environment variable, rather than picking a default
that's wrong for half of its users.

## Durability, verified not assumed

- **An acknowledged write survives `kill -9`**, verified by a randomised
  crash harness run as a continuous integration job, not merely asserted.
- **Every I/O operation in a representative workload has been individually
  failed** by a fault-injection layer, with recovery checked after each —
  including the nastiest case, a failed `fsync`, which the engine treats as
  unrecoverable and requires a restart rather than silently continuing on
  ambiguous data.
- **Checksums are verified on every read path**, including cache hits.
- A **50,000-to-10,000,000-operation reference-model test** checks every
  operation against an independent, obviously-correct in-memory model,
  shrinking any divergence to a minimal reproducing sequence.

## Non-goals

Stated explicitly, because scope is a design decision, not an oversight:

- No replication or clustering — single node only
- No multi-key transactions — single-key atomicity only
- No Redis data types beyond strings — no lists, sets, hashes, sorted sets, or streams
- No Redis feature parity — no pub/sub, no Lua scripting
- Not positioned as faster than RocksDB — built to be understood and
  measured, not to win benchmarks against engines with years of
  production tuning
- Linux only — `fsync` semantics differ enough elsewhere to be a distraction

## Supported platforms

| Platform | Package |
|---|---|
| `linux-x64` | [`@abishekraj2007/strata-linux-x64`](https://www.npmjs.com/package/@abishekraj2007/strata-linux-x64) |
| `linux-arm64` | [`@abishekraj2007/strata-linux-arm64`](https://www.npmjs.com/package/@abishekraj2007/strata-linux-arm64) |

Any other platform fails fast at launch with a clear error naming exactly
what's unsupported, rather than silently downloading or compiling
something unexpected.

## How this package works

This is the thin wrapper package: it ships a launcher script and declares
the two platform packages above as `optionalDependencies`. npm's own
`os`/`cpu` matching decides which one actually gets installed — there is
no postinstall script, no runtime download, and nothing executes until you
run `strata-server` or `strata-cli` yourself. The launcher resolves the
real binary from whichever platform package landed on disk and `exec`s it,
forwarding `SIGTERM`/`SIGINT` so the server's graceful-shutdown path
(drain connections, flush, exit) is the one that actually runs, not a
signal swallowed by a wrapper process.

## Full documentation

The complete picture — the on-disk format specification, architecture
decision records with their accepted costs, the full benchmark suite, the
profiling and GC-characterisation reports, and the concurrency audit — lives
in the [main repository](https://github.com/AbishekRaj2007/Strata):

| Document | Contents |
|---|---|
| [README](https://github.com/AbishekRaj2007/Strata#readme) | Product overview, full benchmark table, design decisions |
| [docs/format.md](https://github.com/AbishekRaj2007/Strata/blob/main/docs/format.md) | Binding on-disk format specification |
| [docs/adr/](https://github.com/AbishekRaj2007/Strata/tree/main/docs/adr) | Architecture decision records |
| [docs/benchmarks.md](https://github.com/AbishekRaj2007/Strata/blob/main/docs/benchmarks.md) | Every measured result, with hardware and reproduction commands |

## License

MIT. See [LICENSE](https://github.com/AbishekRaj2007/Strata/blob/main/LICENSE).
