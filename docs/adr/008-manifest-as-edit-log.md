# ADR-008: Manifest as an append-only edit log

**Status:** Accepted

## Context

The engine must know, at every instant including immediately after a crash, exactly which SSTables are live at which level. A compaction changes that set atomically — four input files stop being live and two output files start, at the same instant — and a crash during the change must leave the database in one of the two valid states, never between them.

This is the same durability problem the WAL solves for user data, applied to the engine's own metadata, and it is the harder of the two because the state being tracked is structured rather than a linear sequence of writes.

## Decision

An append-only log of *edits* rather than snapshots of state. Each record contains one or more edits (ADD_FILE, DELETE_FILE, SET_LOG_NUMBER, SET_NEXT_FILE_NUMBER, SET_LAST_SEQUENCE) and is the atomic unit. The current version is reconstructed by replaying every edit from the beginning. A `CURRENT` file names the active manifest and is replaced atomically by rename.

The in-memory counterpart is an immutable `Version` struct holding the complete per-level file list, published by an atomic pointer swap.

## Alternatives considered

**Rewriting a full state file on every change.** Write the complete file list to `MANIFEST.tmp`, fsync, rename over `MANIFEST`. Genuinely simpler, and atomic via rename. Rejected on cost: with thousands of SSTables the file list is large, and rewriting all of it for a compaction that changed six files means write volume proportional to total file count on every compaction. Compaction is continuous, so that cost is continuous too.

**Storing metadata in the WAL alongside user writes.** One log rather than two. Rejected because the lifecycles are incompatible: a WAL file is deleted once its memtable is flushed, but manifest edits must persist for the lifetime of the database.

**A SQLite database or similar for metadata.** Would provide atomicity and durability off the shelf. Rejected under the dependency policy in plan.md §5 — the atomic commit of a multi-file state change is exactly the interesting problem, and delegating it would remove the part worth building.

**Mutable in-memory state protected by a mutex, instead of immutable versions.** The obvious first design, and wrong in a way worth recording. A reader holding the file list across a compaction sees files deleted underneath it, and locking for the entire duration of a read serialises reads against compaction — which defeats the purpose of background compaction entirely.

## Consequences

**Accepted:** the manifest grows without bound as edits accumulate, and startup replay time grows with it. Mitigation is to periodically write a fresh manifest containing a compacted snapshot of current state and switch `CURRENT` to it. Not implemented in v1; the growth rate is tolerable at project scale, and this is an honest known limitation to state rather than hide.

**Accepted:** replay must be strictly order-sensitive and idempotent in effect. An ADD followed later by a DELETE of the same file leaves it absent, no matter how state is accumulated. Getting this wrong resurrects deleted files into the version, which then reference files no longer on disk.

**Accepted:** the manifest is a single point of failure. Lose it or corrupt it and the SSTables become an unordered pile of files with no level assignment. This is inherent to the design and is why the manifest is checksummed per record (ADR-007) and why T7.3 includes manifest corruption in the sweep.

**Gained — the central property:** the manifest fsync is the commit point for compaction. One instruction before it, the compaction has not happened: all inputs are live, all outputs are orphans on disk that startup will clean up. One instruction after, it has fully happened. There is no third state and no partial visibility. This single property is what makes compaction crash-safe, and it is the thing to be able to state precisely under questioning.

**Gained:** an edit is proportional to what changed, not to total state, so compaction's metadata cost stays constant as the database grows.

**Gained:** the immutable `Version` plus atomic pointer swap means readers never block on compaction and compaction never waits for readers. A reader takes a pointer, holds a reference for the duration of its operation, and is unaffected by any swap that happens meanwhile. Old versions are freed when their last reader releases — the same copy-on-write principle the storage engine itself uses, applied to its own metadata.
