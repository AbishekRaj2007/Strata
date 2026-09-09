# Benchmarks

Every number in this file is measured, never estimated. Every row states the hardware, the sync policy, and the exact command that reproduces it.

**Rules for this document**, per plan.md T1.4 and T8.1:

- Report the median of at least three runs, with variance. Never best-of-N.
- State hardware in full: CPU, RAM, disk model, filesystem, kernel.
- Record the exact reproduction command for every row.
- Note machine conditions — background load, thermal state, power source.
- Change one thing at a time. A row that varies two parameters measures nothing.

A result below target is a finding, not a failure, provided it can be explained from a profile.

## Test machine

Recorded for T2.2 and confirmed unchanged by the T1.4 baseline run on 2026-08-21. Any later row measured elsewhere states so explicitly.

| Property | Value |
|---|---|
| CPU | 11th Gen Intel Core i5-11400H @ 2.70GHz, 12 logical cores |
| RAM | 8 GB (7.5 GiB usable) |
| Disk | Samsung MZVLQ512HBLU-00B00, 512 GB NVMe |
| Filesystem | ext4 on /dev/nvme0n1p7 |
| Kernel | 7.0.13-arch1-1 |
| Go version | go1.26.4 |
| Load generator | valkey-benchmark 9.1.0 |

Note the RAM figure: 8 GB total. Phase 5 onward deliberately tests datasets larger than this, so a "larger than RAM" claim on this machine means larger than 8 GB, not larger than a server's memory.

## Group commit — WAL sync coordination (T2.2)

Measures the leader-follower handoff in `internal/wal/syncer.go` against a real file, isolated from the WAL record format. It is the fsync amortisation on its own: one writer pays the full sync cost, and concurrent writers share it.

Three runs, `-count=1` each, on the machine below. Median reported.

| Writers | Run 1 | Run 2 | Run 3 | Median |
|---|---|---|---|---|
| 1 | 876 writes/sec | 897 | 880 | **880 writes/sec** |
| 32 | 15,053 writes/sec | 15,033 | 13,492 | **15,033 writes/sec** |
| **Speedup** | 17.2× | 16.8× | 15.3× | **16.8×** |

T2.2's *Done when* asks for at least 5×. The single-writer figure of 880 writes/sec implies ~1.14 ms per fsync, which matches what this NVMe device should cost and confirms the syncs are reaching media rather than being absorbed somewhere.

```sh
go test ./internal/wal -run TestGroupCommitScalesWithConcurrency -v -count=1
```

**Condition that invalidates this measurement:** the file must live on real storage. An earlier version of this benchmark used `t.TempDir()`, which on this host is tmpfs, where `fsync` returns without touching a device — it reported 254,000 writes/sec single-threaded and a 0.9× "speedup", because with nothing to amortise there is nothing for group commit to save. The benchmark now creates its file inside the repository, with `STRATA_BENCH_DIR` to override on hosts whose checkout is itself on tmpfs or NFS.

Machine conditions: idle desktop, on AC power, no thermal throttling observed. This is a laptop-class NVMe drive with volatile write cache enabled, so these figures are optimistic relative to a datacentre SSD with cache flushes forced.

## Baseline — in-memory map (T1.4)

The ceiling. Every later number is a fraction of this, and knowing the fraction is how "my disk is slow" is distinguished from "my code is slow".

Measured 2026-08-21 on the machine above, via `make baseline`. Median of 3 runs per row, 1,000,000 requests, 50 clients, 64-byte values, over a 10,000-key keyspace. Engine: in-memory map, no durability, no WAL — nothing on the write path touches a disk.

| Workload | Pipelined | Throughput | p50 | p99 | Range across 3 runs |
|---|---|---|---|---|---|
| SET | no | **181,587 ops/sec** | 0.143 ms | 0.327 ms | 180,440–183,520 |
| SET | yes (P=16) | **1,199,041 ops/sec** | 0.487 ms | 2.695 ms | 1,190,476–1,223,990 |
| GET | no | **186,324 ops/sec** | 0.135 ms | 0.295 ms | 184,706–186,393 |
| GET | yes (P=16) | **1,956,947 ops/sec** | 0.215 ms | 0.951 ms | 1,915,709–1,976,285 |

Reproduce, after `make build` and starting `bin/strata-server -addr :6380`:

```sh
redis-benchmark -p 6380 -t set -n 1000000 -c 50 -d 64 -r 10000 --precision 3
redis-benchmark -p 6380 -t set -n 1000000 -c 50 -d 64 -r 10000 --precision 3 -P 16
redis-benchmark -p 6380 -t get -n 1000000 -c 50 -d 64 -r 10000 --precision 3
redis-benchmark -p 6380 -t get -n 1000000 -c 50 -d 64 -r 10000 --precision 3 -P 16
```

The GET rows are **hit-path** numbers: the keyspace is populated before each measured pass, and `DBSIZE` confirms 9,999 of 10,000 keys present. This is worth stating because it is the easy mistake here — `redis-benchmark -t get` against a freshly flushed database measures the miss path at full speed and reports it in a row labelled GET. The absent-key path is a separate measurement belonging to T5.1, and is not in this table.

**Conditions.** AC power, CPU governor `powersave`, load average 2.77 immediately before the run, load generator `valkey-benchmark 9.1.0`. Two independent invocations of the harness an hour apart agreed within 2% on every row, so the figures are stable despite the machine not being quiesced.

**What this ceiling says.** Unpipelined SET and GET land within 3% of each other at ~180k ops/sec, and both sit near the p50 latency floor of 0.14 ms. At that point the map is not the bottleneck — per-request syscall and scheduling cost is. Pipelining at P=16 multiplies throughput 6.6× for SET and 10.5× for GET, which is the same statement from the other side: amortising the syscall is worth more than anything happening inside the engine. The read/write asymmetry only appears once pipelining removes that floor, and even then it is 1.6×.

The consequence for later phases: any Phase 3+ SET number below ~180k ops/sec is the storage engine's cost and nothing else, because the protocol layer above it has already been shown to sustain that rate. This is the number to subtract from.

**Governor caveat for Phase 8.** These were measured under `powersave`. T8.1 must either re-pin to `powersave` or rerun this baseline under `performance` before comparing — a governor change alone can move these figures more than most optimisations will.

## Profiles (T1.4) — not captured, blocked

`make profile` does not currently produce a usable profile. Two separate problems, one fixed and one open.

**Fixed: the load ended before the capture began.** `profile.sh` started `redis-benchmark -n 2000000` and then captured a 30-second CPU profile. At the pipelined rates in the table above, two million requests complete in about three seconds, so the profiler spent the remaining twenty-seven seconds sampling an idle server and produced a 673-byte profile with essentially no samples. The load is now driven by a restart loop bounded by the capture duration rather than by a request count, and the script refuses to render flamegraphs from a profile whose total sample time is near zero — an empty profile is not a smaller version of the right answer.

**Open: strata-server is SIGKILLed when CPU profiling runs concurrently with load.** Reproducible within a second of the capture starting. What has been ruled out:

| Hypothesis | Evidence against |
|---|---|
| Out of memory | RSS steady at 20 MB, 2.2 GB available, `/proc/pressure/memory` all zeros, `systemd-oomd` inactive |
| A leak under pipelined load | 45 seconds of sustained load with no profiling: RSS flat at 20 MB |
| Profiling overhead alone | 20-second CPU profile plus heap capture on an idle server: survives, returns 200 |
| A Go panic or fatal error | stderr empty; exit status 137, which is SIGKILL, not a runtime abort |
| Bad signal handling in `main.go` | `signal.NotifyContext` is scoped to SIGINT and SIGTERM only; nothing re-raises or self-kills |
| The agent sandbox the diagnosis ran in | Identical failure with the sandbox disabled |

So it needs load *and* profiling together, dies with SIGKILL and no output, and is not memory. The next step is to run `make profile` from an ordinary terminal with `dmesg -w` alongside it — the diagnosis above had no kernel log access, which is the one place the sender of a SIGKILL is recorded.

This blocks nothing in Phase 2. It must be resolved before T8.2, which is a profiling task end to end.

## Targets

From plan.md §19. Targets, not promises.

| Workload | Target |
|---|---|
| SET, pipelined, sync=interval | 100k+ ops/sec |
| SET, unpipelined | 30k+ ops/sec |
| SET, sync=always | 2–5k ops/sec |
| GET, dataset in cache | 150k+ ops/sec |
| GET, dataset 10× cache | 20k+ ops/sec |
| GET, absent key, blooms on | 100k+ ops/sec |
| p99 latency, mixed load | under 5 ms |
| Space amplification | under 2× |
| Write amplification | 10–30× |

## Tuning study (T5.3)

Not yet run. Sweeps bits-per-key, block size, and cache size.

## Amplification characterisation (T6.6)

Not yet run. Measures write, read, and space amplification against the level size multiplier.

## Optimisation log (T8.3)

Not yet run. One row per change, before and after, including the optimisations that did not work.

## Bloom filters and block cache — the tuning study (T5.3)

Measured 2026-09-09 on the machine above. Everything in this section is a Go
benchmark or a Go test, so no server is involved and no network is in the path;
each row isolates one parameter of the SSTable read path.

All benchmark rows are three runs at `-benchtime 200000x`, median reported with
the full range. The cost tables are computed rather than timed and are
deterministic — same seed, same layout — so they carry no variance column.

Reproduce the whole study:

```sh
go test ./internal/sstable/ -run '^$' -bench 'Tuning' -benchtime 200000x -count 3
go test ./internal/sstable/ -run 'TestTuning' -v -count 3
go test ./internal/cache/  -run 'TestTuningCacheSizeAgainstHitRate' -v -count 3
```

Fixture for every SSTable row: one table, 100,000 keys of 16 bytes, values of
22 bytes, 10 bits per key and 4 KiB blocks unless the row varies them.

### What the bloom filter buys on an absent key

Both arms run the identical code against the identical file. The only
difference is whether the filter is consulted — `Table.Get` against
`Table.getUnfiltered` — so the delta cannot be attributed to anything else
about the table.

| Block cache | Filter | Run 1 | Run 2 | Run 3 | Median | Speedup |
|---|---|---|---|---|---|---|
| cold | consulted | 614.9 ns | 516.5 | 460.1 | **516.5 ns** | |
| cold | bypassed | 5608 ns | 5190 | 5464 | **5464 ns** | **10.6×** |
| warm | consulted | 495.3 ns | 513.1 | 623.7 | **513.1 ns** | |
| warm | bypassed | 2305 ns | 2132 | 2179 | **2179 ns** | **4.2×** |

T5.1's target is 5×. The cold figure of **10.6×** clears it; the warm figure of
4.2× does not.

The two numbers answer different questions and the cold one is the relevant
one. An absent key's blocks are, by definition, the blocks nothing has read —
so on a real absent-key lookup the cache is cold for exactly that block, and
the filter is skipping a `pread`. The warm row measures something narrower and
still worth knowing: even with the block already in memory, consulting the
filter is 4.2× faster than decoding the block, because the block read is not
the whole cost — the restart-array binary search and the entry scan are the
rest, and the filter skips those too.

### What the bloom filter costs on a present key

The control. A filter cannot help a key that is present: it says "maybe", the
block is read anyway, and the only effect is one hash and seven bit tests of
added work.

| Filter | Run 1 | Run 2 | Run 3 | Median |
|---|---|---|---|---|
| consulted | 3612 ns | 3408 | 3505 | **3505 ns** |
| bypassed | 3613 ns | 3316 | 3775 | **3613 ns** |

The medians differ by 3% in favour of the *filtered* path, which is not a real
effect — the ranges overlap almost completely. The honest reading is that the
filter's cost on a present key is below this benchmark's resolution, which is
what the arithmetic predicts: one xxHash plus seven bit tests against a 3.5 µs
lookup.

### Bits per key

Memory cost and false positive rate (deterministic, from
`TestTuningBitsPerKeyCost`):

| bits/key | k | bloom block | bytes/key | measured FP rate | theoretical |
|---|---|---|---|---|---|
| 4 | 3 | 50,016 B | 0.50 | 14.83% | 14.69% |
| 8 | 6 | 100,016 B | 1.00 | 2.17% | 2.16% |
| 10 | 7 | 125,016 B | 1.25 | **0.83%** | 0.82% |
| 16 | 11 | 200,016 B | 2.00 | 0.05% | 0.05% |

Absent-key latency at each setting, warm cache:

| bits/key | Run 1 | Run 2 | Run 3 | Median | Range |
|---|---|---|---|---|---|
| 4 | 785.3 ns | 672.6 | 850.8 | **785.3 ns** | 672.6–850.8 |
| 8 | 488.0 ns | 429.8 | 539.0 | **488.0 ns** | 429.8–539.0 |
| 10 | 401.9 ns | 486.6 | 500.7 | **486.6 ns** | 401.9–500.7 |
| 16 | 629.2 ns | 379.1 | 434.4 | **434.4 ns** | 379.1–629.2 |

**Finding, stated carefully:** only the 4-bit configuration separates from the
others by more than run-to-run variance. Its 14.8% false positive rate means
roughly one absent lookup in seven still pays for a full block read, and that
shows up as ~300 ns of median latency. From 8 bits upward the ranges overlap
and this benchmark cannot distinguish them.

So the recommendation of **10 bits per key** does not rest on the latency
sweep, which does not support it. It rests on the cost table: 10 bits buys a
0.83% false positive rate for 1.25 bytes per key, where 8 bits gives 2.17% for
1.00 and 16 bits gives 0.05% for 2.00. Going 8 → 10 removes about 60% of the
remaining false positives for 0.25 bytes per key; going 10 → 16 removes 94% of
what is left but costs 0.75 bytes per key, and at 0.83% there is little left
worth buying. Saying this plainly matters more than producing a latency graph
with a convenient minimum in it.

### Block size

Read amplification (deterministic, from `TestTuningBlockSizeCost`; 10,000
lookups spread across the keyspace by a stride of 7,919 so consecutive reads do
not land in the same block):

| block size | data blocks | file bytes | bytes read per lookup |
|---|---|---|---|
| 1 KiB | 3,704 | 3,916,859 | **994.7** |
| 4 KiB | 893 | 3,788,868 | 4,074.0 |
| 16 KiB | 221 | 3,765,500 | 16,448.1 |
| 64 KiB | 56 | 3,756,824 | 65,927.2 |

Present-key latency, no block cache:

| block size | Run 1 | Run 2 | Run 3 | Median |
|---|---|---|---|---|
| 1 KiB | 5222 ns | 5597 | 5088 | **5222 ns** |
| 4 KiB | 7392 ns | 7701 | 7944 | **7701 ns** |
| 16 KiB | 14,047 ns | 14,573 | 15,265 | **14,573 ns** |
| 64 KiB | 41,837 ns | 42,321 | 41,381 | **41,837 ns** |

Read amplification is very close to linear in block size, and latency tracks
it: a random point lookup reads one block and uses about 38 bytes of it, so
the cost of the read is the cost of the block. At 64 KiB a lookup moves 65 KB
to answer a 38-byte question — 1,700× amplification.

**Finding that contradicts the default:** for pure random point reads, 1 KiB
blocks are **1.5× faster** than the 4 KiB default and read 4× fewer bytes. The
larger index that costs — 3,704 entries instead of 893, and a file 128 KB
larger from the extra per-block restart and trailer overhead — does not show up
as latency, because the index binary search is in memory and grows only
logarithmically.

**The default stays at 4 KiB, and this study does not justify that.** What it
measures is random point reads, which is the workload block size is worst for.
Range scans amortise a block across many entries and pull the other way, as
does the per-block index memory once a database holds thousands of tables
rather than one. Neither has been measured yet. Recording this as an open
question is the honest outcome; changing a durable format default on one
workload's evidence would not be.

### Cache size

Zipf-distributed reads over 20,000 distinct 4 KiB blocks — 78 MiB of data —
500,000 accesses. The generator is seeded, so all three runs are byte-identical
and the table has no variance to report; the run-to-run check confirms the
measurement is deterministic rather than that it is stable under noise.

| capacity | resident blocks | hit rate, s=1.05 | hit rate, s=1.20 |
|---|---|---|---|
| 1 MiB | 256 | 52.76% | 72.46% |
| 4 MiB | 1,024 | 67.94% | 83.66% |
| 16 MiB | 4,096 | 82.61% | **91.98%** |
| 64 MiB | 16,384 | **95.59%** | **96.82%** |
| 256 MiB | 19,144 | 96.17% | 96.82% |

The knee is at 64 MiB for this working set. Going 16 → 64 MiB buys 13 points at
s=1.05; going 64 → 256 MiB buys 0.6 points at s=1.05 and nothing at all at
s=1.20, because at that skew the tail past 64 MiB is accessed so rarely that
holding it resident is wasted memory.

Two things this table makes concrete. First, the hit rate is a property of the
workload's skew at least as much as of the cache: at 1 MiB, moving from s=1.05
to s=1.20 is worth 20 points, more than a 16× capacity increase buys at the
lower skew. Second, T5.2's ">90% on a Zipfian workload" is met at 16 MiB for a
78 MiB working set — 20% of the data — which is the ordinary bargain a cache
offers and worth stating as a ratio rather than as an absolute.

**Recommendation: 64 MiB**, the current default, on the evidence above. It is
the knee for a working set of this size; a database whose hot set is much
larger should scale it, and this table gives the shape of the curve to scale
along.

### Summary of defaults

| Parameter | Default | Measurement it rests on |
|---|---|---|
| Bloom bits per key | 10 | 0.83% false positive rate at 1.25 bytes/key; the knee of the cost table above |
| Bloom probes, k | 7 | Derived as (m/n)·ln2, confirmed by `TestExcessProbesMakeTheFilterWorse`: 3.27% at k=2, 0.83% at k=7, 5.50% at k=20 |
| Block size | 4 KiB | **Not supported by this study for point reads** — 1 KiB measured 1.5× faster. Retained pending a scan benchmark. |
| Block cache | 64 MiB | 95.6% hit rate at s=1.05 on a 78 MiB working set; 256 MiB buys 0.6 more points |

Machine conditions: idle desktop, AC power, CPU governor `powersave`, no
thermal throttling observed. Benchmark variance across the three runs is 5–35%
on the sub-microsecond rows, which is why the bits-per-key finding above is
stated as "cannot distinguish" rather than as a ranking.
