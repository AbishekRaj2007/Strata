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

Not yet recorded. T1.4 fills this in with the machine every subsequent number is measured on.

| Property | Value |
|---|---|
| CPU | |
| RAM | |
| Disk | |
| Filesystem | |
| Kernel | |
| Go version | |

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
