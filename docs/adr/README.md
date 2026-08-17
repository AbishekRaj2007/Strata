# Architecture decision records

One file per decision, named `NNN-short-title.md`. Each records the context that forced a choice, the decision, the alternatives genuinely considered, and the consequences accepted.

The consequences section is the one that matters. A decision recorded without a cost is a decision that was never really made.

| ADR | Decision | Status |
|---|---|---|
| [001](001-go-not-rust.md) | Go, not Rust | Accepted |
| [002](002-leveled-not-size-tiered.md) | Leveled compaction, not size-tiered | Accepted |
| [003](003-resp2-not-resp3.md) | RESP2, not RESP3 | Accepted |
| [004](004-sequence-number-per-write.md) | Sequence number on every write | Accepted |
| [005](005-skip-list-memtable.md) | Skip list memtable | Accepted |
| [006](006-block-based-sstables.md) | Block-based SSTables, 4 KiB blocks | Accepted |
| [007](007-crc32c-everywhere.md) | CRC32C on every block and WAL record | Accepted |
| [008](008-manifest-as-edit-log.md) | Manifest as an append-only edit log | Accepted |
