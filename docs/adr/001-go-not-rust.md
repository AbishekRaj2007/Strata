# ADR-001: Go, not Rust

**Status:** Accepted

## Context

A storage engine's central difficulty is that a compaction thread rewrites and deletes files while reader threads are actively reading them. Every design must answer: what stops a reader from touching a file that compaction just unlinked?

There are two ways to spend the project's effort on that question. One is to let the language enforce the answer at compile time. The other is to solve it explicitly at runtime and be able to explain the solution.

The project's purpose is understanding storage engines, not demonstrating language proficiency. Whatever is chosen has to be defensible under questioning.

## Decision

Go, with an explicit reference-counting scheme over immutable `Version` snapshots (see ADR-008 and plan.md §6).

## Alternatives considered

**Rust.** Genuinely the stronger engineering choice for a production engine, and this is not a close call on merit alone. `Arc` plus lifetimes would make the file-lifetime bug a compile error rather than a race that surfaces once every few million operations. No GC means no GC contribution to p99, which is the exact tail-latency problem this project will have to measure and explain in T8.4. Rejected because the borrow checker would absorb a large share of the twelve-week budget teaching memory lifetimes, and that is not the subject being studied. The lifetime problem does not disappear in Go — it just moves from the compiler to a scheme that has to be designed, implemented, and defended, which is more instructive for this specific purpose.

**C++.** What LevelDB and RocksDB are actually written in, so the reference implementations would translate directly. Rejected: manual lifetime management plus no memory safety plus a slow iteration loop is the worst combination available for a solo twelve-week project, and use-after-free in a compaction path is precisely the bug class that would consume weeks.

**Zig.** Attractive allocator control and explicit error handling. Rejected: an immature ecosystem, no `pprof` equivalent, and near-zero interview legibility. The profiling story alone disqualifies it, because Phase 8 depends entirely on good profiling tools.

## Consequences

**Accepted:** GC pauses land directly in p99 latency, which is the metric this project reports. That cost is not hypothetical, and T8.4 exists to quantify it in milliseconds rather than hand-wave it.

**Accepted:** the file-lifetime problem must be solved by hand. A missed reference release leaks disk space; a premature release causes a read of a deleted file. Neither is caught by the compiler, and the race detector only catches what the test suite actually executes. T4.2 and its fault-injection test exist because of this decision.

**Accepted:** `[]byte`-to-`string` conversions allocate, so comparators on the hot path need care to avoid garbage that would not exist in Rust. T8.3 budgets for this specifically.

**Gained:** `pprof`, the race detector, and `testing` ship with the toolchain. Phase 8 is a matter of running tools rather than assembling them.

**Gained:** goroutines make one-per-connection and dedicated flusher/compactor goroutines the obvious structure rather than a thread-pool design exercise.

**Follow-up:** a Rust port of the storage engine is planned after v0.1.0. Having measured GC's p99 contribution first makes that port a comparison with data behind it rather than a rewrite for its own sake.
