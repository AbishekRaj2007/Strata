# T8.3 — Optimisation cycle

Worked in the order `docs/profiles/t8.2-report.md` ranked, one change at a
time, each measured before moving to the next.

## 1. Reuse the WAL writer's encode buffer (succeeded)

**Where:** `internal/wal/writer.go`, `Writer.Write`.

**Before:** `Write(b *Batch)` called `b.Encode(nil)` — a fresh allocation on
every batch — even though `WriteRecord` copies every byte of `payload` into
its own fixed `[BlockSize]byte` block buffer via `copy()` before returning.
Nothing outlives the call, so the destination slice `Encode` writes into is
safe to reuse on the next `Write`.

**Change:** `Writer` grew an `encodeBuf []byte` field; `Write` now does
`w.encodeBuf = b.Encode(w.encodeBuf[:0])`.

**Measured** (`BenchmarkWriterWrite`, `go test -bench -benchmem`, median of 3
runs, single-record batches):

| | ns/op | B/op | allocs/op |
|---|---|---|---|
| before | 3055 | 216 | 5 |
| after | 2666 | 64 | 1 |

5 allocs → 1, ~13% faster. The remaining allocation is `Batch.Records`
growing inside the benchmark's own loop, not `Writer`.

## 2. Reuse the per-connection command-args slice (succeeded)

**Where:** `internal/server/server.go`, `commandArgs` / `conn.serve`.

**Before:** `commandArgs` allocated a fresh `[][]byte` on every command. Its
elements are pointers into the `resp.Value` tree the reader just produced —
not into the returned slice's own backing array — so the backing array
itself is free to reuse once `dispatch` for that command has returned.

**Change:** `commandArgs` now appends onto a caller-supplied `dst`;
`conn` keeps the returned slice in a new `argsBuf` field and passes
`argsBuf[:0]` back in on the next command.

**Measured** (`BenchmarkCommandArgs/{Fresh,Reused}`, median of 3 runs):

| | ns/op | B/op | allocs/op |
|---|---|---|---|
| fresh (before) | 264 | 168 | 3 |
| reused (after) | 13 | 0 | 0 |

## 3. `GOGC` tuning (succeeded — configuration, not code)

**Hypothesis:** the default `GOGC=100` runs the garbage collector more often
than a workload with Strata's allocation profile needs, trading CPU and
pause time for memory the machine has to spare; raising it should improve
both throughput and tail latency at the cost of peak heap size.

**Measured** (`loadgen mixed -clients 20 -keyspace 50000 -read-frac 0.8`,
`sync=interval`, 10s runs, median of 3 runs each, same machine and back to
back to keep background load comparable across the pair):

| GOGC | throughput (ops/sec) | p50 | p95 | p99 | p99.9 |
|---|---|---|---|---|---|
| 100 (default) | 22,307 | 0.46ms | 3.50ms | 5.71ms | 8.19ms |
| 400 | 38,372 | 0.42ms | 1.30ms | 2.17ms | 3.69ms |

Consistent across all three runs at each setting (throughput within 1% run
to run, unlike the read-path noise T8.1 recorded elsewhere on this machine).
~72% more throughput and roughly 2.6x better p99 at `GOGC=400`, which is
GC pause time coming off the tail directly — quantified further in T8.4.
The cost this table doesn't show is peak RSS, which rises with `GOGC`; T8.4
measures that tradeoff explicitly rather than this report picking a GOGC
value or claiming this table alone justifies one.

## 4. Reuse the RESP reader's bulk-string buffer (failed)

**Hypothesis:** `readBulk`'s `buf := make([]byte, n)` is the largest single
flat allocation site in T8.2's heap profile (18.8% of `alloc_space`); a
`Reader`-owned scratch buffer reused across calls, the same pattern that
worked for optimisations 1 and 2, should cut it to near zero.

**What was tried:** added a `scratch []byte` field to `resp.Reader` and had
`readBulk` slice into it instead of allocating, growing it only when the
next value is larger than its current capacity.

**Result: broke correctness immediately.** `go test ./internal/resp/...`
failed `TestReadArray` (`Array = "foo", "foo"` — two elements read as the
same string) and a fuzz seed; `go test ./internal/server/...` failed six
tests outright and panicked a seventh on an out-of-range index.

**Why it failed, and why optimisations 1 and 2 didn't:** in both successful
cases, the reused buffer's *contents* were fully consumed by the time of
reuse — `WriteRecord` copies the encoded batch into its own block buffer
before `Write` returns, and `dispatch` finishes reading every argument
before the connection loop asks for the next command. `readBulk`'s buffer is
different in kind: its whole purpose is to become `Value.Bytes`, which is
handed to the caller and is still live — stored in the args slice, then
inside the engine — for as long as that value matters, which is *past* the
point where the next `ReadValue` call would need the same scratch space to
read the next argument. A three-element command array reads three bulk
strings into the same backing array *within a single command*, so even the
first argument gets clobbered before `commandArgs` ever runs. This is not a
lifetime that a `Reader`-level scratch buffer can safely share — it would
need either a copy on the way out (defeating the point) or the consumer
taking ownership of pool slots explicitly, which no part of the current
request path does. Reverted; `internal/resp/resp.go` is unchanged from
before this experiment.

## Summary

| # | Change | Result |
|---|---|---|
| 1 | WAL encode buffer reuse | succeeded — 5→1 allocs/op |
| 2 | Command-args slice reuse | succeeded — 3→0 allocs/op |
| 3 | `GOGC=400` | succeeded — +72% throughput, p99 2.6x better |
| 4 | RESP bulk-buffer reuse | failed — breaks correctness, reverted |

Three succeeded with before/after data; one failed and is documented with
its root cause, per plan.md's T8.3 trap ("one change, one measurement,
always") and done-when condition.
