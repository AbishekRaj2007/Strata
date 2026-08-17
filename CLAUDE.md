# CLAUDE.md

Strata — a persistent, log-structured key-value store in Go with a Redis-compatible wire protocol.

**Read [plan.md](plan.md) before doing anything on this repo.** It is the authoritative specification: goals and non-goals (§2), technical decisions (§5), architecture (§6), byte-exact on-disk formats (§7), repository layout (§8), and the phased task list (Phases 0–9) with a *Done when* condition per task. This file covers how to work; plan.md covers what to build.

When plan.md and this file disagree, plan.md wins. When the code and plan.md disagree, stop and raise it — silently drifting from the spec is the failure mode that costs the most here.

## Project status

Pre-implementation. The repository contains plan.md only. The first work is Phase 0 (T0.1–T0.4): repository skeleton, format specification, ADRs, dev tooling.

The directory is named `Starta`; plan.md names the project `Strata`. Resolve this before Phase 1 — repo name, Go module path, and the `INFO` banner must agree.

## The rule that overrides convenience

Section 4 of plan.md defines which components must be hand-written by the author. Do **not** generate, scaffold, or "just sketch" these:

- skip list
- bloom filter
- block encoder and decoder
- k-way merge iterator
- compaction picker and executor
- WAL record framing and recovery

For these, the only allowed help is *after* a working implementation exists: review it, attack it, propose tests, find the hole in the durability argument. If asked to write one from scratch, say so and offer review instead.

Everything else is fair game: scaffolding, Makefiles, CI, test harnesses, fixtures, benchmark runners, profiling interpretation, docs.

## Language and dependencies

Go, standard library only for anything load-bearing. Permitted third-party packages: `testify`, `xxhash`, `golang.org/x/sys/unix`. Anything else requires a new ADR in `docs/adr/` first. If a library does the interesting part of a task, it is disqualified by definition.

Linux only. Do not add Windows compatibility shims.

## Code standards

Write code that reads well under interview questioning, because that is its actual purpose.

- Every package has a `doc.go` stating its single responsibility in one sentence. A sentence needing an "and" means the package should be two packages.
- Exported identifiers are documented. Package comments explain the responsibility, not the mechanics.
- Comments explain **why**, never **what**. A comment restating the line below it is noise; delete it. Reserve comments for non-obvious algorithms, ordering constraints, durability arguments, and memory-ordering reasoning — the places where the code cannot speak for itself.
- No commented-out code, no leftover experiments, no `TODO` without an owning task ID from plan.md.
- Errors wrap with `%w`. Errors are values returned up, not logged and swallowed at the point of failure.
- The engine interface is `Put`, `Get`, `Delete`, `Scan`, `Close`, `Stats` and returns `(value, error)` — never `(value, bool)`. It was designed in T1.3 for the LSM engine, not for the temporary map.
- Logging is structured, behind the single logger interface, at levels that stay useful under load.
- `golangci-lint` must be clean with no suppressions. A suppression needs a written justification.

Naming and structure follow the surrounding code. Comparators are `(user_key ascending, sequence descending)` everywhere — one comparator, used identically in the memtable, the block format, the merge iterator, and compaction.

## Durability invariants

These are the properties the project exists to demonstrate. Any change touching the write path, flush, or compaction must preserve them, and the reasoning belongs in the commit message.

- An acknowledged write survives `kill -9`. Only acknowledged writes carry that guarantee.
- A WAL file may be deleted only once its data is durable **and** referenced by a durable manifest. Not after the SSTable fsync — after the manifest fsync.
- The manifest fsync is the commit point for compaction. Before it, the compaction never happened; after it, it fully happened. There is no third state.
- File creation is not durable until the containing directory is fsynced.
- A tombstone is dropped only at the bottom-most level that could contain the key, with no snapshot requiring it. Dropping early resurrects deleted data.
- A file is deleted from disk only when no held version references it.
- A failed fsync is unrecoverable: close the database and require a restart.

Silently returning wrong data is worse than crashing. Checksums are consulted on every read path, cache hits included.

## Testing

Testing strategy is §20 of plan.md. Practical rules:

- Tests come before or alongside implementation, never bolted on after.
- Table-driven tests for encoding and protocol work, including every malformed input you can construct.
- Property tests over random input for anything with an invariant.
- The reference-model test (T4.5) runs from Phase 4 onward, not from Phase 7. Every later phase is validated against it automatically.
- Crash tests record **acknowledgements**, not sends.
- Everything runs under `-race`. Contrive tests that force compaction and reads to overlap rather than hoping they collide.
- The invariant checker (T6.5) runs after every compaction in test builds.

Coverage target is above 70% on `internal/engine` and `internal/compaction`.

## Benchmarks

Never invent a number. Every figure in the README, in docs, or in conversation traces to a run recorded in `docs/benchmarks.md` with hardware, sync policy, and the exact reproduction command.

Report median and variance across at least three runs — never best-of-N. Change one thing, measure one thing. Profile before optimising, every time.

## Task workflow

Work one task from plan.md at a time. A task closes only when its *Done when* condition is satisfied in full — not when the code compiles and looks right. Read the task's **Trap** before starting; each one documents a failure mode that costs a day.

Do not convert a task's scope sub-bullets into a checklist. They describe a system to think through, not items to tick off.

Phase order is fixed. Stop at phase boundaries, never mid-phase.

## Documentation

- `docs/format.md` is the binding on-disk specification. It must stay ahead of the code — change the spec first, then the reader, writer, and fixtures. Someone with no access to the source should be able to write a conformant reader from it alone.
- `docs/adr/` holds one file per decision: context, decision, alternatives rejected, consequences accepted. The consequences section is the one that matters.
- `docs/benchmarks.md` holds every measurement with its conditions.
- README is a product page, not a build log. Numbers above the fold, non-goals stated explicitly.

If AI assistance was substantial for a component, the README says so. That honesty is part of the deliverable.
