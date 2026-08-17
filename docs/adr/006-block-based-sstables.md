# ADR-006: Block-based SSTables, 4 KiB blocks

**Status:** Accepted

## Context

An SSTable holds far more data than fits in memory, so a lookup must read a bounded portion of the file rather than the whole thing. The file needs an internal granularity: the unit that gets read from disk, checksummed, and cached.

That unit is a single choice with three separate consequences, which is what makes it worth an ADR. Too small and per-unit overhead and index size dominate. Too large and every lookup drags in data it does not need, wasting both bandwidth and cache.

## Decision

Block-based layout with a ~4 KiB target block size, prefix-compressed entries, restart points every 16 entries, and a per-block CRC32C. The block is simultaneously the unit of disk read, of checksum verification, and of cache residency.

## Alternatives considered

**One entry per read, via an index of every key.** Precise — read exactly the bytes needed. Rejected because an index entry per key would not fit in memory for a dataset larger than RAM, which is the entire point of the project. It also forfeits locality: a scan would issue one read per key rather than one per block.

**Larger blocks, 64 KiB.** Fewer index entries, better compression ratio, better sequential scan throughput. Rejected as a default because a point lookup for a 100-byte value would read 64 KiB, wasting 99.8% of the transfer and evicting 16× more useful data from the cache per read. T5.3 sweeps 1/4/16/64 KiB and reports what actually happens rather than resting on this reasoning.

**Smaller blocks, 512 bytes.** Minimal read amplification per lookup. Rejected: below the 4 KiB page size the OS reads a full page regardless, so the saving is imaginary, while index size grows 8× and prefix compression loses most of its benefit as each block restarts.

**Mmap the whole file.** Simple, and lets the OS manage paging. Rejected because page faults are invisible to profiling, making Phase 8 substantially harder, and because an I/O error becomes a `SIGBUS` rather than a returned error — which is incompatible with the fault-injection layer in T7.2 and with reporting corruption cleanly.

## Consequences

**Accepted:** a point lookup reads a full 4 KiB block to return one entry. For small values that is real read amplification, and it is the cost that buys locality on scans and a bounded index.

**Accepted:** prefix compression and restart points are in direct tension. Sharing prefixes saves space; restart points cost space but make a block binary-searchable. The 16-entry interval is a tunable, and being able to explain what happens at 1 (no compression, maximum searchability) and at 1000 (maximum compression, linear scan) is the point of having it configurable.

**Accepted:** the first entry after every restart point must store its key in full. Miss this and sequential decoding still works while seeks return wrong keys — a bug that hides until Phase 4 and looks like a read-path problem rather than an encoding one.

**Gained:** checksum granularity matches read granularity, so corruption is detected on exactly the bytes read, and the reported error names a specific block offset.

**Gained:** cache granularity matches read granularity too, so caching blocks rather than key-value pairs gives locality for free — reading one key warms its neighbours, which is what makes the Zipfian hit rate in T5.2 achievable.

**Gained:** 4 KiB matches the page size, so a block read is one page-aligned I/O rather than a straddling read of two pages.
