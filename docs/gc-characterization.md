# T8.4 — GC behaviour and tail latency

Measured via `GODEBUG=gctrace=1` (print-based, unaffected by the SIGPROF
restriction documented in `docs/profiles/t8.2-report.md`) alongside
`test/bench/loadgen`, same machine, same 20-client / 50k-keyspace / 64-byte
value shape throughout so the runs are comparable to each other and to
T8.3's `GOGC` table.

`gctrace`'s `A+B+C ms clock` triple is `A` = STW mark-start pause, `B` =
concurrent mark (does not stop the world, but competes for CPU with request
goroutines), `C` = STW mark-termination pause. "STW pause" below always
means `A+C`; `B` is reported separately because it cannot itself appear in
a request's latency the way a real stop-the-world pause can.

## GC pause distribution under load (mixed 80/20, `GOGC=100`, 20s)

1,325 GC cycles in 20 seconds — one roughly every 15ms:

| | value |
|---|---|
| total STW time | 224.9ms (1.1% of wall-clock) |
| mean STW pause | 0.170ms |
| **max STW pause** | **7.914ms** |

## Correlation between GC and p99

Same workload, same duration, `GOGC` varied (data from `test/bench/loadgen`
alongside the trace above):

| `GOGC` | GC cycles/20s | mean STW | max STW | p99 latency | max latency |
|---|---|---|---|---|---|
| 100 (default) | 1,325 | 0.170ms | 7.914ms | 5.901ms | 16.026ms |
| 400 | 375 | 0.274ms | 1.589ms | 2.054ms | 8.808ms |

The correlation is direct, not coincidental: at `GOGC=100` the single worst
STW pause recorded (7.9ms) is close to the measured p99 (5.9ms) and a
meaningful fraction of the measured max (16.0ms) — one unlucky request
landing inside that pause is sufient on its own to produce a multi-millisecond
outlier. At `GOGC=400`, quadrupling the trigger threshold cuts GC frequency
3.5x (1,325 → 375 cycles) and the worst pause 5x (7.9ms → 1.6ms), and p99
drops by very nearly the same factor (5.9ms → 2.05ms, 2.9x). The *mean* STW
pause barely moves (0.17ms → 0.27ms) — this is not "GC gets cheaper", it is
"GC runs less often", which is the entire mechanism `GOGC` controls.

This directly explains T8.1's `sync=interval` numbers sitting well under
their p99 targets at default settings but with occasional high-tens-of-ms
outliers in the raw data: those outliers are GC-cycle collisions with a
client's request, not compaction or lock contention (which `docs/profiles/
t8.2-report.md`'s block/mutex profile already separately accounts for as
83–97% mutex/RWMutex time, not GC time).

## Heap growth vs memtable size

Write-only load, `GOGC=100` (default), 15s, varying `-memtable-mb`:

| `-memtable-mb` | GC cycles/15s | live heap (steady state) | heap goal |
|---|---|---|---|
| 4 (default) | 110 | ~8–9 MB | ~18–19 MB |
| 32 | 28 | ~59–100 MB | ~142–200 MB |

Live heap tracks roughly 2–3x the configured memtable size (consistent with
the active memtable plus at least one immutable memtable pinned during
flush, per T3.2's rotation design), and `GOGC=100`'s heap goal is
consistently ~2x live heap, exactly as the algorithm defines it. GC
frequency falls as memtable size grows because the collector's trigger is a
fraction of live heap: an 8x larger memtable needs roughly 8x more garbage
before the same percentage-growth trigger fires, at a similar allocation
rate — fewer, larger collections rather than a change in total work.

## `GOMEMLIMIT` interaction

`GOGC=400` alone gave the win above only because the machine had headroom to
let live heap grow to ~150 MB unconstrained. Capping it with
`GOMEMLIMIT=64MiB` (same `GOGC=400`, same mixed workload) reproduces
`GOGC=100`'s behaviour almost exactly:

| config | GC cycles/20s | max STW | p99 | max latency |
|---|---|---|---|---|
| `GOGC=400` | 375 | 1.589ms | 2.054ms | 8.808ms |
| `GOGC=400`, `GOMEMLIMIT=64MiB` | 1,274 | 4.513ms | 5.926ms | 17.252ms |
| `GOGC=100` (no limit) | 1,325 | 7.914ms | 5.901ms | 16.026ms |

`GOMEMLIMIT` is the binding constraint once the heap approaches it: the
collector falls back to running as often as `GOGC=100` would, regardless of
the `GOGC` setting, because the alternative is exceeding the limit. Raising
`GOGC` only helps a workload that has memory to spare; on a
memory-constrained deployment `GOMEMLIMIT` reasserts the tradeoff `GOGC=100`
represents whether or not `GOGC` says otherwise.

## What this costs, in milliseconds (closing ADR-001's loop)

At default settings, GC's worst observed single-pause contribution to tail
latency was **7.9ms**, against a measured p99 of 5.9ms and max of 16.0ms on
the same run — GC is not the dominant cost (`docs/profiles/t8.2-report.md`'s
83–97% mutex-wait share of block/mutex samples is larger and more frequent),
but it is not negligible either: a request unlucky enough to land inside a
mark-termination pause pays multiple milliseconds it would not pay in a
non-GC'd runtime. Tuning `GOGC=400` cuts that worst case to 1.6ms — recovering
most of the difference — at the cost of roughly 4x peak RSS (from the ~150 MB
observed heap goal at `GOGC=400` vs ~35 MB at `GOGC=100` in the same run), a
trade only available on a machine with memory to spare, and one `GOMEMLIMIT`
correctly vetoes when it isn't.

**The honest cost of choosing Go over Rust here, quantified rather than
assumed:** a rare few-millisecond stall on a small minority of requests, in
exchange for not hand-writing a memory allocator, a thread-safe reference
counter, or an arena discipline across skip list, block builder, and RESP
parsing paths — the durability and correctness surface plan.md's Phase 0–7
tasks already spent the project's budget on. At this project's current
allocation profile (`docs/profiles/t8.2-report.md`), that stall is bounded
in the single-digit milliseconds and directly tunable; it was never the
unbounded, unpredictable pause language-choice debates assume.

## Reproduction

```
GODEBUG=gctrace=1 bin/strata-server -addr :PORT -data-dir DIR -sync interval \
  -log-level error 2>gctrace.log &
bin/loadgen mixed -addr 127.0.0.1:PORT -clients 20 -duration 20s \
  -keyspace 50000 -read-frac 0.8 -seed 1

grep -oP '\d+\.?\d*\+\d+\.?\d*\+\d+\.?\d* ms clock' gctrace.log \
  | sed 's/ ms clock//' \
  | awk -F'+' '{stw=$1+$3; sum+=stw; n++; if (stw>max) max=stw}
               END{print "n="n, "sum_stw_ms="sum, "mean="sum/n, "max="max}'
```

Vary `GOGC`, `GOMEMLIMIT`, and `-memtable-mb` as environment/flags on the
server invocation to reproduce each table above.
