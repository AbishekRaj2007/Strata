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

Recorded for T2.2. T1.4 confirms this is the same machine when it records the baseline; any later row measured elsewhere states so explicitly.

| Property | Value |
|---|---|
| CPU | 11th Gen Intel Core i5-11400H @ 2.70GHz, 12 logical cores |
| RAM | 8 GB |
| Disk | Samsung MZVLQ512HBLU-00B00, 512 GB NVMe |
| Filesystem | ext4 on /dev/nvme0n1p7 |
| Kernel | 7.0.13-arch1-1 |
| Go version | go1.26.4 |

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

Not yet measured.

| Workload | Pipelined | Throughput | p50 | p99 | Command |
|---|---|---|---|---|---|

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
