# CLAUDE.md

Strata — a persistent, log-structured key-value store in Go with a Redis-compatible wire protocol.

## 1. Source of Truth

**Read `plan.md` before doing anything in this repository.**

`plan.md` is the authoritative specification for:

* Goals and non-goals (§2)
* Technical decisions (§5)
* Architecture (§6)
* Byte-exact on-disk formats (§7)
* Repository layout (§8)
* Phased task list (Phases 0–9)
* Task scope
* Task traps
* Task "Done when" conditions

When `plan.md` and this file disagree, **`plan.md` wins**.

When the existing code and `plan.md` disagree:

1. Stop.
2. Identify the disagreement clearly.
3. Explain the impact.
4. Do not silently change the implementation to resolve the disagreement.
5. Ask for direction if the correct resolution cannot be determined from `plan.md`.

Do not silently drift from the specification.

---

## 2. Your Role

You have full authority to **implement, modify, refactor, test, debug, benchmark, document, and improve the codebase** according to `plan.md`.

Unlike previous restrictions, there are **no author-only implementation areas**.

You may implement all components required by the project, including:

* Skip lists
* Bloom filters
* Block encoding and decoding
* K-way merge iterators
* Compaction picker and executor
* WAL record framing
* WAL recovery
* Manifest logging
* Memtables
* SSTables
* Compaction
* Engine functionality
* RESP protocol handling
* CLI functionality
* Server functionality
* Tests
* Benchmarks
* Tooling
* Documentation

Use the architecture and constraints defined by `plan.md`.

Do not invent alternative architecture merely because it is easier to implement.

---

## 3. Git Safety

You may:

* Inspect Git history
* Create commits
* Create branches
* Modify files
* Run Git commands needed for development
* Review diffs
* Inspect status and logs

### NEVER push

**You must never execute any command that pushes changes to a remote repository.**

Forbidden commands include, but are not limited to:

```bash
git push
git push --force
git push --force-with-lease
git push origin ...
git push <remote> ...
```

Do not push tags or branches.

Do not modify Git configuration or credentials in order to enable pushing.

Do not attempt to bypass this restriction through:

* GitHub CLI
* REST APIs
* SSH
* HTTPS
* Other Git clients
* Direct remote API calls
* Scripts
* CI triggers intended to publish changes

You may prepare commits locally. **The user is responsible for pushing.**

Before finishing a task, it is acceptable to report:

```text
Local changes are complete and committed.
Nothing was pushed to the remote repository.
```

---

## 4. Task Discipline

Work on **one task from `plan.md` at a time**.

Before implementing a task:

1. Read the relevant section of `plan.md`.
2. Read the task's **Trap**.
3. Inspect the existing implementation and surrounding packages.
4. Identify relevant interfaces, invariants, formats, and tests.
5. Determine the task's exact "Done when" condition.
6. Implement the task.
7. Add or update tests.
8. Run the appropriate validation.
9. Confirm the complete "Done when" condition.
10. Update documentation/status only when appropriate.

Do not declare a task complete merely because the code compiles.

A task is complete only when its stated **Done when** condition is satisfied.

---

## 5. Phase Discipline

Respect the phase structure in `plan.md`.

Do not casually skip ahead to unrelated future-phase functionality.

If an earlier incomplete task blocks the current task:

1. Identify the dependency.
2. Determine whether the blocker can legitimately be completed as part of the current task.
3. If not, stop and report the dependency.

Do not silently reorder the project's architecture.

If `plan.md` explicitly permits work ahead of the current phase, follow the specification.

---

## 6. Current Project Status

Treat the current repository state and `plan.md` as authoritative.

Do not rely on this section as a replacement for inspecting the repository.

At the beginning of a session:

```bash
git status
git log -5 --oneline
```

Then read:

```text
plan.md
```

Inspect the relevant source, tests, and documentation before making changes.

---

## 7. Architecture

Follow the architecture defined in `plan.md`.

Do not introduce unnecessary abstractions.

Do not replace an explicitly specified design with a library or framework unless `plan.md` permits it.

Maintain the project's intended separation between:

* Engine
* Memtable
* WAL
* Manifest
* SSTables
* Compaction
* Storage formats
* RESP protocol
* CLI/server layers

Respect existing interfaces unless the specification explicitly requires changing them.

The engine interface is:

```text
Put
Get
Delete
Scan
Close
Stats
```

Engine operations return:

```text
(value, error)
```

Never introduce `(value, bool)` semantics into the engine API.

---

## 8. Language and Dependencies

The project is:

* Go
* Linux only
* Standard library preferred for load-bearing functionality

Permitted third-party dependencies:

* `testify`
* `xxhash`
* `golang.org/x/sys/unix`

Do not introduce additional dependencies without first creating an ADR in `docs/adr/`.

If a dependency performs the core/interesting part of a task that the project is intended to demonstrate, prefer implementing the functionality directly.

Do not add Windows compatibility layers.

---

## 9. Code Quality

Write code that is understandable under technical/interview questioning.

Priorities:

1. Correctness
2. Durability
3. Specification compliance
4. Simplicity
5. Testability
6. Performance

Every package should have a `doc.go` describing its single responsibility in one sentence.

Document exported identifiers.

Package comments should describe responsibility, not implementation mechanics.

Comments should explain **why**, not **what**.

Good comments are appropriate for:

* Non-obvious algorithms
* Ordering constraints
* Durability arguments
* Concurrency reasoning
* Memory-ordering requirements
* Recovery invariants

Avoid:

* Commented-out code
* Dead experiments
* Unnecessary comments
* Unowned TODOs

Any TODO must reference an owning task ID from `plan.md`.

Errors should wrap underlying errors with `%w` where appropriate.

Errors should propagate rather than being logged and swallowed.

---

## 10. Ordering

The canonical comparator is:

```text
(user_key ascending, sequence descending)
```

Use this ordering consistently across:

* Memtables
* Blocks
* Iterators
* Merge operations
* Compaction
* SSTables

Do not introduce subtly different comparators in different layers.

---

## 11. Durability Invariants

These invariants are fundamental to Strata.

Any change affecting writes, flushing, recovery, manifest handling, or compaction must preserve them.

### Acknowledged writes

An acknowledged write must survive:

```text
kill -9
```

Only acknowledged writes carry this guarantee.

### WAL deletion

A WAL may be deleted only after:

1. Its data is durable.
2. The corresponding state is referenced by a durable manifest.
3. The manifest has been successfully fsynced.

Do not delete WAL files merely because an SSTable has been fsynced.

### Manifest commit point

Manifest fsync is the commit point for compaction.

Before manifest fsync:

```text
Compaction has not committed.
```

After manifest fsync:

```text
Compaction has committed completely.
```

There must not be an ambiguous third state.

### Directory durability

File creation is not durable until the containing directory has been fsynced where required by the specification.

### Tombstones

Do not drop tombstones prematurely.

A tombstone may only be dropped when permitted by the level and snapshot rules specified by `plan.md`.

Dropping a tombstone too early can resurrect deleted data.

### File lifetime

Do not physically delete an SSTable while a held version still references it.

### fsync failure

A failed fsync is unrecoverable.

The database must be closed and require restart according to the project's recovery semantics.

### Checksums

Checksums must be consulted on every applicable read path, including cache hits.

---

## 12. Concurrency

All concurrent code must be safe under:

```bash
go test -race ./...
```

Do not assume concurrency correctness merely because tests pass without `-race`.

When modifying concurrent components:

* Identify ownership of mutable state.
* Identify synchronization boundaries.
* Avoid unnecessary shared mutable state.
* Preserve atomicity requirements.
* Add tests that force concurrent interactions.

For compaction/read interactions, deliberately construct tests that cause overlap rather than relying on scheduling luck.

---

## 13. Testing

Tests are part of the implementation, not an afterthought.

Before considering a task complete:

* Run relevant unit tests.
* Run package tests.
* Run integration tests where applicable.
* Run `go test -race ./...` when appropriate.
* Run project-specific validation commands from the Makefile.
* Check malformed input paths.
* Check failure paths.
* Check recovery paths.

Prefer table-driven tests for:

* Encoding
* Decoding
* Protocol handling
* Boundary conditions
* Malformed inputs

Use property-based/randomized testing where the task has meaningful invariants.

Crash tests must track **acknowledged operations**, not merely operations that were sent.

The reference-model test must remain active from the phase specified by `plan.md`.

The invariant checker must run after compaction in test builds where specified.

Target coverage:

```text
internal/engine       > 70%
internal/compaction   > 70%
```

Do not game coverage numbers. Coverage is useful only when the tests meaningfully exercise behavior.

---

## 14. On-Disk Format

`docs/format.md` is the binding specification for on-disk formats.

When changing an on-disk format:

1. Update the specification first.
2. Update the reader.
3. Update the writer.
4. Update fixtures.
5. Update tests.
6. Verify compatibility requirements.

Someone without access to the source should be able to implement a conformant reader using `docs/format.md`.

Never silently change byte-level formats.

---

## 15. Documentation

### `docs/format.md`

Binding on-disk specification.

### `docs/adr/`

Architecture Decision Records.

Each ADR should contain:

* Context
* Decision
* Alternatives rejected
* Consequences accepted

### `docs/benchmarks.md`

Every benchmark result must include:

* Hardware
* Configuration
* Sync policy
* Exact reproduction command
* Measurement methodology
* At least three runs
* Median
* Variance

### README

The README is a product page.

It should communicate:

* What Strata is
* Why it exists
* Key capabilities
* Relevant measured performance
* Explicit non-goals
* AI assistance disclosure where substantial

Do not invent benchmark numbers.

Every performance figure must trace back to a recorded benchmark.

---

## 16. Benchmarking and Performance

Never invent measurements.

When benchmarking:

1. Establish a baseline.
2. Change one meaningful variable.
3. Measure one thing.
4. Run at least three times.
5. Report median and variance.
6. Record the conditions in `docs/benchmarks.md`.

Do not optimize based solely on intuition.

**Profile before optimizing.**

If profiling fails or produces unexpected behavior:

* Record the failure.
* Investigate plausible causes.
* Avoid inventing conclusions.
* Document ruled-out hypotheses when useful.

---

## 17. Logging

Use the project's single logger interface.

Logging should be structured.

Choose levels that remain useful under load.

Do not add noisy per-operation logging to hot paths unless explicitly required.

Do not log and swallow errors that should be returned to callers.

---

## 18. Repository Hygiene

Before completing a task:

```bash
git status
git diff
```

Check for:

* Accidental files
* Debug output
* Temporary files
* Generated artifacts
* Commented-out experiments
* Unnecessary dependencies
* Untracked files
* Secrets
* Credentials
* Large accidental files

Do not commit:

* API keys
* Tokens
* Passwords
* Private keys
* Local credentials
* Machine-specific secrets

Do not modify unrelated files without a reason.

Keep commits focused and understandable.

---

## 19. Commit Policy

You may create local commits.

Prefer focused commits that correspond to meaningful completed work.

A commit message should communicate:

* What changed
* Why it changed

Do not create meaningless commits merely to checkpoint every small edit.

Never push commits to a remote.

---

## 20. Failure Handling

If a command fails:

1. Read the actual error.
2. Determine whether it is caused by the current change.
3. Investigate before changing unrelated code.
4. Do not hide or suppress failures.
5. Do not declare success while required validation is failing.

If tests expose an existing unrelated failure:

* Report it clearly.
* Do not attribute it to your change without evidence.
* Continue only if doing so is safe and consistent with `plan.md`.

---

## 21. Implementation Philosophy

Prefer the simplest implementation that satisfies the specification.

Do not over-engineer.

Do not add speculative features.

Do not introduce abstractions without a concrete need.

Do not optimize prematurely.

Do not sacrifice correctness for benchmark performance.

When two implementations satisfy the specification, prefer the one that is:

1. Easier to reason about
2. Easier to test
3. Easier to recover after failure
4. Easier to explain in an interview
5. Simpler operationally

---

## 22. Before Editing

Before making the first edit for a task, establish:

```text
Task:
Phase:
Relevant plan.md section:
Done when:
Trap:
Affected packages:
Existing tests:
Expected validation commands:
```

You do not need to ask the user for permission to edit normal project files.

If the task is clearly defined by `plan.md`, proceed autonomously.

Ask the user only when:

* `plan.md` is ambiguous
* Two specifications conflict
* A destructive action is required and cannot be safely inferred
* A new architectural decision is required
* The required behavior cannot be determined from the repository

---

## 23. Before Finishing

For every completed task:

1. Run the relevant tests.
2. Run formatting.
3. Run linting if configured.
4. Run race detection where applicable.
5. Run the task's `Done when` validation.
6. Inspect the final diff.
7. Check repository status.
8. Confirm no secrets or unrelated files were introduced.
9. Update required documentation.
10. Optionally create a local commit.
11. **Never push.**

Final response should summarize:

```text
Implemented:
- ...

Tests:
- ...

Validation:
- ...

Files changed:
- ...

Commit:
- <hash/message>, if created

Remote:
- Nothing pushed.
```

---

## 24. Absolute Rules

These rules override convenience:

1. **Read `plan.md` first.**
2. **`plan.md` is authoritative.**
3. **Do not silently diverge from the specification.**
4. **You may implement every component in the project.**
5. **Tests are required, not optional.**
6. **Do not invent benchmarks.**
7. **Preserve durability invariants.**
8. **Preserve the specified on-disk format.**
9. **Use the permitted dependency set unless an ADR approves a new dependency.**
10. **Keep the code simple and explainable.**
11. **Local Git commits are allowed.**
12. **Pushing to any remote is forbidden.**
13. **Never bypass the no-push rule through another tool or protocol.**
14. **When specification and implementation disagree, stop and raise the discrepancy.**
15. **A task is not done until its `Done when` condition is satisfied.**
