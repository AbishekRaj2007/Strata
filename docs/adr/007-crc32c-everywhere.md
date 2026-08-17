# ADR-007: CRC32C on every block and WAL record

**Status:** Accepted

## Context

Storage hardware corrupts data. Bit rot on the platter, firmware bugs, a torn write from a crash mid-flush, a cable fault, a bad DIMM — each produces bytes that differ from what was written, with nothing announcing that it happened.

For a database, the failure mode this creates is the worst one available: returning wrong data confidently. A crash is recoverable and visible. A silently corrupted value propagates into whatever the application does next, and by the time anyone notices, the correct value is long gone.

The choice is not whether to detect corruption but what to spend on detection.

## Decision

CRC32C (Castagnoli) over every SSTable block, every WAL fragment, the bloom block, the index block, and every manifest record. Verified on every read that touches those bytes, block cache hits included.

## Alternatives considered

**No checksums, trusting the filesystem.** Some filesystems — ZFS, btrfs — checksum data themselves. Rejected because it makes correctness depend on deployment configuration, and the common case (ext4, xfs) checksums metadata only, not file contents. An engine that is only correct on ZFS is not a correct engine.

**CRC32 (IEEE).** The more widely known variant. Rejected purely on hardware support: CRC32C has a dedicated instruction on both amd64 (SSE4.2) and arm64, and Go's standard library uses it automatically. Same detection strength, several times the throughput, no additional dependency.

**xxHash.** Faster still, and already a permitted dependency for bloom filtering. Rejected for this purpose because it is a non-cryptographic hash designed for distribution quality, not an error-detecting code. CRC32 has proven guarantees on burst errors — it detects any burst up to 32 bits — which is exactly the shape corruption takes on block storage. Using the right tool matters more than the marginal speed.

**SHA-256 or another cryptographic hash.** Rejected as answering the wrong question. The threat model is hardware failure, not an adversary editing files in place. The cost is orders of magnitude higher for detection strength that hardware faults do not require.

**Checksumming the whole file rather than per block.** Cheaper to compute once. Rejected because verification would require reading the entire file, which is impossible for the intended dataset sizes, and because it could not report *where* the damage is.

## Consequences

**Accepted:** 4 bytes per block and per WAL fragment. Negligible against a 4 KiB block; a rounding error.

**Accepted:** CRC computation on every write and verification on every read, including cache hits. Verifying on cache hits is a deliberate cost — it catches a bit flipped in RAM after the block was cached, which no on-disk checksum scheme would otherwise detect — and it will appear in the CPU profiles in T8.2. If it turns out to dominate, that is a finding worth reporting, not a reason to silently drop the check.

**Accepted:** CRC32C detects corruption but cannot correct it. A block that fails its checksum is unreadable, and the affected data is lost. This is the correct behaviour: reporting the loss loudly is strictly better than serving wrong bytes.

**Accepted:** a checksum over ~32 bits gives a roughly 1-in-4-billion chance that a corrupted block passes. Acceptable for this project; a production engine at extreme scale might want 64 bits.

**Gained:** the corruption sweep in T7.3 becomes meaningful. Flipping any bit in any file produces a clear diagnostic naming the file and offset, rather than a wrong answer or a panic.

**Gained:** the WAL recovery distinction in T2.3 depends on checksums entirely. A trailing fragment that fails its checksum is expected truncation; a mid-file failure with valid records after it is corruption. Without per-fragment checksums that distinction is not expressible, and recovery would have to choose between rejecting healthy databases and silently losing data.
