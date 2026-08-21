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
