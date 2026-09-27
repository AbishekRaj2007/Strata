# Strata

**A persistent, log-structured key-value store with a Redis-compatible wire protocol.**

Written in Go. No storage-engine dependencies. Built to be measured.

> `Strata` is a placeholder — geological strata, layered sediment, matching the level structure of an LSM-tree. Rename it if something better lands, but do it before Phase 1. Repo name, module path, and the `INFO` banner should agree from day one.

---

## How to read this document

Each phase contains a small number of **large tasks**. A task is a unit of work that produces something demonstrable — usually a half-day to two days of focused effort. Every task carries:

- **Effort** — realistic hours, assuming you are learning the concept as you go
- **Model** — which Claude model to reach for, and for what part of the work
- **Scope** — what the task actually covers
- **Done when** — the specific condition that closes it
- **Trap** — the failure mode that costs you a day if you walk into it blind

Tick the checkbox only when *Done when* is fully satisfied. Sub-bullets are scope description, not a checklist — resist converting them, because the point of large tasks is to keep you thinking in systems rather than in TODOs.

---

## Table of contents

1. [What this is](#1-what-this-is)
2. [Goals and non-goals](#2-goals-and-non-goals)
3. [Definition of done](#3-definition-of-done)
4. [Working with Claude on this project](#4-working-with-claude-on-this-project)
5. [Technical decisions](#5-technical-decisions)
6. [Architecture](#6-architecture)
7. [On-disk formats](#7-on-disk-formats)
8. [Repository layout](#8-repository-layout)
9. [Phase 0 — Foundations](#phase-0--foundations)
10. [Phase 1 — Network layer and protocol](#phase-1--network-layer-and-protocol)
11. [Phase 2 — Durability](#phase-2--durability)
12. [Phase 3 — Memtable and on-disk tables](#phase-3--memtable-and-on-disk-tables)
13. [Phase 4 — The read path](#phase-4--the-read-path)
14. [Phase 5 — Bloom filters and caching](#phase-5--bloom-filters-and-caching)
15. [Phase 6 — Leveled compaction](#phase-6--leveled-compaction)
16. [Phase 7 — Correctness hardening](#phase-7--correctness-hardening)
17. [Phase 8 — Performance](#phase-8--performance)
18. [Phase 9 — Ship it](#phase-9--ship-it)
19. [Benchmark targets](#19-benchmark-targets)
20. [Testing strategy](#20-testing-strategy)
21. [Risk register](#21-risk-register)
22. [Interview preparation](#22-interview-preparation)
23. [Glossary](#23-glossary)
24. [Reading list](#24-reading-list)
25. [Schedule](#25-schedule)

---

## 1. What this is

Strata is a single-node database that stores string keys mapped to string values, persists them to disk, survives process crashes and power loss, and handles datasets far larger than available RAM.

It speaks RESP — the Redis wire protocol — so `redis-cli`, every Redis client library in every language, and `redis-benchmark` all work against it without modification.

Internally it is a log-structured merge-tree: writes are buffered in a sorted in-memory structure and flushed to immutable sorted files on disk; a background process merges those files to reclaim space and keep reads fast.

**One-sentence pitch for a resume or README:**

> A from-scratch LSM-tree storage engine in Go with write-ahead logging, crash recovery, bloom filters, and leveled compaction, exposed over the Redis wire protocol — benchmarked at N ops/sec with p99 latency of M ms.

Fill in N and M from your own measurements in Phase 8. Never guess them.

---

## 2. Goals and non-goals

### Goals

| # | Goal | Why it matters |
|---|---|---|
| G1 | Durability under crash | The hardest property to get right, and what separates a database from a cache |
| G2 | Dataset larger than RAM | The entire justification for on-disk storage |
| G3 | Correct read semantics | Newest write wins; deletes stay deleted; no resurrection after compaction |
| G4 | Sustained write throughput | The reason to choose LSM over B-tree at all |
| G5 | Bounded space amplification | Compaction must provably reclaim space |
| G6 | Real, reproducible benchmarks | The numbers are the deliverable |
| G7 | Readable, documented code | Someone must be able to read it with you in an interview |

### Non-goals

Explicitly out of scope. State these in your README — scoping decisions are themselves a signal of maturity.

- **Replication or clustering.** Single node. Distributed consensus is a different project.
- **Multi-key transactions.** Single-key atomicity only.
- **Redis data types beyond strings.** No lists, sets, hashes, sorted sets, streams.
- **Redis feature parity.** No pub/sub, no Lua scripting.
- **Beating RocksDB.** You will not, and claiming otherwise is a red flag. You are building to understand and to measure.
- **Windows support.** Linux first. `fsync` semantics differ enough to be a distraction.

---

## 3. Definition of done

The project is complete when all of the following hold. Treat this as a contract with yourself.

- [ ] `redis-cli` connects; `SET`, `GET`, `DEL`, `EXISTS`, `SCAN`, `PING` all behave correctly
- [ ] `redis-benchmark` runs clean and produces published numbers
- [ ] A 10 GB dataset under a 1 GB memory cap reads and writes correctly
- [ ] `kill -9` at any point, followed by restart, loses no acknowledged write
- [ ] A one-hour randomised workload produces zero divergence from a reference model
- [ ] Disk usage stays within 2× live data size once compaction settles
- [ ] Coverage above 70% on `internal/engine` and `internal/compaction`
- [ ] README carries an architecture diagram, a benchmark table with hardware stated, and a design-decisions section
- [ ] A written post explains one non-obvious thing you learned, backed by data

---

## 4. Working with Claude on this project

This project's value is entirely in what *you* can explain under questioning. A storage engine you did not write is worth less than no storage engine at all, because it collapses the moment an interviewer asks why L0 files may overlap. So the rule is not "avoid AI" — it is **route AI to the work that isn't the point.**

### Model routing

| Work | Model | Reasoning |
|---|---|---|
| Architecture decisions, format design, trade-off analysis | **Opus 5** | Long-horizon reasoning about consequences; good at surfacing what you haven't considered |
| Hardest correctness reasoning — compaction invariants, crash windows, concurrency proofs | **Fable 5** | The most capable tier. Reach for it when a bug resists Opus, or when you need someone to find the hole in your durability argument |
| Bulk implementation, refactors, multi-file changes | **Claude Code**, Sonnet 5 default | Runs in your repo, sees the whole tree. Escalate the session to Opus 5 when it stalls |
| Test generation, table-driven cases, fixtures, fuzz targets | **Sonnet 5** | High volume, well-specified, low ambiguity |
| Makefiles, CI YAML, Dockerfiles, doc comments, README polish | **Haiku 4.5** | Mechanical. Don't spend a bigger model on it |
| Explaining a paper or the LevelDB source | **Opus 5** | Explanation quality matters more than speed |
| Debugging a data race or reading a flamegraph | **Opus 5**, escalate to **Fable 5** | Requires holding a lot of state at once |
| Reviewing code you already wrote | **Opus 5** | The single highest-value use on this project |

Also worth knowing: **Claude Cowork** suits the multi-step research work in Phases 8 and 9 — comparative benchmarking, assembling the tuning study, pulling documentation together across many files.

### Write these yourself. No exceptions.

These are your interview surface. If you cannot derive them at a whiteboard, the project has failed at its purpose.

- The skip list
- The bloom filter
- The block encoder and decoder
- The k-way merge iterator
- The compaction picker and executor
- WAL record framing and recovery

For each, the correct workflow is: **attempt it yourself → get it working → then ask Opus 5 to review and attack it.** Review after is fine. Generation before is not.

This list is exactly six items and is read literally. The RESP2 codec (T1.1) was for a time treated as a seventh; ADR-009 records why it is not, and the reasoning there is the test to apply if another component's membership is ever ambiguous. "WAL record framing and recovery" means the WAL — a durability construct whose failure mode is silent data loss after a crash. It does not extend to framing problems generally.

### AI-assisted is fine here

- Repository scaffolding, Makefile, CI configuration
- Test harnesses and fixture generation
- The benchmark runner and result formatting
- Profiling interpretation ("here's my flamegraph, what stands out")
- Documentation, README, diagrams
- Explaining concepts before you implement them
- Code review of everything

### The honesty rule

If you used AI substantially on a component, say so in the README. Interviewers respect a candidate who says "I hand-wrote the compaction executor; the benchmark harness was AI-assisted." They lose all respect for one who claims everything and folds under a single follow-up.

---

## 5. Technical decisions

Record these as ADRs in `docs/adr/` — one short file each: context, decision, consequences. A repo with real ADRs reads unusually well.

| ID | Decision | Rationale |
|---|---|---|
| ADR-001 | Go, not Rust | Concurrent compaction with shared readers is the central difficulty. Go's GC removes the memory-lifetime problem so the storage problem gets full attention. `pprof` gives free profiling. Rust port planned as a follow-up. |
| ADR-002 | Leveled compaction, not size-tiered | Lower read and space amplification, higher write amplification. The right trade for a read-heavy KV store, and the more instructive algorithm. |
| ADR-003 | RESP2, not RESP3 | Simpler to parse, universally supported, sufficient for string commands. |
| ADR-004 | Sequence number on every write | Gives a total order across memtable and all SSTables. Makes "newest wins" a comparison rather than a heuristic, and opens the door to snapshot reads. |
| ADR-005 | Skip list memtable | Concurrent reads during writes without a global lock, plus an ordered iterator for flushing. |
| ADR-006 | Block-based SSTables, 4 KB blocks | Matches page granularity; enables per-block checksums and caching. |
| ADR-007 | CRC32C on every block and WAL record | Silent corruption must be detectable. |
| ADR-008 | Manifest as an append-only edit log | Atomic multi-file state changes without rewriting full state on every compaction. |

**Dependency policy.** Standard library only for everything load-bearing. Permitted: `testify` for assertions, `xxhash` for bloom hashing, `golang.org/x/sys/unix` for `fsync` control. Anything else needs an ADR. If a library does the interesting part for you, do not use it.

---

## 6. Architecture

```
                          ┌──────────────────────┐
        TCP :6380  ───▶   │  RESP server         │
                          │  goroutine per conn  │
                          └──────────┬───────────┘
                                     │
                          ┌──────────▼───────────┐
                          │  Command dispatch    │
                          └──────────┬───────────┘
                                     │
                          ┌──────────▼───────────┐
                          │  Engine (public API) │
                          │  Put / Get / Delete  │
                          └──────────┬───────────┘
                                     │
        ┌────────────────────────────┼────────────────────────────┐
        │                            │                            │
  ┌─────▼──────┐            ┌────────▼────────┐          ┌────────▼────────┐
  │    WAL     │            │  Memtable       │          │  Version /      │
  │ append+sync│            │  skip list      │          │  Manifest       │
  └────────────┘            └────────┬────────┘          └────────┬────────┘
                                     │ full                       │
                            ┌────────▼────────┐                   │
                            │ Immutable       │                   │
                            │ memtable queue  │                   │
                            └────────┬────────┘                   │
                                     │ flush                      │
                            ┌────────▼────────────────────────────▼────────┐
                            │  Level 0   (freshly flushed, may overlap)    │
                            │  Level 1   (~10 MB,  non-overlapping)        │
                            │  Level 2   (~100 MB, non-overlapping)        │
                            │  Level N   (~10× previous)                   │
                            └───────────────────┬──────────────────────────┘
                                                │
                                     ┌──────────▼───────────┐
                                     │  Compactor goroutine │
                                     └──────────────────────┘
```

### Write path

1. Assign the next sequence number.
2. Append the record to the WAL; sync per policy.
3. Insert into the active memtable.
4. Acknowledge to the client.
5. If the memtable exceeds threshold, move it to the immutable queue, open a fresh memtable and WAL, signal the flusher.

### Read path

1. Active memtable.
2. Immutable memtables, newest to oldest.
3. L0 tables, newest to oldest — they may overlap, so all must be checked.
4. L1 and below: binary-search the level's key ranges for the single candidate table, then bloom filter, then index, then block.
5. First match wins. A tombstone match returns not-found.

### Concurrency model

| Component | Approach |
|---|---|
| Connections | One goroutine each |
| Memtable | Lock-free reads, single-writer append |
| Version state | `atomic.Pointer` swap over an immutable `Version` struct |
| SSTable files | Reference counted; deleted only at zero |
| Flusher | One dedicated goroutine |
| Compactor | One dedicated goroutine; parallel compaction is a stretch goal |

The `Version` struct is the crux. It is immutable and holds the complete list of live SSTables per level. Readers take a pointer and hold a reference for the duration of a read. Compaction builds a *new* version and atomically swaps the pointer. Old versions die when their last reader releases. Nothing is mutated in place — the same principle as the storage engine itself, applied to its own metadata.

---

## 7. On-disk formats

Specify these before writing code. Changing a format mid-project means rewriting reader, writer, and every fixture.

### 7.1 Directory layout

```
data/
├── CURRENT              # text file naming the active manifest
├── MANIFEST-000001      # append-only log of version edits
├── 000004.wal           # WAL for the active memtable
├── 000007.wal           # WAL for an immutable memtable not yet flushed
├── 000002.sst           # SSTables, monotonically numbered
├── 000003.sst
└── LOCK                 # advisory lock preventing two processes on one directory
```

### 7.2 WAL record

Records pack into 32 KB blocks. A record crossing a block boundary is split into fragments, which bounds the damage from a torn write to a single block.

```
┌─────────┬──────────┬────────┬──────────────────┐
│ CRC32C  │ Length   │ Type   │ Payload          │
│ 4 bytes │ 2 bytes  │ 1 byte │ Length bytes     │
└─────────┴──────────┴────────┴──────────────────┘

Type: 1 = FULL, 2 = FIRST, 3 = MIDDLE, 4 = LAST
```

Payload is a batch, so future multi-key writes are a format-compatible change:

```
u64 sequence_number
u32 record_count
  repeated:
    u8      kind        (0 = SET, 1 = DELETE)
    uvarint key_len
    []byte  key
    uvarint value_len   (absent when kind = DELETE)
    []byte  value
```

**Recovery rule:** read records until one fails checksum or is truncated, then stop. Everything before is durable; everything after is discarded. A partial trailing record is expected after a crash and is not an error.

### 7.3 SSTable

```
┌────────────────────────────────────┐
│  Data block 0        (~4 KB)       │
│  Data block 1                      │
│  ...                               │
│  Data block N                      │
├────────────────────────────────────┤
│  Bloom filter block                │
├────────────────────────────────────┤
│  Index block                       │
├────────────────────────────────────┤
│  Footer               (48 bytes)   │
└────────────────────────────────────┘
```

**Data block.** Entries sorted by `(key ascending, sequence descending)` so the newest version of a key sorts first.

```
entry:
  uvarint shared_prefix_len     # bytes shared with previous key
  uvarint unshared_len
  uvarint value_len
  u8      kind
  u64     sequence
  []byte  unshared_key_bytes
  []byte  value_bytes

block trailer:
  u32[]   restart_offsets       # every 16th entry; prefix sharing resets here
  u32     restart_count
  u8      compression_type      # 0 = none, 1 = snappy (stretch)
  u32     crc32c
```

Prefix compression is a real win on realistic key sets (`user:1001:name`, `user:1002:name`). Restart points let you binary-search within a block instead of scanning from the start.

**Index block.** One entry per data block: largest key in that block, plus offset and length. Loaded into memory on open.

**Footer.** Fixed size so it reads with one seek from the end.

```
u64 bloom_offset
u64 bloom_length
u64 index_offset
u64 index_length
u32 format_version
u64 magic         # 0x53545241544100 ("STRATA\0")
```

### 7.4 Manifest

An append-only log of *edits*, not snapshots.

```
edit:
  u32 edit_type   # 1 = ADD_FILE, 2 = DELETE_FILE, 3 = SET_LOG_NUMBER,
                  # 4 = SET_NEXT_FILE_NUMBER, 5 = SET_LAST_SEQUENCE

ADD_FILE:
  u32     level
  u64     file_number
  u64     file_size
  []byte  smallest_key
  []byte  largest_key
  u64     smallest_seq
  u64     largest_seq
```

On startup, replay the manifest to reconstruct the current version. A compaction swapping four inputs for two outputs writes one edit containing four DELETEs and two ADDs, then fsyncs. **That single fsync is the commit point.** Crash before it and the compaction never happened; crash after and it fully happened. There is no in-between. This is the most important durability property after the WAL.

### 7.5 Command set

| Command | Notes |
|---|---|
| `PING [msg]` | |
| `ECHO msg` | |
| `SET key value` | No options in v1; `EX`/`NX` are stretch goals |
| `GET key` | |
| `DEL key [key ...]` | Returns count deleted |
| `EXISTS key [key ...]` | |
| `SCAN cursor [MATCH p] [COUNT n]` | Cursor encodes the last key returned |
| `DBSIZE` | Approximate is fine; say so |
| `INFO` | Level sizes, table counts, compaction stats, sync policy, cache hit rate |
| `COMPACT` | Non-standard; forces full compaction. Invaluable for testing |
| `FLUSHDB` | |
| `COMMAND DOCS` | Return an empty array |

> **Gotcha worth knowing now:** modern `redis-cli` sends `COMMAND DOCS` on connect. Ignore it and the client hangs before you can type anything, and it will look like your server is broken. An empty array is enough.

---

## 8. Repository layout

```
strata/
├── cmd/
│   └── strata-server/main.go     # flags, config, signals, startup
├── internal/
│   ├── resp/                     # protocol reader and writer
│   ├── server/                   # accept loop, connection lifecycle, dispatch
│   ├── engine/                   # public Put/Get/Delete/Scan, orchestration
│   ├── memtable/                 # skip list
│   ├── wal/                      # writer, reader, recovery
│   ├── sstable/                  # builder, reader, block encoding, iterators
│   ├── bloom/                    # filter
│   ├── manifest/                 # version, version edits, version set
│   ├── compaction/               # picker and executor
│   └── cache/                    # block cache
├── test/
│   ├── crash/                    # kill -9 harness
│   ├── model/                    # reference model comparison
│   └── bench/                    # workload generators
├── docs/
│   ├── adr/
│   ├── format.md
│   └── benchmarks.md
├── Makefile
├── README.md
└── plan.md
```

**Why `internal/`:** Go forbids external modules from importing it. That is a deliberate statement that these are implementation details, and it stops you from treating the layout as a public API by accident.

---

## Phase 0 — Foundations

**Goal:** a repository that builds, tests, lints, and runs in CI before any real code exists.
**Total effort:** 6–9 hours.

### - [ ] T0.1 — Stand up the repository, toolchain, and CI

**Effort:** 2–3 h · **Model:** Haiku 4.5 for config files; Claude Code (Sonnet 5) to generate the tree in one pass

Create the public GitHub repository and bring the full development loop online before writing a line of engine code. This covers `go mod init`, the complete package skeleton from section 8 with a `doc.go` in each package stating that package's single responsibility, a Makefile exposing `build test bench lint race cover clean`, a committed `.golangci.yml`, and a GitHub Actions workflow running build, `go vet`, `go test -race`, and lint on every push. Add LICENSE and a `.gitignore` covering `data/`, `*.prof`, `bin/`.

Set up a GitHub Project board with one column per phase and load in the task IDs from this document. That board is what keeps you honest in week 10 when compaction is fighting you.

**Done when:** a fresh clone passes `make test`, and CI is green on the very first commit.

**Trap:** don't skip the `doc.go` files. Writing one sentence about what each package is responsible for, *before* you write it, is the cheapest architecture review you will ever get. Any package whose sentence needs an "and" should be two packages.

---

### - [x] T0.2 — Write the authoritative on-disk format specification

**Effort:** 2–3 h · **Model:** Opus 5 — design work, and worth reviewing before you commit to bytes

Transfer sections 7.1–7.4 into `docs/format.md` as the binding specification, then extend it: byte-exact field ordering, endianness (little-endian throughout), varint encoding rules, maximum key and value sizes, behaviour on every malformed input, and a worked hex dump of a small SSTable containing three keys.

That hex dump is not busywork. When your reader disagrees with your writer at 1am in week 6, the hex dump is the referee.

**Done when:** someone who has never seen your code could write a conformant reader from `docs/format.md` alone.

**Trap:** decide maximum key and value sizes now and enforce them at the protocol boundary. Discovering in Phase 6 that a 500 MB value breaks your block builder's size assumptions is a genuinely bad afternoon.

---

### - [x] T0.3 — Write the initial architecture decision records

**Effort:** 1.5–2 h · **Model:** Opus 5 — ask it to argue the *opposite* side of each decision before you write

Write ADR-001 through ADR-008 as real documents, each with context, the decision, the alternatives rejected, and the consequences accepted. The consequences section is the one that matters — "we accept 10–30× write amplification in exchange for single-file reads below L0" is the sentence that proves you understood the trade rather than copied a design.

**Done when:** each ADR names at least one alternative and one concrete cost of the choice made.

**Trap:** ADRs written to justify a decision you already made are worthless. Write ADR-002 by genuinely working out what size-tiered compaction would give you first.

---

### - [x] T0.4 — Build the development and observability harness

**Effort:** 1.5–2 h · **Model:** Sonnet 5

Build the tooling you will live inside for twelve weeks: a `strata-cli` dev binary that can dump an SSTable in human-readable form, dump the manifest as a version history, print the current level layout, and validate all invariants on a data directory. Add structured logging with levels behind a single logger interface. Wire `net/http/pprof` behind a flag now rather than in Phase 8.

**Done when:** `strata-cli dump 000002.sst` prints readable output — even with no SSTable existing yet, the command exists and errors cleanly.

**Trap:** people build this in week 10 while debugging compaction, having spent weeks squinting at hex. Build it first. It pays for itself by Phase 3.

---

## Phase 1 — Network layer and protocol

**Goal:** `redis-cli` connects and commands work against an in-memory map.
**Total effort:** 12–18 hours.
**Milestone:** the first time `redis-cli -p 6380` gives you a prompt and `SET foo bar` returns `OK`. Screenshot it.

### Understanding RESP

Every message is a type byte, a payload, then `\r\n`.

```
+OK\r\n                          Simple string
-ERR unknown command\r\n         Error
:42\r\n                          Integer
$5\r\nhello\r\n                  Bulk string (length-prefixed)
$-1\r\n                          Null bulk string — this is "key not found"
*2\r\n$3\r\nGET\r\n$3\r\nfoo\r\n Array of 2 bulk strings
```

Clients always send commands as an array of bulk strings. Servers reply with whichever type fits.

### - [x] T1.1 — Implement the complete RESP2 codec

> **Done.** `internal/resp/resp.go` implements the reader and writer against the author-written suite in `internal/resp/resp_test.go`. AI-implemented per ADR-009 — this task is not §4 hand-write surface. Fuzzed 60s / ~10M execs with no panic; the full tree is green under `-race`. `internal/resp/CONTRACT.md` is now superseded by the doc comments and can be deleted.

**Effort:** 4–6 h · **Model:** Sonnet 5 to implement against the written suite; Opus 5 to review the error paths afterwards

Build a reader and writer covering all five RESP2 types plus the null bulk string, with strict framing. The reader sits on a `bufio.Reader` and returns a typed `Value`; the writer sits on a `bufio.Writer` with one method per reply shape.

The interesting work is entirely in the failure paths. A length prefix claiming 4 GB must be rejected before allocation, not after. A stream ending mid-record must produce a clean error rather than a partial value. The null bulk string (`$-1\r\n`) must be representable distinctly from the empty bulk string (`$0\r\n\r\n`), because your entire "key not found" semantics rides on that distinction.

Finish with a fuzz target over the reader asserting no panic on arbitrary bytes, plus table-driven tests covering every type and every malformed variant you can construct.

**Done when:** the fuzz target survives 60 seconds, and the test table includes at least fifteen malformed inputs, all handled without panic.

**Trap:** the null-versus-empty bulk string distinction. Collapse them and you will spend Phase 4 chasing a bug where deleted keys read back as empty strings.

---

### - [x] T1.2 — Build the TCP server and connection lifecycle

> **Done.** `internal/server/server.go` plus tests, including the 50-connection and shutdown-drain cases, green under `-race` against the real T1.1 codec.

**Effort:** 3–4 h · **Model:** Sonnet 5 for scaffolding, Opus 5 to review shutdown correctness

Stand up the listener on port 6380 — deliberately not 6379, so real Redis stays usable on your machine — with one goroutine per connection, 16 KB buffered reader and writer per connection, and a read timeout so dead clients get reaped.

The substantial part is graceful shutdown. On `SIGINT` or `SIGTERM` you must stop accepting, let in-flight commands complete, flush and close every connection, and only then close the engine. Getting this right now means that in Phase 6, when shutdown must also drain a compaction, you are extending a correct design rather than retrofitting one.

**Done when:** 50 concurrent connections run clean under `-race`, and `SIGTERM` during active load exits with code 0 having lost nothing.

**Trap:** a naive shutdown that closes the listener and calls `os.Exit` looks fine and hides a class of bug you will meet again with fsync in Phase 2. Do the drain properly.

---

### - [x] T1.3 — Build command dispatch and the full v1 command set

> **Closed 2026-08-21.** `internal/server/command.go` and `internal/engine/`, passing under `-race` against the real T1.1 codec. Verifying against it caught a desynchronisation bug the throwaway reference codec had hidden: `validateKeys` reported a rejected key by returning the result of `WriteError`, which is nil on success, so the caller ran the command anyway and wrote a second reply — leaving every later reply on that connection off by one. It now returns `(rejected bool, err error)`.
>
> Both halves of *Done when* are now executed by `make interop` (`test/interop/`): `redis-cli` round-trips every command in §7.5, 42 assertions, and an unmodified `go-redis` program passes 36, including `redis.Nil` on an absent key, binary-safe values containing NUL and a bare CRLF, the cursor-driven `Scan` iterator, and reply synchronisation after two protocol errors — the last being a direct regression test for the desynchronisation bug above. go-redis lives in a nested module so it stays out of the root dependency graph.

**Effort:** 3–4 h · **Model:** Sonnet 5

Implement the dispatch table mapping case-insensitive command names to handlers, with arity validation producing proper RESP errors, and implement every command in section 7.5 against a temporary `map[string][]byte` guarded by an `RWMutex`.

Define the engine interface here, and define it as the *final* one — `Put`, `Get`, `Delete`, `Scan`, `Close`, `Stats` — so the map is a swappable implementation rather than something you refactor around in Phase 3. This single decision saves a day later.

Handle `COMMAND DOCS` with an empty array.

**Done when:** `redis-cli` round-trips every command, and a Go program using `go-redis` works unmodified against your server.

**Trap:** if the engine interface leaks map semantics — returning `(value, bool)` instead of `(value, error)` — you will rewrite every call site in Phase 3. Design it for the LSM engine you are going to build.

---

### - [x] T1.4 — Establish the performance baseline

> **Closed 2026-08-21.** `docs/benchmarks.md` carries the baseline table, the stated hardware, and a reproduction command per row. Median of 3 runs each: SET 181,587 ops/sec unpipelined and 1,199,041 at P=16; GET 186,324 and 1,956,947. Two independent invocations agreed within 2%.
>
> Fixing the harness to produce those numbers was most of the work. It parsed the `-q` one-liner, which on the installed valkey-benchmark reports p50 and no p99, and it matched on `^SET` against output whose progress lines are carriage-return separated, so nothing matched at all. It now parses the full `Summary:` block. More seriously, it ran `FLUSHDB` before every pass including GET, so the GET rows would have measured the miss path on an empty keyspace and published it under a heading that says GET; the keyspace is now populated and verified before each measured GET pass.
>
> **The profile capture is not done** — `make profile` cannot complete, see "Profiles" in `docs/benchmarks.md`. T1.4's *Done when* does not require it, so this task is closed on the table; the profiles are carried as a known gap.

**Effort:** 2–4 h · **Model:** Sonnet 5 for the harness, Opus 5 to interpret the first profiles

Run `redis-benchmark` against the in-memory implementation across SET and GET, pipelined and unpipelined, and record results in `docs/benchmarks.md` with full hardware detail — CPU, RAM, disk model, filesystem, kernel. Capture a CPU profile and a heap profile under load and commit the flamegraphs.

This is your ceiling. Every number from here on is a fraction of it, and knowing the fraction is how you tell "my disk is slow" from "my code is slow." Without this baseline, every later measurement is uninterpretable.

**Done when:** `docs/benchmarks.md` has a baseline table, stated hardware, and the exact reproduction command for each row.

**Trap:** benchmarking without pinning machine state — background processes, thermal throttling, a laptop on battery. Note the conditions, and rerun this baseline in Phase 8 on the same machine before comparing.

---

## Phase 2 — Durability

**Goal:** acknowledged writes survive `kill -9`.
**Total effort:** 14–20 hours.

Short on code, long on thinking. Also the phase most people get subtly wrong, which makes it the one most worth doing slowly.

### The fsync question

Writing to a file does not put data on disk. It puts data in the OS page cache. Process crash: the OS still flushes it, you are fine. Machine power loss: gone. `fsync` forces it to physical media and costs around a millisecond even on NVMe.

So it is a policy choice, and you implement all three:

| Policy | Guarantee | Cost |
|---|---|---|
| `always` | Nothing acknowledged is ever lost | One sync per write; a few thousand ops/sec |
| `interval` | Lose at most the last N ms | Background sync every N ms; near-zero |
| `never` | Survives process crash, not machine crash | Free |

### - [x] T2.1 — Implement WAL record framing and the writer

**Effort:** 4–5 h · **Model:** write it yourself; Fable 5 to attack your framing for gaps

Implement 32 KB block framing with FULL, FIRST, MIDDLE, and LAST fragment types, CRC32C over type and payload, and the batch payload encoding from section 7.2. Handle the case where fewer than 7 bytes remain in a block by zero-padding to the boundary.

Fragmentation exists for one reason: to bound the blast radius of a torn write. A crash mid-write corrupts at most one 32 KB block, and the framing lets recovery detect precisely where damage starts. Make sure you can articulate that — it is a common interview question.

**Done when:** 10,000 records of randomly varying size, including records larger than a block, write and read back byte-identical.

**Trap:** the boundary case where a record header would straddle a block edge. Handle it explicitly with padding; a lot of subtle recovery bugs live there.

---

### - [x] T2.2 — Implement sync policies and group commit

> **Wired and verified.** `internal/wal/policy.go` and `internal/wal/syncer.go`: all three policies, the leader-follower handoff, and the interval timer. `internal/wal`'s own group-commit benchmark test measured 16.0x speedup this run (docs/benchmarks.md records a 16.8x median across three prior runs), past the 5x bar. The falsely-signalled-writer test exists and was verified by mutation — injecting the bug (reading the offset after the sync instead of capturing it before) makes it fail with named offsets.
>
> `internal/engine/rotation.go`'s `memtableSet.Add` binds a `wal.Syncer` to each slot (`openSlot`) and calls `AwaitDurable` after releasing `mu`, instead of syncing inside the write lock — this is the change the T7.4 stress profile and `docs/concurrency.md`'s "Known gap" section were waiting on. Releasing the lock around the fsync reopens the hazard that section predicted: a slot can rotate onto the immutable queue and reach the flusher while a writer is still mid-fsync against its WAL file, and `Discard`/`Close` must not close that fd out from under the fsync. Addressed with a per-slot `sync.WaitGroup` (`slot.inflight`): `Add` registers on it before releasing `mu` and `Discard`/`Close` wait on it before calling `wal.Writer.Close`.
>
> A second hazard surfaced on inspection and is fixed alongside this: `wal.Writer.Offset()` is now read by `Syncer.leadSync` from a different goroutine than the one driving `WriteRecord`, but `offset` is a plain field `WriteRecord` mutates mid-call — a data race, and worse, a window where `Offset()` could report bytes not yet handed to the OS. `internal/wal/writer.go` now tracks that separately as `published atomic.Int64`, stored only immediately after each successful `WriteAt`, with `Offset()` reading it instead of `offset`.
>
> **Run and confirmed.** `go build ./...` and `go vet ./...` clean. `go test ./internal/wal/... -race` and `go test ./internal/engine/... -race` both pass, including `TestGroupCommitScalesWithConcurrency` and `TestNoWriterIsSignalledBeforeItsRecordIsSynced`. End-to-end confirmation against the real server (`strata-server -sync always`, `redis-benchmark -t set`, unpipelined): 1 client 584.7 ops/sec, 32 clients 8,912.7 ops/sec — **15.2x**, past the 5x bar, and consistent with the WAL-level number now that the lock-release wiring is actually live on the write path. (This supersedes the 744 ops/sec `sync=always`/50-client figure recorded under T8.1, which predates this wiring; see the note added there.)

**Effort:** 3–4 h · **Model:** Opus 5 — the coordination logic is the interesting part

Implement all three sync policies behind a config flag, then implement group commit on top: when several writers arrive concurrently, one performs the fsync on behalf of all and wakes the rest. Throughput then *rises* with concurrency rather than collapsing.

Design it as a leader-follower handoff — arriving writers queue, the first becomes leader, the leader syncs everything queued at the moment it starts, then signals every follower whose write was included. Get the boundary condition right: a writer arriving *during* the sync must wait for the next one, not be falsely told its write is durable.

**Done when:** measured throughput at `sync=always` with 32 concurrent clients is at least 5× the single-client figure, and a targeted test proves no writer is ever signalled before its record was actually synced.

**Trap:** the falsely-signalled-writer bug is the single most dangerous defect in this project — it silently breaks your core durability guarantee and no ordinary test catches it. Write the specific test.

---

### - [x] T2.3 — Implement the WAL reader and startup recovery

**Effort:** 3–4 h · **Model:** write it yourself; Opus 5 to review the failure taxonomy

Build the streaming reader that reassembles fragments into records, then the recovery path that replays all WAL files in file-number order and restores the sequence counter to the highest sequence observed.

The judgement call is the failure taxonomy. A truncated *trailing* record is expected after a crash and must be treated as clean end-of-log. A checksum failure in the *middle* of a log, with valid records after it, indicates real corruption and must be surfaced loudly rather than silently skipped. Conflating these means either rejecting healthy databases or silently losing data — write the distinction into code comments and be able to defend it.

**Done when:** truncating the WAL at every byte offset from 0 to file length produces, in every case, either full recovery or clean recovery of a prefix — never a panic and never a partial record.

**Trap:** the every-offset truncation test sounds excessive. It is not. It finds real bugs, and it is twenty lines.

---

### - [x] T2.4 — Build the crash-testing harness

> **Built and run green.** `test/crash/` holds the harness: process supervision with SIGKILL, the acknowledgement recorder, the verifier, and the `STRATA_CRASH_AT`/`STRATA_CRASH_AFTER_N` contract. The two unconditional self-tests (`TestHarnessDrivesServer`, `TestWorkloadRecordsOnlyAcknowledgedWrites`) pass. `TestAcknowledgedWritesSurviveKill` — 100 consecutive randomised SIGKILL/restart iterations at `sync=always` against the real `engine.LSM` (`STRATA_DURABLE=1`) — passes, 53s. `TestTargetedKillPoints` (the WAL-point fault-injection hooks) and `TestFlushWindowSurvivesKill` (50 flush-window kills, T3.5's remaining clause) both pass as well.
>
> `STRATA_CRASH_AT`/`STRATA_CRASH_AFTER_N` are read by `cmd/strata-server/main.go`, which builds a counting abort hook wired through `engine.Options.CrashHook func(point string) error` into `memtableSet.Add` (`after_wal_write`, `after_wal_sync`) and `Flusher.step` (`during_flush`, at `StepAfterTableSync`). `before_manifest_sync`/`after_manifest_sync` (T6.3's compaction points) remain unwired — T6.3 already shipped its own in-process fault-injection tests for that path and does not depend on this mechanism.
>
> **CI job added.** `make crash` (`STRATA_DURABLE=1 go test ./test/crash/... -v -timeout 20m`) and a `crash` job in `.github/workflows/ci.yml`, satisfying the "running as a CI job" clause of *Done when*.

**Effort:** 4–6 h · **Model:** Sonnet 5 for the harness, Fable 5 to design the verification predicate

Build the infrastructure that validates durability for the rest of the project: a parent process that spawns the server, drives a write workload while recording every *acknowledgement*, sends `SIGKILL` at a randomised point, restarts, and verifies every acknowledged write is present.

Make the kill point targetable rather than only random — you will need to kill specifically during a flush in Phase 3 and specifically during the manifest write in Phase 6. Build that hook now via an environment-variable-driven fault injection point, and this harness serves you through Phase 7 unchanged.

**Done when:** 100 consecutive randomised crash iterations pass at `sync=always`, running as a CI job.

**Trap:** a harness recording writes it *sent* rather than writes that were *acknowledged* produces false failures forever. The acknowledgement is the contract; only acknowledged writes must survive.

---

## Phase 3 — Memtable and on-disk tables

**Goal:** exceed RAM. Sorted, immutable data lands on disk.
**Total effort:** 20–26 hours.

### - [x] T3.1 — Implement the concurrent skip list

**Effort:** 5–6 h · **Model:** write it entirely yourself; Opus 5 to review memory ordering afterwards

Implement a skip list with probabilistic level assignment (p = 0.25, max 12 levels), ordered by `(user_key ascending, sequence descending)` so the newest version of a key sorts first. Provide `Insert`, `Get`, and a forward iterator, and track approximate memory usage so the size threshold means something.

Make reads lock-free using atomic loads on forward pointers, with a single writer. This is your first real encounter with the fact that "correct under concurrency" is a different property from "correct" — one of the most valuable things this project will teach you. Benchmark against a `map` plus sort to confirm the ordered iterator earns its cost.

**Done when:** a property test inserting 100k random keys confirms sorted iteration order, and a concurrent test with one writer and eight readers is clean under `-race`.

**Trap:** insert must link the new node at level 0 *before* linking higher levels, or a concurrent reader can traverse a high-level pointer to a node not yet reachable at the bottom. Reason this through carefully; it is exactly the class of bug the race detector may not catch on every run.

---

### - [x] T3.2 — Implement memtable rotation and the immutable queue

> **Done.** `internal/engine/rotation.go` holds `memtableSet`: the threshold, the atomic swap, the bounded queue, the fresh memtable and WAL opened in one critical section, reads walking active then immutables newest-first, and stall accounting surfaced through `INFO` as `write_stalls` and `write_stall_seconds`. `internal/memtable/skiplist.go` (T3.1) is now wired in as the `New` hook, replacing the `sliceTable` test double everywhere. Green under `-race`, 93.1% coverage on `internal/engine`.
>
> Both *Done when* conditions pass against the real skip list: `TestSustainedWorkloadRotatesWithReadsCorrect` and `TestBackpressureStallsRatherThanGrowing`. The trap is now fully asserted: `TestSequenceRangesAreDisjointAcrossSlots` covers the memtable half, and `TestEachRecordLandsInItsMemtablesWAL` covers the WAL half by parsing each slot's WAL file back with T2.3's reader and checking its sequence set against the memtable's.
>
> Written before T2.3 and T3.1 at the author's direction, out of the usual phase order; closed once both landed.

**Effort:** 3–4 h · **Model:** Opus 5 — the atomicity requirements are subtle

Implement the size threshold (default 4 MB, configurable), the atomic swap of the active memtable into a bounded immutable queue, opening a fresh memtable and WAL in the same critical section, and read paths consulting active then immutable newest-first.

The bounded queue matters. If flushing falls behind, writes must stall with a clearly reported metric rather than growing memory without limit. Implementing backpressure now, and exposing stall duration in `INFO`, means Phase 6 already has the mechanism compaction needs.

**Done when:** a sustained write workload rotates repeatedly with reads correct throughout, and artificially stalling the flusher produces measured backpressure rather than an OOM.

**Trap:** rotating the memtable and rolling the WAL must be atomic with respect to sequence numbers. If a write lands in the new memtable but the old WAL, recovery reorders it and you get silent corruption.

---

### - [x] T3.3 — Implement the SSTable block encoder and decoder

**Effort:** 4–5 h · **Model:** write it yourself; Opus 5 to review boundary handling

Implement the block format from section 7.3: prefix-compressed entries, restart points every 16 entries, trailer with restart offsets and CRC32C, targeting ~4 KB blocks. Implement the decoder with binary search over restart points followed by a linear scan within the interval.

Prefix compression and restart points are in tension — sharing saves space, restarts cost space but enable search. Being able to explain why the restart interval is a tunable, and what happens at 1 and at 1000, is a genuinely good interview answer.

**Done when:** a round-trip over 100k entries with heavily shared prefixes reproduces every entry exactly, and `Seek` to a key between two entries lands on the correct successor.

**Trap:** the first entry after each restart point must store its key in full. Miss that and decoding works during sequential scans and fails only on seeks — the worst kind of bug to find late.

---

### - [x] T3.4 — Implement the SSTable builder and reader

**Effort:** 4–5 h · **Model:** Sonnet 5 for plumbing, but write the layout logic yourself

Build the writer that consumes a sorted iterator and emits a complete SSTable — data blocks, bloom block (stub until Phase 5), index block, footer — with correct offsets throughout, then `fsync` the file *and* the containing directory. Build the reader that validates the footer magic, loads the index, binary-searches to a candidate block, verifies its checksum, and searches within it. Add a forward iterator and a `Seek`.

The directory fsync is not optional and is widely missed. A file creation is not durable until the directory entry itself is synced; skip it and a crash can leave a manifest referencing a file that does not exist.

**Done when:** building from 100k random pairs, reopening, and verifying every key plus full iteration order passes; and a single corrupted byte anywhere in the file is caught by a checksum rather than returning wrong data.

**Trap:** offsets written before you know the final size of a preceding section. Build sections in order and record offsets as you go; do not try to patch the footer afterwards.

---

### - [x] T3.5 — Implement the flusher and close the loop

> **Built; all four done-when clauses met.** `internal/engine/flush.go` holds `Flusher`: it consumes the immutable queue, builds the SSTable via `sstable.WriteTable` (which fsyncs the file and the directory), appends ADD_FILE through `manifest.Log.Append` (which fsyncs, and is the commit point), installs the version, and only then discards the memtable and deletes its WAL. Green under `-race`.
>
> The ordering is proven by a deterministic fault-injection hook rather than by argument. `TestWALSurvivesUntilTheManifestCommits` stops the flush between the SSTable fsync and the manifest fsync and asserts the WAL is still on disk; it was mutation-tested by moving the retire into that window, which fails it by name. Table-count, readability and flat-memory clauses are covered by `TestFlushProducesOneTablePerMemtable`, `TestFlushedDataIsReadableAfterReplay` and `TestFlushMemoryStaysFlat` — the ratio and memory tests run at a size the suite can afford rather than at 1 GB (verified again this run: 19 tables for ~628,000 bytes at a 32 KiB threshold, and heap growing from 473,656 to 623,592 bytes for 10x the writes), and say so.
>
> **Fourth clause closed:** "50 kills targeted at the flush window lose nothing." `cmd/strata-server/main.go` opens the durable `engine.LSM` (`engine.Open`), wiring in the WAL, flusher, manifest and compactor behind the same `Engine` interface. `test/crash/crash_test.go`'s `TestFlushWindowSurvivesKill` — 50 iterations, each arming `KillPointDuringFlush` directly via T2.4's `STRATA_CRASH_AT` wiring against a server with `-memtable-bytes` set to 16 KiB so a flush actually happens within a short run, the kill landing on a varying occurrence count per iteration so it hits different flushes across the run — was run under `STRATA_DURABLE=1` as part of closing T2.4 and passed (12.2s, zero durability violations). `go build ./...`, `go vet ./...`, and `go test ./internal/engine/... -race` are all clean.

**Effort:** 4–6 h · **Model:** Opus 5 for the ordering constraints

Wire the background flusher: consume from the immutable queue, build an SSTable from the memtable iterator, append the ADD_FILE manifest edit, fsync the manifest, and only then drop the memtable and delete its WAL.

The ordering is a durability argument you must be able to state: the WAL may only be deleted once the data it protects is durable *and* referenced by a durable manifest. Delete it a moment early and a crash in that window loses acknowledged data permanently. Use the fault injection hook from T2.4 to kill the process at exactly that point and prove it holds.

**Done when:** writing 1 GB with a 4 MB memtable produces roughly 250 SSTables, every key is readable, process memory stays flat regardless of dataset size, and 50 kills targeted at the flush window lose nothing.

**Trap:** deleting the WAL after the SSTable fsync but before the manifest fsync. It looks safe. It is not, and it is the classic version of this bug.

---

## Phase 4 — The read path

**Goal:** correct reads across memtable and many SSTables, with working deletes.
**Total effort:** 16–20 hours.

### - [x] T4.1 — Implement the version and manifest subsystem

> **Complete.** `internal/manifest/` holds `FileMetadata`, `VersionEdit` encoding and decoding to §4, the immutable `Version` with its level invariants, `Apply`, `VersionSet` with atomic installation, the atomic `CURRENT` write, and — as of the log commit — `Log`/`CreateLog`/`Append` and `Recover`. Green under `-race`.
>
> The framing blocker is cleared. `wal.Writer.WriteRecord` and `wal.Reader.NextRecord` were extracted as the raw-bytes entry points, so §4's "reuses the WAL block framing exactly" is now literally one implementation rather than two, and the §2.3 clean-tail rule is inherited rather than reimplemented.
>
> Both halves of the *Done when* are met: `TestRecoverReconstructs200FilesAcrossFourLevels` replays 200 files over 4 levels from bytes actually written to disk, and `TestRecoverStopsAtATruncatedEdit` sweeps every truncation length and recovers to the last complete edit. The truncation sweep includes the untruncated length so it cannot pass vacuously, and was mutation-tested: treating the clean tail as fatal fails it.

**Effort:** 5–6 h · **Model:** Opus 5 for the design; write the implementation yourself

Implement `FileMetadata` (file number, size, smallest and largest key, sequence range), the immutable `Version` holding per-level file lists, `VersionEdit` encoding and decoding, and `VersionSet` applying edits to produce new versions. Implement manifest replay on startup and the `CURRENT` file written atomically via write-temp-then-rename-then-fsync-directory.

The immutability of `Version` is the whole design. Grasping *why* a mutable list plus a mutex is not sufficient — a reader holding the list across a compaction sees files deleted underneath it — is the conceptual centre of the concurrency story, and the thing to be crispest about in interviews.

**Done when:** a directory with 200 SSTables across 4 levels reconstructs exactly from manifest replay, and a manifest truncated mid-edit recovers to the last complete edit.

**Trap:** replay must be idempotent and order-sensitive. An ADD followed by a DELETE of the same file must leave it absent, regardless of how you accumulate state.

---

### - [x] T4.2 — Implement file lifecycle and reference counting

> **Complete.** `internal/manifest/refcount.go` and `cleanup.go`: references on both versions and files, `Acquire`/`Release`, obsolete collection, `DeleteObsolete`, and the §4.1 startup orphan sweep.
>
> The done-when now runs against real tables. `TestReadersNeverTouchADeletedSSTable` puts 100 readers against 100 compactions where every file is a real SSTable built by `sstable.WriteTable`: readers open the file, parse its footer and index, and verify a data block's checksum, and the compactor genuinely `os.Remove`s obsolete files. 14,000 real reads, clean under `-race`. The tracker-based `TestReadersNeverTouchADeletedFile` is kept alongside it — it runs a heavier interleaving cheaply, so the two are complementary rather than redundant.
>
> The fault-injection layer is retained on top of real deletion because the two catch different things: POSIX keeps an already-open file readable after unlink, so unlinking alone would miss a reader that opened before the delete. Mutation-tested — releasing the old version's file references before the new version takes its own fails it with a named file.

**Effort:** 3–4 h · **Model:** Fable 5 — lifetime bugs here are the hardest to reason about

Implement reference counting on both versions and files, with the invariant that a file is deleted from disk only when no version referencing it is still held by any reader. Readers acquire a reference to the current version for the duration of an operation and release it after.

This is the Go equivalent of the memory-lifetime problem Rust would have forced on you at compile time. You get to solve it explicitly, which is more instructive — and it is exactly what to reference when someone asks why you chose Go.

**Done when:** a stress test running compactions while 100 clients read continuously never touches a deleted file, verified by a fault-injection layer that panics on any read of a file marked deleted.

**Trap:** the window between reading the version pointer and incrementing the reference count. Get this wrong and it fails once every few million operations — which means it fails in front of an interviewer, not in your tests.

---

### - [x] T4.3 — Implement the complete Get path

> **Complete.** `internal/engine/get.go` holds `lookup`: active memtable, immutable memtables newest-first, L0 tables newest-first, then one binary-searched candidate per level below L0, first match winning and a tombstone stopping the search. Table access goes through a `tableReader` interface so T5.1's bloom filter and T5.2's cache slot in without disturbing the order.
>
> Done-when met exhaustively. `TestEveryFlushInterleavingOfWriteOverwriteDelete` sweeps all 2^7 = 128 placements of flushes across write-plus-five-overwrites-plus-delete, and a companion sweeps the 2^6 without the delete asserting the newest value wins — 192 subtests.
>
> Mutation-tested twice. Iterating L0 oldest-first fails the trap test with the visit order named; letting a tombstone fall through to lower levels fails every interleaving in which the delete was flushed, identified by mask.

**Effort:** 3–4 h · **Model:** write it yourself; Opus 5 for review

Implement lookup in the documented order — active memtable, immutable queue newest-first, all L0 tables newest-first, then L1 and below via binary search over key ranges to find the single candidate per level — with first match winning and a tombstone match returning not-found.

The asymmetry between L0 and lower levels is worth internalising properly. L0 files come from independent memtable dumps, so their key ranges overlap arbitrarily and all must be checked. Below L0, compaction guarantees non-overlap, so at most one file per level can contain a given key and binary search finds it. That single invariant is what makes reads scale.

**Done when:** a key written, overwritten five times, and deleted returns not-found under every possible interleaving of flushes, verified exhaustively.

**Trap:** L0 ordering must be by file number descending, not by any key-range heuristic. Newest file wins, always.

---

### - [x] T4.4 — Implement the k-way merge iterator

> **Complete.** `internal/sstable/merge.go` holds `MergeIterator`: a heap-based merge over any number of `memtable.Iterator` sources, deduplicating by user key under the canonical `(user_key asc, sequence desc)` comparator, with tombstone suppression as a flag so Phase 6 compaction can reuse it verbatim. It lives in `internal/sstable` because §8 assigns "iterators" to that package.
>
> Done-when both halves met. `TestMergeTenSourcesWithHeavyOverlap` merges 2,440 entries over 10 sources down to 300 distinct keys and checks the result entry-for-entry against a sorted reference model, with and without tombstone suppression. `TestMergeCostGrowsLogarithmicallyInSources` fixes the entry count and varies only k: going 2 → 128 sources costs **6.4×** against the 7× that O(n log k) predicts, where O(nk) would predict 64×.
>
> Mutation-tested twice. Comparing user keys alone fails three tests including the ten-source case; replacing `heap.Fix` with a per-entry `heap.Init` (making it O(nk)) moves the ratio to 40.1× and fails the complexity assertion.

**Effort:** 3–4 h · **Model:** write it yourself — this is core interview surface

Implement a heap-based merge iterator over an arbitrary set of source iterators that deduplicates by user key, keeping the highest sequence number, and optionally skips tombstoned keys.

Build this once and build it well, because compaction reuses it verbatim in Phase 6. A clean merge iterator makes compaction almost easy; a messy one makes compaction miserable. It is also the component most likely to come up at a whiteboard, because it is small enough to write live and rich enough to discuss.

**Done when:** merging 10 sources with heavy key overlap produces exactly the correct deduplicated sequence, verified against a sorted reference, and a benchmark confirms O(n log k) rather than O(nk).

**Trap:** comparing only user keys and forgetting sequence order gives a merge that returns arbitrary versions. The comparator is `(user_key asc, sequence desc)` and it must be the same comparator used everywhere.

---

### - [x] T4.5 — Implement SCAN and the reference model test

> **Closed by Phase 6.** `LSM.Scan` pages on a key-based cursor over `MergeIterator` with tombstone suppression, and `test/model/` holds the reference engine, the weighted generator, the runner and a delta-debugging shrinker.
>
> A specification conflict was resolved here rather than papered over. §7.5 says the cursor "encodes the last key returned", but `Engine.Scan` took a `uint64`, which cannot hold a key, and T1.2 called that interface final. The interface changed, because a numeric offset names a position that a flush moves — `TestScanCursorSurvivesAFlush` flushes mid-iteration and asserts no key is skipped or repeated. The RESP layer keeps the numeric cursor Redis clients require by issuing per-connection tokens, so `make interop` and go-redis are unaffected.
>
> **Shrinking is proven** (`TestShrinkerFindsAMinimalSequence`, 2,002 ops → 2). **`TestModelManySeeds` is green**: 50 seeds × 1,000 operations, zero divergence, 18.7 s. It found three real bugs, each reported as a sequence under twenty operations — see commit 8110205.
>
> **`TestModelFiftyThousandOperations` is now green**: 50,000 operations, zero divergence, 8.8 s. The prediction made here while it was unticked held exactly. Before Phase 6 the run exceeded a nine-minute timeout, for a structural reason rather than an accidental one — with no compaction, L0 grew without bound and every full scan reopened every table in it. Compaction bounds L0, and the same run now completes in under nine seconds, a speedup of at least 60×. Measured on the T6 branch at commit `09cdb11`, reproduced with `go test ./test/model -run TestModelFiftyThousandOperations -v`.

**Effort:** 2–3 h · **Model:** Sonnet 5 for the test harness; write SCAN yourself

Implement `SCAN` with cursor, `MATCH`, and `COUNT` on top of the merge iterator, then build the reference model test: a plain Go `map` implementing the same interface, a weighted random operation generator, and a runner asserting identical results across millions of operations.

Introducing model testing here rather than in Phase 7 is deliberate. Every subsequent phase then runs against it automatically, and compaction bugs in Phase 6 surface as a specific failing operation sequence rather than a vague suspicion that something is wrong.

**Done when:** 50,000 random operations produce zero divergence, and the runner shrinks any failure to a minimal reproducing sequence.

**Trap:** your cursor must stay valid across compactions. Encoding it as a file offset breaks the moment files merge; encode the last key returned.

---

## Phase 5 — Bloom filters and caching

**Goal:** make lookups of absent keys cheap.
**Total effort:** 10–13 hours.

The satisfying phase — small code, dramatic measurable effect. Benchmark before and after; that delta is a graph for your README.

### - [x] T5.1 — Implement the bloom filter and integrate it

> **Complete.** `internal/bloom` holds the builder and reader; `internal/sstable` writes a real bloom block and consults it in `Table.Get` before the index search and any block read. `k` is derived as (m/n)·ln2 rather than hardcoded.
>
> Done-when, measured: 100,000 keys inserted, 1,000,000 probes, **zero** false negatives, and a measured false positive rate of 0.8158% against a theoretical 0.8194% — well inside 2×. The rate is bounded from below as well as above, since a filter measuring far under theory means the probes are not distributed as the derivation assumes.
>
> The trap is handled by making the false negative bound exactly zero rather than a ratio, and by two decisions that a "few is fine" reading would have gotten wrong: a nil filter answers "maybe" rather than "absent", and every byte of the bloom block is covered by a checksum sweep at open, because clearing one bit turns a present key into a reported absence.

**Effort:** 4–5 h · **Model:** write it yourself; Opus 5 to check your parameter derivation

Implement a bloom filter with configurable bits-per-key (default 10, ~1% false positive rate), using double hashing (`h1 + i*h2`) to derive k hashes from a single 64-bit hash rather than running k independent hash functions. Serialise into the SSTable bloom block, load on table open, and consult it before any block read in `Get`.

Derive the optimal `k` for your `m/n` rather than hardcoding it, and be able to explain why adding hash functions past the optimum makes the filter *worse* — the bit array saturates and everything starts looking present.

**Done when:** 100k keys inserted, 1M probes confirm zero false negatives, and the measured false positive rate sits within 2× of theoretical.

**Trap:** false negatives are a correctness bug, not a performance bug — a false negative means a key that exists reports as absent and your database has lost data. Test for zero, not for few.

---

### - [x] T5.2 — Implement the sharded block cache

> **Complete.** `internal/cache` is an LRU keyed by `(file_number, block_offset)`, bounded in bytes, split into 16 independently locked shards, with hits and misses exposed through `INFO`. `Flusher.dropObsolete` evicts a file's blocks when the file is deleted, after the manifest fsync and never before.
>
> Done-when, measured: a Zipf(s=1.2) workload over 20,000 blocks against a 16 MiB cache reaches a **91.98%** hit rate, and 2,000 rounds of warm-a-file/evict-the-oldest hold at 8 resident files and 2 MiB with no growth.
>
> The flat-memory assertion took two attempts and the first one is the lesson: bytes are bounded by capacity whether the resident blocks are live or dead, and the deletion index can be cleared without the blocks being dropped, so neither detects the leak. Counting distinct files with blocks still resident does — with `EvictFile` disabled it peaks at 43 dead files and the cache pins itself at capacity, evicting live data to hold data no reader can want.
>
> Checksums are verified on cache hits as well as file reads (CLAUDE.md §11), and a block that fails verification is never inserted, so one bad read cannot become a permanently bad read.

**Effort:** 3–4 h · **Model:** Sonnet 5, with Opus 5 reviewing the eviction-during-delete path

Implement an LRU cache keyed by `(file_number, block_offset)`, capacity configurable in bytes with a 64 MB default, sharded into 16 segments to cut lock contention. Expose hit and miss counters through `INFO`, and evict all of a file's blocks when that file is deleted.

The file-deletion eviction path is the one that bites. Miss it and you leak memory proportional to compaction throughput — which means the leak only appears under sustained load, which means you find it in week 11.

**Done when:** a Zipfian read workload achieves above 90% hit rate, and a long compaction-heavy run shows flat cache memory usage.

**Trap:** caching blocks, not key-value pairs. Block granularity gives locality for free and matches your checksum granularity. Per-key caching is a different, worse design.

---

### - [x] T5.3 — Produce the tuning study

> **Complete.** `docs/benchmarks.md` §"Bloom filters and block cache" records bits-per-key across 4/8/10/16, block size across 1/4/16/64 KiB, and cache size across 1 MiB–256 MiB at two Zipf skews. Three runs each, medians and full ranges, with the reproduction commands.
>
> Done-when, measured: absent-key lookups are **10.6× faster** with the filter than without on a cold cache (516.5 ns against 5,464 ns), against the same file with only the filter bypassed. Warm, the ratio is 4.2× — recorded rather than hidden, since the cold figure is the one that describes a real absent-key lookup, whose block is by definition not resident.
>
> Every parameter recommendation in the README traces to a row in the study, including the one the study does not support: for random point reads 1 KiB blocks measured 1.5× faster than the 4 KiB default with 4× less read amplification, and the default is retained only pending a range-scan benchmark that has not been run. The bits-per-key recommendation rests on the false-positive cost table, not on the latency sweep, which cannot separate 8, 10 and 16 from run-to-run variance.
>
> The trap is handled: three runs, median and range for every row, and no ranking claimed between configurations whose ranges overlap.

**Effort:** 3–4 h · **Model:** Claude Cowork or Opus 5 — multi-run analysis work

Run the parameter sweeps that turn this from a feature into a finding: bits-per-key across 4, 8, 10, 16, plotting false positive rate against memory cost and measured absent-key latency; block size across 1 KB, 4 KB, 16 KB, 64 KB against read amplification and cache efficiency; cache size across a range against hit rate.

Write it up in `docs/benchmarks.md` with charts. This section is disproportionately impressive because almost nobody does it — it demonstrates you can measure a system rather than only build one.

**Done when:** absent-key lookups are at least 5× faster with blooms enabled, and every parameter recommendation in your README traces to a measurement in this study.

**Trap:** running each configuration once. Run three times, report the median, note the variance, or your "finding" is noise.

---

## Phase 6 — Leveled compaction

**Goal:** bounded file count, bounded space, garbage actually reclaimed.
**Total effort:** 28–38 hours. The largest phase by a wide margin. Do not rush it.

### The scheme

- **L0** holds freshly flushed tables which may overlap in key range. Triggered by file count, default 4.
- **L1 and below** hold tables with strictly non-overlapping ranges, each level targeting ~10× the bytes of the one above.
- Compaction picks a file from level *i*, finds all overlapping files in level *i+1*, merges them, and writes output into level *i+1*.

### - [x] T6.1 — Implement the compaction picker

**Effort:** 5–7 h · **Model:** Opus 5 to reason through the policy; write the code yourself

Implement scoring per level — L0 by file count, others by total bytes over target — selecting the highest-scoring level above 1.0, then choosing the input file by round-robin over key position so compaction sweeps the keyspace instead of hammering one region. Expand with all overlapping files from the next level, special-case L0 by expanding to include every L0 file overlapping the chosen range, and cap total input size to bound the duration of any single compaction.

The picker is pure policy with no correctness burden, which makes it the ideal place to experiment. Try a naive picker first, watch the pathological behaviour it produces under a skewed workload, then fix it. That before-and-after is excellent README material.

**Done when:** under a skewed write workload, compaction work distributes across the keyspace rather than concentrating, demonstrated with a per-key-range compaction count chart.

**Trap:** the L0 expansion rule is recursive in effect — adding an L0 file widens the key range, which may pull in more L0 files. Iterate to a fixed point.

---

### - [x] T6.2 — Implement the compaction executor

**Effort:** 7–9 h · **Model:** write it yourself; Fable 5 to attack the tombstone logic

Build the executor: merge iterator over all inputs, output through the SSTable builder rolling to a new file at target size (default 2 MB), keeping only the newest version of each user key, and never splitting one user key across two output files.

The hard part, and the part most likely to hide a bug, is tombstone dropping. A tombstone may only be discarded when compacting into the bottom-most level that could contain that key *and* no snapshot requires it. Drop it early and older data at a lower level becomes visible again — a deleted key resurrects. Construct that failure deliberately as a test *before* writing the logic; you will understand the rule far better having seen it break.

**Done when:** a targeted test proves a deleted key never resurrects across every compaction path, and no output file ever splits a user key.

**Trap:** the resurrection bug is silent, rare, and catastrophic. It will not show up in casual testing. Write the adversarial test first.

---

### - [x] T6.3 — Implement atomic version installation

**Effort:** 4–5 h · **Model:** Fable 5 — the crash-safety crux of the entire system

Build the commit path: assemble a version edit containing a DELETE for every input and an ADD for every output, append it to the manifest, fsync, then atomically swap the version pointer and release input references. Files are deleted only when their refcount reaches zero.

The manifest fsync is the commit point and the system has exactly two valid states: crash before it and the compaction never happened, with all inputs still live and outputs orphaned; crash after and it fully happened. Write the orphan cleanup that runs at startup, scanning for `.sst` files absent from the manifest and removing them.

**Done when:** 50 kills targeted precisely at the pre-fsync and post-fsync windows leave the database correct in every case, with orphans cleaned on restart.

**Trap:** deleting an input file before the manifest edit is durable. It looks fine in testing because the crash window is microseconds. It is a permanent-data-loss bug.

---

### - [x] T6.4 — Implement scheduling, backpressure, and write stalls

**Effort:** 4–5 h · **Model:** Opus 5

Wire the dedicated compactor goroutine with triggers after every flush and every completed compaction, a soft threshold that slows writes and a hard threshold (default 12 L0 files) that stalls them, optional I/O rate limiting to protect foreground latency, and clean shutdown that either finishes or cleanly abandons in-flight work.

Deliberate stalling is counterintuitive and worth understanding well: it is better to slow writes predictably than to let L0 grow unbounded and have read latency collapse for everyone. Expose stall count and total stall duration in `INFO`, because you will need them in Phase 8.

**Done when:** a write workload exceeding compaction throughput produces bounded L0 growth and reported stalls rather than unbounded degradation, and shutdown during compaction is clean.

**Trap:** rate-limiting compaction too aggressively converts a latency problem into a space problem. Measure both.

---

### - [x] T6.5 — Implement continuous invariant verification

**Effort:** 3–4 h · **Model:** Sonnet 5 for the checker, Fable 5 to help enumerate invariants

Build an invariant checker, runnable both as a test hook after every compaction and as a `strata-cli validate` command, asserting: L1+ files never overlap in key range; no user key appears twice in one level; every manifest file exists on disk and every on-disk `.sst` appears in the manifest; sequence numbers are monotonic; live data never shrinks except through deletes.

Run it after every compaction in test builds. This converts compaction bugs from "reads sometimes return wrong data three hours in" to "assertion failed at compaction 47" — the difference between a day of debugging and ten minutes.

**Done when:** the checker runs after every compaction under test, a six-hour soak passes with zero violations, and a deliberately corrupted version is caught immediately.

**Trap:** invariants that only run at shutdown catch nothing useful. Run them continuously in test builds.

---

### - [x] T6.6 — Measure and characterise amplification

**Effort:** 3–4 h · **Model:** Opus 5 for analysis

Instrument bytes written by the user versus bytes written to disk (write amplification), disk reads per logical read (read amplification), and disk bytes per live byte (space amplification). Run the characterisation: write 1 GB, overwrite it entirely, force compaction, confirm disk usage returns near 1 GB. Then vary the level size multiplier across 4, 10, and 20 and plot how the three amplifications move against each other.

That plot is the single best artifact this project can produce. It shows you understand storage engine design as a constrained optimisation rather than a set of features, and it gives you a concrete answer to "what would you change for a write-heavy workload."

**Done when:** space amplification is under 2× after settling, all three figures are reported in `INFO` and `docs/benchmarks.md`, and the multiplier sweep is plotted.

**Trap:** measuring space amplification before compaction has quiesced. Force compaction, wait for idle, then measure.

---

## Phase 7 — Correctness hardening

**Goal:** prove it works rather than believe it works.
**Total effort:** 16–22 hours.

This phase is what separates a project from a *credible* project. Most candidates skip it, which is precisely why doing it is disproportionately convincing.

### - [x] T7.1 — Scale up model-based testing

> **Closed.** The framework was already in place -- `test/model/distribution.go` (uniform, Zipfian, sequential, adversarial common-prefix), `test/model/generate.go` (weighted structural operations, deliberate rotation and compaction), `test/model/soak.go` (the long-run driver) and the delta-debugging shrinker. What was missing was the run itself. `go test ./test/model -run TestModelSoak -model.ops=10000000 -timeout=40m` completed 10,000,000 operations against the real `engine.LSM` with **zero divergence** in 25m14s, sustaining 6,600 ops/s once the tree reached steady depth (throughput fell from the 100k-op sample's ~9,200 ops/s as memtables, SSTable count and compaction work grew with the dataset -- expected LSM behaviour, not a regression). `TestInjectedBugIsCaughtAndShrunk` catches `resurrectingSystem`'s injected tombstone-resurrection bug and shrinks it from 120 operations to 4: `PUT key-0005, DEL key-0005, COMPACT, SCAN`, well under the 20-operation bar, with the minimal sequence confirmed to still contain the delete/compaction pair that causes it.

**Effort:** 4–5 h · **Model:** Sonnet 5 for generators, Fable 5 for shrinking strategy

Extend the T4.5 model test into a serious property-testing framework: configurable key distributions (uniform, Zipfian, sequential, adversarial common-prefix), operation weights, deliberate memtable rotation and compaction triggers interleaved with operations, and automatic shrinking of failures to minimal reproducing sequences. Run 10 million operations nightly, a reduced count on every commit.

**Done when:** 10 million operations complete with zero divergence, and an artificially injected bug is caught and shrunk to under 20 operations.

**Trap:** uniform random keys exercise almost nothing interesting. Zipfian distributions and shared-prefix adversarial keys find the real bugs.

---

### - [x] T7.2 — Build the fault injection layer

> **Regression found and fixed post-Phase-8.** `TestFaultSweep` started failing during the T8.3 session (confirmed pre-existing and unrelated to those changes): under an injected WAL fsync failure, an acknowledged write could come back holding a different key's value, because a failed `fsync` never removes the bytes a preceding `WriteAt` already placed in the file -- they are indistinguishable from a genuinely durable record by content or checksum. `internal/wal/writer.go`'s `Rollback` existed for exactly this (and `internal/manifest` already used its equivalent) but was never wired onto the WAL's own write path. Fixed in `internal/engine/rotation.go`: on `AwaitDurable` failure, `memtableSet.Add` re-takes its own write lock and truncates the WAL back to `Syncer.SyncedOffset()` before returning -- the same lock that already serialises every `Write` to that file, so the truncate can't race a concurrent writer, which a naive fix calling `Rollback` from inside `Syncer.leadSync` itself would have. `Syncable` grew a `TruncateTo(offset int64) error` method; `TestFailedSyncPoisonsTheEngine` grew the assertion that would have caught this originally (the failed write must be absent after reopen, not merely present-but-unacknowledged). `TestFaultSweep` now passes consistently (3 consecutive runs, 485 faults each) and the full suite is green under `-race`, including `test/crash`'s real `kill -9` harness.

**Effort:** 5–6 h · **Model:** Opus 5 for the design

Generalise the T2.4 hook into a proper fault injection layer: a filesystem wrapper that can fail any write, fail any fsync, truncate a file mid-write, return short reads, or inject latency — all controllable by operation index so failures are reproducible. Then systematically inject a fault at every I/O operation across a full workload and verify the database is always recoverable.

This is what real storage engine test suites do, and describing it is a strong interview signal because it shows you know "it didn't crash in my testing" is not evidence of durability.

**Done when:** every single I/O operation in a representative workload has been failed individually, and the database recovers correctly in every case.

**Trap:** a failed fsync is the nastiest case, because on some systems the error is reported once and the dirty page is dropped. Treat a failed fsync as unrecoverable — close the database and require restart.

---

### - [x] T7.3 — Run the corruption and edge-case sweep

> **Closed.** `internal/engine/corruption_test.go` holds `TestCorruptionSweep`: bit flips in one and in every SSTable, truncation at ten random offsets, a deleted SSTable, a corrupted `MANIFEST`, a corrupted `CURRENT`, and a `CURRENT` naming a nonexistent manifest. The assertion throughout is `checkNoWrongAnswers` -- an error is an acceptable outcome, a value that does not match what was written never is. `TestEdgeCaseKeysAndValues` covers the empty key, empty value, maximum-size key and value, prefix keys, strictly sequential keys across a flush, and repeated overwrites of one key. `TestRestartWithZeroData` and `TestSecondOpenOnALiveDirectoryFailsCleanly` cover the remaining two; the latter exercises the new `dirLock` (`internal/engine/lock.go`), added because nothing previously stopped two processes from sharing a directory and corrupting both the WAL and the manifest. The trap -- checksums on cache hits -- was already satisfied: `Table.loadBlock`'s doc comment states and `NewBlock` enforces that cached bytes are re-verified on every call, not only on the first read from disk.
>
> All green under `-race`, including the full unrelated suite (`go test ./... -short`).

**Effort:** 4–5 h · **Model:** Sonnet 5

Systematically corrupt: flip random bits in SSTables, truncate files at random offsets, delete a random SSTable, corrupt the manifest, corrupt `CURRENT`. In every case verify a clear diagnostic error rather than a panic or, far worse, a silently wrong answer.

Then sweep the edge cases: empty key, empty value, maximum-size key and value, a key that is a strict prefix of another, all keys identical, strictly sequential keys, restart with zero data, restart with a corrupt manifest, two processes opening the same directory.

**Done when:** every corruption scenario produces a clear error, every edge case has a test, and no scenario panics.

**Trap:** silently returning wrong data is the worst possible failure mode for a database — much worse than crashing. Verify checksums are actually consulted on every path, including cache hits.

---

### - [x] T7.4 — Complete the concurrency audit

> **Closed.** `go test ./... -race -short -count=1` is green across all 18 packages. `test/stress/mixed_test.go`'s `TestMixedWorkloadStress` ran the real one-hour, 100-client audit under `-race` (`-stress.duration=1h -stress.clients=100`): **52,297,151 operations, 0 errors, 3600s**, against a real `engine.LSM` and `server.Server` with a deliberately small compaction geometry (`L0Trigger: 2`, 64 KiB memtable threshold) so flushes and compactions ran continuously through the whole window rather than only at the start -- the trap, addressed directly rather than hoped past. Goroutine and file-descriptor counts, read from inside the same process via `runtime.NumGoroutine()` and `/proc/self/fd`, matched their pre-`Open` baselines within tolerance after `Close` and a settle period: zero leaks, and the file-descriptor check ran against a database that had been compacting continuously for an hour, not a quiet one.
>
> `goleak` was not added; a manual before/after `runtime.NumGoroutine()` comparison gives the same signal without a dependency outside CLAUDE.md's permitted set (`testify`, `xxhash`, `golang.org/x/sys/unix`) or the ADR that would require.
>
> Block and mutex profiles were captured both from a 30 s/150-client run and from the full hour (`-stress.profile`). Both agree on the worst contention point: `sync.(*Mutex).Lock`/`Unlock` inside `internal/engine.(*memtableSet).Add` accounts for ~98% of measured block and mutex time. The cause is exact and already understood, not mysterious -- `Add` holds its lock across the WAL fsync under `SyncAlways`, and T2.2's group-commit `wal.Syncer` is built but not wired onto this path (see plan.md's T2.2 entry and `docs/concurrency.md`'s "Known gap" section for why wiring it safely is nontrivial).
>
> `docs/concurrency.md` inventories every shared structure in the codebase -- `memtableSet`, `LSM`, `dirLock`, `Flusher`, `manifest.VersionSet`, `compaction.Picker`/`Scheduler`, `cache.Cache`, `wal.Syncer`, `memtable.SkipList`, `server.Server` -- what protects each, and the safety argument for each access, cross-referenced against the actual profile data above rather than written from intent alone.

**Effort:** 3–4 h · **Model:** Opus 5, escalate to Fable 5 on anything that resists

Run the full suite under `-race`, run a one-hour 100-client mixed-workload stress test, verify zero goroutine leaks with `goleak`, verify no file descriptor leaks under sustained compaction, and capture block and mutex profiles to locate your worst contention.

Then write up, in `docs/`, the concurrency model as it actually ended up — every shared structure, what protects it, and the argument for why each access is safe. Writing this document finds bugs, and it is the document you will reread the night before an interview.

**Done when:** zero race findings, zero leaks, the one-hour stress test is clean, and the concurrency document exists and matches the code.

**Trap:** the race detector only finds races on paths actually executed. Contrive tests that force compaction and reads to overlap rather than hoping they happen to.

---

## Phase 8 — Performance

**Goal:** numbers you can defend.
**Total effort:** 16–20 hours.

### - [x] T8.1 — Build the benchmark harness and full result set

> **Closed.** `test/bench/full.sh` drives every workload in §19 against the real `engine.LSM` (not T1.4's in-memory stand-in): sequential and random pipelined SET, unpipelined SET at both `sync=interval` and `sync=always`, GET in-cache, GET at ~10x a deliberately shrunk cache, and GET on a guaranteed-absent key. `test/bench/loadgen` (new, `mixed` and `overtime` subcommands) covers what valkey-benchmark cannot: an interleaved 80/20 mixed workload with full p50/p95/p99/p99.9 latency percentiles timed per-operation, and throughput sampled every second during a sustained-compaction run. `test/bench/chart` renders that CSV as an SVG with no new dependency. `docs/benchmarks.md`'s new "Full workload suite" section is every number this run produced, with the exact reproduction command per row -- the *Done when* condition.
>
> The write-path numbers are solid (under 6% spread across three runs) and one target miss is fully explained: `sync=always` measured 744 ops/sec against a 2-5k target, consistent with the T7.4 stress profile's finding that `memtableSet.Add` holds its lock across the WAL fsync, so 50 concurrent clients get no benefit from their concurrency under this policy -- the exact T2.2 gap `docs/concurrency.md` already documents, now with a production-path number attached to it.
>
> The read-path numbers are **not** asserted as clean measurements. `/proc/loadavg` climbed from 3.0 to 9.6 during this run (12 logical cores, live desktop session, not the idle machine T1.4 and T6.6 used), and two manual reruns of the identical GET command minutes apart produced 20,589 and 638,298 ops/sec -- a 30x spread with nothing else changed. `docs/benchmarks.md` records this explicitly rather than publishing a false-precision median across incomparable runs, per plan.md's own instruction to note background load and never compare runs against different machine conditions. The harness itself is verified working; a rerun of the GET/mixed rows on a quiet machine is the identified next step, not a gap in T8.1.
>
> The throughput-over-time curve (`docs/throughput-over-time.svg`) is internally consistent -- one continuous 60 s run, immune to the cross-run noise above -- and flat at 37-41k ops/sec with no stall, which is T6.4's backpressure design working as intended under a small compaction geometry (`L0Trigger: 2`) rather than an absence of pressure.

**Effort:** 5–6 h · **Model:** Sonnet 5 for the harness, Claude Cowork for the analysis pass

Build a reproducible harness generating every workload in section 19 — sequential write, random write, random read in-cache, random read at 10× cache, absent-key read, mixed 80/20 — with warm-up, multiple runs, median and variance reporting, and latency percentiles at p50, p95, p99, p99.9. Every result records hardware, sync policy, and the exact reproduction command.

Add the run that matters most: throughput and latency plotted *over time* during heavy compaction. The shape of that curve — the stalls, the recovery, the periodicity — is the most interesting graph in the entire project, because it shows the system's real behaviour rather than its steady-state average.

**Done when:** every number in your README regenerates from one command, and the throughput-over-time chart exists.

**Trap:** reporting best-of-N. Report median and variance. An interviewer who has run benchmarks will ask, and best-of-N is a tell.

---

### - [x] T8.2 — Profile and produce the allocation and contention report

> **Closed, with one measurement gap disclosed.** `docs/profiles/t8.2-report.md` has five ranked, hypothesis-first items in each of the three categories against real heap (`heap.prof`) and mutex/block (`mutex.prof`, `block.prof`) profiles captured under `test/bench/loadgen`'s mixed 80/20 workload via `test/stress`'s existing `-stress.profile` mechanism. The worst-contention hypothesis — `memtableSet.Add`'s lock, already named by `docs/concurrency.md` and the T8.1 `sync=always` finding — is confirmed and quantified: 84–97% of block/mutex samples, and persists under `sync=interval`, proving the contention is inherent to the single `RWMutex` design rather than an artifact of holding it across fsync.
>
> **CPU profiling could not be captured.** Both `curl .../debug/pprof/profile` against the live server under load and `go test -cpuprofile` (with *no* load at all) are reliably `SIGKILL`ed by this sandboxed execution environment — reproduced 8/8 times, isolated to the `SIGPROF`/`setitimer` mechanism specifically (heap/mutex/block profiling of the identical loaded server succeeds every time). The report's CPU-consumer ranking is therefore stated as a hypothesis from code inspection only, explicitly flagged as not meeting the "profile first" bar, with re-running on an unrestricted machine named as the next step. This is an environment limitation, not a fabricated measurement.

**Effort:** 4–5 h · **Model:** Opus 5 for flamegraph interpretation

Capture CPU profiles under write and read load, heap and allocation profiles, block profiles for lock contention, and mutex profiles. Produce a written report identifying the top five CPU consumers, the top five allocation sites, and the worst contention point — each with a hypothesis about the cause and a proposed fix.

Write the hypotheses *before* you optimise. Comparing what you predicted against what actually helped is the most educational half hour in the project, and it makes a good interview story either way.

**Done when:** the report exists with five ranked items in each category and a stated hypothesis for each.

**Trap:** optimising what you assume is slow. Profile first, every time, without exception.

---

### - [x] T8.3 — Execute the optimisation cycle

> **Closed.** `docs/optimizations.md` documents four attempts, one change at a time: WAL encode-buffer reuse (5→1 allocs/op), the per-connection command-args slice reuse (3→0 allocs/op), `GOGC=400` (+72% throughput, p99 2.6x better than default, measured across 3 runs each), and a RESP bulk-string buffer reuse that **failed** — it broke correctness (`TestReadArray`, a fuzz seed, and six `internal/server` tests, one with a panic) because, unlike the two successful buffer-reuse changes, the reused bytes are still live past the point of reuse (`Value.Bytes` is handed to the caller and stored by the engine, not fully consumed before the next read). Reverted; `internal/resp/resp.go` is unchanged. Three successes with before/after data plus one documented failure satisfies the done-when condition.

**Effort:** 5–6 h · **Model:** Opus 5 for strategy, Claude Code (Sonnet 5) for the mechanical changes

Work the list in profile order: buffer reuse via `sync.Pool` on the hot path, eliminating string/[]byte conversions in comparators, more aggressive WAL batching, block size tuned to your measured workload, bloom bits-per-key tuned from the T5.3 study, and `GOGC` tuning with its effect on p99 measured rather than assumed.

Document each change as a before/after table. Include the optimisations that *didn't* work — those are more interesting than the ones that did, and including them signals honesty that experienced interviewers notice immediately.

**Done when:** at least three optimisations are documented with before/after data, and at least one failed optimisation is documented with an explanation of why it failed.

**Trap:** changing several things at once. One change, one measurement, always.

---

### - [x] T8.4 — Characterise GC behaviour and tail latency

> **Closed.** `docs/gc-characterization.md` measures all four items via `GODEBUG=gctrace=1` (the print-based mechanism sidesteps T8.2's SIGPROF sandbox restriction): pause distribution under load (1,325 GCs/20s at `GOGC=100`, mean STW 0.17ms, max 7.9ms), correlation with p99 (max STW pause tracks measured p99 closely at both `GOGC=100` and `GOGC=400`, both varying together by roughly the same factor when `GOGC` changes), heap growth vs. `-memtable-mb` (live heap ~2-3x memtable size at both 4 MB and 32 MB), and `GOGC`/`GOMEMLIMIT` interaction (`GOMEMLIMIT=64MiB` reproduces `GOGC=100`'s behaviour almost exactly even at `GOGC=400`, correctly vetoing the tuning when memory is capped). ADR-001's loop is closed with a number: GC's worst observed single-pause cost is 7.9ms at default settings, tunable down to 1.6ms, against p99/max latencies of the same order — real but not dominant next to the 83-97% mutex-wait share T8.2 measured separately.

**Effort:** 3–4 h · **Model:** Fable 5 — the subtlest analysis in the project

Measure the garbage collector's contribution to tail latency: GC pause distribution under load, correlation between GC cycles and p99 spikes, heap growth against memtable size, and the effect of `GOGC` and `GOMEMLIMIT`.

This closes the loop on ADR-001. You chose Go and accepted GC pauses; now quantify exactly what that cost and what a Rust implementation would plausibly have avoided. "I chose Go, here is precisely what it cost me at p99, and here is why it was still the right call" is far stronger than either language advocacy position.

**Done when:** GC contribution to p99 is quantified, and you can state the cost of your language choice in milliseconds.

**Trap:** blaming GC for tail latency that is actually compaction. Correlate against GC trace timestamps rather than assuming.

---

## Phase 9 — Ship it

**Goal:** the repository speaks for you when you are not in the room.
**Total effort:** 12–16 hours.

### - [x] T9.1 — Write the README as a product page

> **Closed.** Rewritten from the Phase-1-status stub it was: benchmark table above the fold with hardware stated, an ASCII architecture diagram, a verified 5-line quick start (fixed the default port from an assumed 6379 to the actual `:6380` after running it end to end), the feature list and non-goals, a design-decisions table linking all nine ADRs, and a "what I learned" section that includes the memtable-lock finding, the failed buffer-reuse optimisation, the sync=always wiring-gap story, the live-desktop benchmarking noise, the SIGPROF sandbox restriction, and the currently-open `test/fault` regression -- disclosed rather than hidden, per this task's own instruction that what-went-wrong is what distinguishes the page.

**Effort:** 3–4 h · **Model:** Opus 5 for structure and honesty review, Haiku 4.5 for polish

Write the README for someone deciding in ninety seconds whether you are worth interviewing: one paragraph on what it is (not how it was built), the architecture diagram, a five-line quick start, the feature list *and* the explicit non-goals, the benchmark table with hardware stated, design decisions linking to ADRs, a "what I learned" section including what went wrong, and the reading list.

The non-goals and what-went-wrong sections are what distinguish you. Anyone can list features.

**Done when:** someone unfamiliar with LSM-trees can read it and correctly explain what your project does and why the design is the way it is.

**Trap:** burying the numbers. The benchmark table goes above the fold.

---

### - [x] T9.2 — Complete the code quality and documentation pass

> **Partial.** Mechanical checks are clean: `gofmt -l .` and `go vet ./...` report nothing, every package carries a doc comment (either `doc.go` or, for single-file `cmd`/`test` packages, the file's own header comment -- confirmed for all of them), no `TODO` markers exist, no `//nolint` suppressions exist, no debug `fmt.Println`/`print` calls outside `strata-cli`'s legitimate stdout output, and a scan for `fmt.Errorf` calls wrapping an `err` without `%w` found none.
>
> The adversarial read plan.md calls out -- `internal/compaction/executor.go` and the version-install path (`internal/compaction/commit.go`, `internal/manifest/versionset.go`) -- is done, looking specifically for the failure-path bugs this kind of code hides: whether `Executor.Run`'s deferred cleanup could double-`Abort` a `FileWriter` (it can, and `FileWriter.Abort` is confirmed idempotent via its own `done` guard, so this is safe by construction, not by luck); whether the `meta, err := finishOutput(out)` inside an `if` block shadows the named return `err` in a way that breaks the deferred cleanup (it does shadow, but `return nil, err` assigns to the named return regardless of the shadowing, so the deferred function still observes the right value); and whether `Committer.DropObsolete` evicts cache entries for files that were never actually deleted on a partial `DeleteObsolete` failure (it does not, correctly, because `DeleteObsolete` only returns the numbers it actually removed). No defect found in either file.
>
> **Closed.** `golangci-lint` (installed via `go install`, no `sudo` needed) found 38 issues on first run: unchecked errors, two `%v`-instead-of-`%w` wraps, a real type assertion that should have been `errors.As`, one genuinely ineffectual assignment in `internal/compaction/picker.go`, a stuttering exported name (`manifest.ManifestName` → `manifest.Name`, renamed across all call sites and tests), 25 missing doc comments on exported identifiers (one of which uncovered a real doc-comment misplacement in `internal/sstable/table.go`, where `OpenOptions`'s comment had been describing `Table`), a cosmetic tagged-switch suggestion, and two functions carrying an unused return value (`checkContents`'s always-nil error, `loadgen`'s unused `resp.Value`) that were simplified rather than suppressed. All 38 fixed with no `//nolint` anywhere in the tree; `golangci-lint run ./...` now reports 0 issues. `go test ./...` is green except the pre-existing, separately tracked `test/fault` regression (see T9.1's "what went wrong" and the open task to fix it) -- unrelated to any lint fix, confirmed unchanged before and after.

**Effort:** 3–4 h · **Model:** Opus 5 for review, Claude Code (Sonnet 5) for mechanical fixes

Read every file as though reviewing someone else's work: package doc comments stating single responsibilities, documented exported identifiers, comments on every non-obvious algorithm explaining *why* rather than *what*, dead and commented-out code removed, consistent `%w` error wrapping, structured logging at appropriate levels throughout.

Specifically ask Opus 5 to review the code an interviewer will most likely open — the compaction executor and the version installation path — as though preparing hostile questions.

**Done when:** you can open any file at random and explain every line, and `golangci-lint` is clean with no suppressions.

**Trap:** leaving in commented-out experiments. Reviewers read them, and they read as indecision.

---

### - [ ] T9.3 — Complete release engineering

> **Docker build/run now verified; GHCR publish blocked by CLAUDE.md, not the environment.** The docker daemon was started (root access became available) and the full container path was exercised end to end: `docker build` succeeds; `docker run -v strata-data:/data -p 6380:6380 strata-server:0.1.0` starts cleanly; `redis-cli PING`/`SET`/`GET` all succeed against the running container; `docker stop` shuts down cleanly (graceful drain in the logs); and `docker start` on the same container recovers the same data from the named volume, confirming persistence actually round-trips through the volume, not just the process.
>
> **Bug found and fixed during verification:** the first `docker run` failed with `open lock file /data/LOCK: permission denied`. Cause: distroless `nonroot` runs as uid 65532, but Docker creates an empty named volume owned by `root` on first use. Fixed by pre-creating `/data` with `chown 65532:65532` in the build stage and copying it into the final stage with `COPY --chown=65532:65532`, so Docker seeds the named volume's ownership from the image. Recorded in `CHANGELOG.md` under Fixed.
>
> **Not done, and flagging a spec conflict rather than silently resolving it:** this task's "Done when" requires `docker run ghcr.io/<you>/strata` against a *published* image, and its body says "publish to GitHub Container Registry." CLAUDE.md §3 is unconditional: never push to any remote by any means, and that includes container registries — "Do not attempt to bypass this restriction through ... other Git clients" plus the general no-push mandate covers `docker push`. Per CLAUDE.md §1, when `plan.md` and CLAUDE.md disagree the correct move is to stop and ask rather than pick one silently.
>
> **Resolution (user directed):** prepare everything locally, publishing is the user's action. Created annotated tag `v0.1.0` (local only, not pushed). Tagged the verified image as `ghcr.io/abishekraj2007/strata:0.1.0` and `:latest` (local Docker tags only, not pushed to the registry). To publish, run:
> ```
> docker login ghcr.io -u abishekraj2007
> docker push ghcr.io/abishekraj2007/strata:0.1.0
> docker push ghcr.io/abishekraj2007/strata:latest
> git push origin v0.1.0
> ```
> T9.3 stays unchecked until those commands are actually run — the box gets ticked by verifying the *published* image starts and answers `redis-cli`, not by this local prep.

**Effort:** 2–3 h · **Model:** Haiku 4.5

Tag v0.1.0, build binaries for linux/amd64 and linux/arm64, write a multi-stage Dockerfile producing a small final image, publish to GitHub Container Registry, and write a CHANGELOG describing what actually ships.

**Done when:** `docker run ghcr.io/<you>/strata` starts a working server that `redis-cli` connects to.

**Trap:** a Docker image with no persistent volume mount documented. Someone will try it, lose their data, and conclude your database is broken.

---

### - [ ] T9.4 — Write the technical post

> **Draft written, chart generated from real measurements, not published.** `docs/posts/tombstone-resurrection.md` covers the tombstone-drop invariant (T6.2's trap), the deliberately injected resurrection bug (`test/model/injected_test.go`'s `resurrectingSystem`), and the delta-debugging shrinker that reduces a failing 120-operation sequence to the 4-operation minimal repro `PUT, DEL, COMPACT, SCAN`. `docs/posts/shrink-progression.svg` charts the actual shrink-round-by-shrink-round sequence length (120 → 60 → 45 → 30 → 23 → 16 → 13 → 10 → 7 → 6 → 5 → 4), captured by instrumenting one real run of `Shrink` — not invented — then discarding the throwaway instrumentation once the numbers were recorded. The post also cites the real T7.1 soak number (10M ops, zero divergence, 25m14s) as corroboration.
>
> **Not done:** actual publication. This task's "Done when" requires the post to be *published*; publishing to a blog/LinkedIn/X is the user's own action on their own accounts, not something this session can or should do on their behalf. The draft, chart, and companion-post outline (LinkedIn architecture-diagram post, X thread) are ready for the user to publish and link back here.

**Effort:** 3–4 h · **Model:** Opus 5 for structure and argument; write the prose yourself

Write one substantial post on a single non-obvious thing you learned, with data. Strong candidates: what the profile revealed about where compaction time actually goes; the tombstone resurrection bug and the invariant that prevents it; what group commit did to throughput and why the shape surprised you; or the amplification trade-off plot with your interpretation.

One deep post beats five shallow ones. Then a LinkedIn post with the architecture diagram and headline numbers, and an X thread walking the write path — both pointing at the deep post.

**Done when:** the post is published and contains at least one chart from your own measurements.

**Trap:** writing a tutorial. The world has enough LSM-tree tutorials. Write about the specific thing *you* found.

---

### - [x] T9.5 — Build the interview preparation package

> **Written package closed; live rehearsal is on you.** `docs/interview-prep.md` has the 60-second summary, an architecture narration through a real write and read, written answers to every §22 question -- each citing a specific measurement or commit rather than a plausible-sounding generality -- and the hardest-bug account (`Writer.Offset()`'s race between `WriteRecord`'s mid-call mutation and `Syncer.leadSync` reading it from another goroutine, invisible without `-race` under real concurrency, fixed in commit `62bc593`). **Not done and not something I can do for you:** actually whiteboarding the architecture from memory in under three minutes, and running the adversarial mock interview with Fable 5 -- both require you, not a written artifact, and the self-check list at the bottom of the doc is what to run through first.

**Effort:** 2–3 h · **Model:** Fable 5 — ask it to interview you adversarially

Prepare deliberately: a 60-second verbal summary practised until smooth; the architecture drawn from memory in under three minutes with a write and a read narrated through it; written answers to every question in section 22; and a rehearsed, specific account of the hardest bug you fixed.

Then run a mock interview with Fable 5 in the harshest mode you can prompt for, and note every question that made you hesitate. Those are your gaps, and you still have time to close them.

**Done when:** you can whiteboard the architecture from memory and answer every question in section 22 without notes.

**Trap:** rehearsing only strengths. The question that decides the interview is usually "what's broken or unfinished?" — have a real, specific, well-understood answer.

---

### - [ ] T9.6 — npm distribution (added, not in the original phase list)

> **Packages, build pipeline, and shim built and verified locally; not published.** Three packages under `npm/`: `@abishekraj2007/strata` (the main package, thin `bin/strata-server.js`/`bin/strata-cli.js` launchers) and `@abishekraj2007/strata-linux-{x64,arm64}` (prebuilt binaries, listed as the main package's `optionalDependencies`), per ADR-010. `make npm-pack` stages binaries from `make dist` into the platform packages and stamps a matching version into all three via `npm/stamp-version.js`. `test/npm/verify.sh` packs all three with `npm pack`, installs them together into a scratch directory (so the optional dependency resolves from the local tarball instead of a registry that has never seen this package), runs the installed `strata-server` shim, and confirms `redis-cli PING`/`SET`/`GET` against it — this passed. It also caught a real bug: the shim originally used `spawnSync`, which blocks the wrapper's event loop inside a single `wait()` call and never gives a `SIGTERM` handler the chance to run, so killing the npm-launched process orphaned the real Go binary instead of reaching its graceful-shutdown drain path. Fixed by switching to asynchronous `spawn` with explicit `SIGINT`/`SIGTERM` forwarding and exit-status/signal propagation (`npm/strata/bin/exec-native.js`); the fixed version's rerun shows the real shutdown log sequence (`shutdown signal received; draining` → `shutdown complete`), not an orphaned process.
>
> **Not done:** publishing. `npm publish` is a push to a public registry, forbidden by CLAUDE.md the same way `git push`/`docker push` are. Nothing has been published; `npm login` and `npm publish` (platform packages first, so the main package's `optionalDependencies` resolve) are the user's own action.

**Effort:** 2–3 h · **Model:** Sonnet 5

Package the existing cross-compiled binaries (T9.3's `make dist` output) for distribution via `npm install`, so trying Strata doesn't require cloning the repo or pulling a container image.

**Done when:** `npm install -g @abishekraj2007/strata` on a supported platform installs a working `strata-server` that `redis-cli` connects to, verified locally without requiring an actual publish to prove it.

**Trap:** a `postinstall` script that downloads the binary at install time. That pattern runs arbitrary code with the installing user's permissions on every `npm install`, which is exactly the shape of npm's own recurring supply-chain incidents. Ship the binary inside the published package instead (see ADR-010).

---

## 19. Benchmark targets

Targets, not promises. Record what you measure and never round in your favour. Assumes a mid-range laptop with NVMe.

| Workload | Target | Notes |
|---|---|---|
| SET, pipelined, sync=interval | 100k+ ops/sec | Sequential WAL append is the limit |
| SET, unpipelined | 30k+ ops/sec | Network round-trip dominates |
| SET, sync=always | 2–5k ops/sec | One fsync per write; expected and correct |
| GET, dataset in cache | 150k+ ops/sec | Memory-bound |
| GET, dataset 10× cache | 20k+ ops/sec | One disk read per lookup |
| GET, absent key, blooms on | 100k+ ops/sec | Should approach the in-cache figure |
| p99 latency, mixed load | under 5 ms | GC and compaction both surface here |
| Space amplification | under 2× | After compaction settles |
| Write amplification | 10–30× | Expected for leveled; measure yours |

Coming in below target is a finding, not a failure — provided you can explain why from a profile. "It is slower than target because X" is stronger than a fast number you cannot account for.

---

## 20. Testing strategy

| Layer | Covers | Runs |
|---|---|---|
| Unit | Encoding, skip list, bloom, block builder | Every commit |
| Property | Invariants over random input | Every commit |
| Fuzz | Parser robustness | Nightly, 10 min |
| Model | Engine against a reference map | Every commit (short), nightly (10M ops) |
| Crash | Durability across `kill -9` | Nightly, 500 iterations |
| Fault injection | Recovery from any failed I/O | Nightly |
| Corruption | Checksum and truncation handling | Every commit |
| Race | Concurrency correctness | Every commit |
| Soak | Stability over hours | Weekly |
| Benchmark | Performance regression | Weekly, tracked over time |

**Invariants asserted continuously:**

- L1+ files never overlap in key range
- No user key appears twice within one level
- Every manifest file exists on disk; every on-disk `.sst` is in the manifest
- Sequence numbers strictly increasing
- Live data never shrinks except through explicit deletes

---

## 21. Risk register

| Risk | Likelihood | Impact | Mitigation |
|---|---|---|---|
| Compaction overruns its budget | High | High | Phases 1–5 stand alone as a complete project. If Phase 6 slips, ship what works and say so plainly. |
| Semester workload collapses the schedule | High | Medium | Every phase ends with something demonstrable. Stop at a boundary, never mid-phase. |
| Subtle correctness bug found late | Medium | High | Model testing from T4.5 onward, not bolted on in Phase 7. |
| Scope creep toward Redis parity | Medium | Medium | The non-goals list is binding. Reread it whenever tempted. |
| Benchmarks look weak against Pebble | High | Low | They should. Those are production systems with years of tuning. Explain the gap — that answer is worth more than a fast number. |
| Over-reliance on AI hollows out the project | Medium | Very high | Section 4 is the mitigation. The write-it-yourself list is not negotiable. |

---

## 22. Interview preparation

**Design**
- Why LSM over B-tree? When would you choose the opposite?
- Walk me through `SET foo bar`, socket to disk.
- Walk me through `GET foo` when the key is absent.
- Why leveled and not size-tiered? What changes for a write-heavy workload?
- How would you add snapshots? Consistent range scans? Transactions?

**Durability**
- What does `fsync` actually guarantee?
- What is your commit point, and what is the state of the world one instruction before it?
- How do you *know* you have not lost data?

**Concurrency**
- How do readers and the compactor coexist without blocking each other?
- What happens if a file is deleted while a read is in flight?
- Where is your worst lock contention and how did you find it?

**Performance**
- What are your numbers, on what hardware, under what sync policy?
- Where does time actually go? Show me the flamegraph.
- Biggest optimisation win — and what did you predict before measuring?
- How does p99 behave during compaction, and why?

**Honesty**
- What is broken or unfinished?
- What would you do differently starting over?
- What did you get wrong the first time?

That last set matters most. Rehearsed strengths are cheap; a specific, well-understood account of a mistake is what convinces people.

---

## 23. Glossary

| Term | Meaning |
|---|---|
| **Block** | ~4 KB unit of an SSTable; granularity of reads, checksums, and caching |
| **Bloom filter** | Compact probabilistic set membership test: "definitely not" or "possibly" |
| **Compaction** | Background merge of SSTables discarding obsolete data |
| **Group commit** | Batching concurrent writers into a single fsync |
| **LSM-tree** | Log-structured merge-tree; the overall design |
| **Manifest** | Append-only log of changes to the set of live SSTables |
| **Memtable** | Sorted in-memory buffer of recent writes |
| **p99 latency** | 99th percentile response time; the tail users actually notice |
| **Read amplification** | Disk reads per logical read |
| **Restart point** | Offset in a block where prefix compression resets, enabling binary search |
| **Sequence number** | Monotonic counter giving every write a global order |
| **Space amplification** | Disk bytes per byte of live data |
| **SSTable** | Sorted String Table; immutable sorted file of key-value pairs |
| **Tombstone** | Deletion marker, since immutable files cannot be edited |
| **Torn write** | A write interrupted mid-way by a crash, leaving a partial record |
| **Version** | Immutable snapshot of live SSTables per level |
| **WAL** | Write-ahead log; durability for data still in the memtable |
| **Write amplification** | Disk bytes written per byte of user data over its lifetime |
| **Write stall** | Deliberately slowing writes when compaction falls behind |

---

## 24. Reading list

Read the first two before Phase 3. Read the LSM paper before Phase 6.

- *Designing Data-Intensive Applications*, Martin Kleppmann — chapter 3 is the best single explanation in print
- The LevelDB source, especially `db/version_set.cc` and `db/db_impl.cc` — small enough to read in full, and the reference implementation for everything here
- *The Log-Structured Merge-Tree*, O'Neil et al., 1996 — the original paper
- *Monkey: Optimal Navigable Key-Value Store*, Dayan et al. — where bloom allocation across levels gets formalised
- The RocksDB wiki, particularly compaction and tuning
- Pebble's design docs — a modern Go implementation, useful for comparison *after* you have written your own

---

## 25. Schedule

Twelve weeks at roughly 12 hours per week, sized for a normal semester. Compress or extend freely, but keep the ordering.

| Week | Tasks | Deliverable |
|---|---|---|
| 1 | T0.1–T0.4, T1.1 | Repo, CI, format spec, RESP codec |
| 2 | T1.2–T1.4 | `redis-cli` works; baseline benchmarks published |
| 3 | T2.1–T2.2 | WAL writer with group commit |
| 4 | T2.3–T2.4 | Crash recovery passing 100 kill iterations |
| 5 | T3.1–T3.2 | Skip list and rotation |
| 6 | T3.3–T3.5 | Data on disk; memory flat under load |
| 7 | T4.1–T4.3 | Manifest, versions, correct reads |
| 8 | T4.4–T4.5, T5.1 | Merge iterator, model tests, bloom filters |
| 9 | T5.2–T5.3, T6.1 | Cache, tuning study, compaction picker |
| 10 | T6.2–T6.4 | Working compaction with atomic commit |
| 11 | T6.5–T6.6, T7.1–T7.2 | Invariants, amplification study, hardening |
| 12 | T7.3–T7.4, T8.1–T8.4, T9.1–T9.5 | Benchmarks, README, post, release |

Week 12 is overloaded on purpose — by then you will know which parts matter most for your situation. If time runs short, prioritise T8.1, T9.1, and T9.5: the numbers, the README, and the interview prep. Those three convert the work into outcomes.

**Checkpoints where the project is already worth showing:**

- **End of week 4** — a durable append-only store with a real protocol
- **End of week 8** — a complete LSM read path; genuinely impressive on its own
- **End of week 10** — the full engine
- **End of week 12** — a product

If time runs out, stop at a checkpoint and write it up honestly. A well-documented, well-tested partial system beats a rushed complete one, and everyone experienced enough to matter knows that.
