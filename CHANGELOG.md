# Changelog

All notable changes to Strata are recorded here. Format loosely follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/); dates are when a
phase's work landed, not when this file was written.

## [0.1.0] — unreleased

The first tagged snapshot. Phases 0–7 (foundations through correctness
hardening) are complete and their `plan.md` done-when conditions are met.
Phase 8 (performance) and Phase 9 (shipping polish) are partial — see
"Known gaps" below and `plan.md` for the authoritative, per-task status.

### Added

- RESP2 protocol implementation and TCP server with graceful shutdown
  (Phase 1): `SET`, `GET`, `DEL`, `EXISTS`, `SCAN`, `PING`, and the rest of
  the v1 command set, round-tripping against both `redis-cli` and `go-redis`.
- Write-ahead log with 32 KiB block framing, CRC32C checksums, and a
  truncation-tolerant recovery reader (Phase 2).
- Group commit (leader-follower fsync batching) for `sync=always`, measured
  at 16.8× median speedup with 32 concurrent writers; wired onto the live
  write path in this release, alongside a per-slot quiescence mechanism so
  the flusher cannot close a WAL file a concurrent writer is still syncing.
- Concurrent skip list memtable, memtable rotation with bounded-queue
  backpressure, and an SSTable block encoder/decoder with prefix
  compression (Phase 3).
- SSTable builder/reader, manifest-based version tracking with reference
  counting, the full `Get` path, a heap-based k-way merge iterator, and
  `SCAN` with a key-based cursor (Phase 4).
- Bloom filters and a sharded, checksum-on-every-read block cache
  (Phase 5), with a tuning study backing every parameter recommendation.
- Leveled compaction: picker, executor, atomic version installation with
  crash-safe orphan cleanup, scheduling with write stalls, continuous
  invariant verification, and measured space/write/read amplification
  (Phase 6).
- Correctness hardening (Phase 7): a 10M-operation model-based test with
  zero divergence, a full I/O fault injection layer, a corruption and
  edge-case sweep, and a documented concurrency audit
  (`docs/concurrency.md`).
- Process-level crash fault injection (`STRATA_CRASH_AT`,
  `STRATA_CRASH_AFTER_N`) wired into the real server at the WAL-write,
  WAL-sync, and flush-window points, so `test/crash` can kill the process
  at an exact spot instead of relying on random timing.
- `strata-cli`, a development binary for dumping SSTables, manifest
  history, and level layout, and validating data-directory invariants.
- A full benchmark harness and result set (`test/bench/`, T8.1), and a
  dependency-free SVG renderer for throughput-over-time charts.
- A multi-stage Dockerfile producing a static, distroless `strata-server`
  image, and a `make dist` target cross-compiling release binaries for
  linux/amd64 and linux/arm64.

### Fixed

- Dockerfile: the named `/data` volume was created owned by `root` on
  first `docker run`, but the distroless `nonroot` final stage runs as
  uid 65532 and could not write to it — the server failed at startup
  with `open lock file /data/LOCK: permission denied`. The build stage
  now pre-creates `/data` with the correct ownership and the final
  stage copies it in with `--chown=65532:65532`, so Docker seeds the
  named volume's ownership from the image on first use.
- `internal/wal`: truncate the unsynced WAL tail on fsync failure
  (T7.2 regression).

### Known gaps (tracked in `plan.md`, not hidden here)

- **T9.4 (the technical post)** has not been written yet.

See `plan.md` for the complete, authoritative per-task history; this file
summarizes rather than replaces it.
