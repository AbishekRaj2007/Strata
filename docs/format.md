# Strata on-disk format specification

**Format version: 1.** This document is binding. The reader, the writer, and every test fixture conform to it; where code and this document disagree, this document is correct and the code is a bug.

A reader implemented from this document alone, with no access to the Strata source, must interpret any Strata data directory correctly.

## 0. Conventions

**Endianness.** Little-endian for every fixed-width integer, without exception. This matches the amd64 and arm64 targets and avoids per-field byte swapping.

**Varints.** Fields marked `uvarint` use unsigned LEB128, identical to Go's `encoding/binary.PutUvarint`: seven payload bits per byte, little-endian group order, high bit set on every byte except the last. A uvarint is at most 10 bytes. A uvarint whose encoding exceeds 10 bytes, or which is not minimally encoded, is malformed.

**Checksums.** CRC32C (Castagnoli polynomial, `0x1EDC6F41`), the same variant as `hash/crc32.Castagnoli`. Stored little-endian. The bytes each checksum covers are stated per structure and are never assumed.

**Byte strings.** Keys and values are arbitrary byte strings. They are never interpreted as text, never NUL-terminated, and may contain any byte including `0x00`. The empty key and the empty value are both legal.

**Reserved fields.** A field marked reserved is written as zero and ignored on read. Readers must not reject a non-zero reserved field, so those bytes stay available for later use.

### 0.1 Size limits

These limits are fixed as of format version 1 and enforced at the protocol boundary, before any allocation:

| Limit | Value | Rationale |
|---|---|---|
| Maximum key size | 65,536 bytes (64 KiB) | Two keys plus block overhead must fit comfortably in a 4 KiB target block without every block becoming a single-entry outlier |
| Maximum value size | 67,108,864 bytes (64 MiB) | Bounds the memtable spike from one write and keeps a single value well under the memtable threshold |
| Maximum entries per write batch | 65,536 | Bounds WAL record reassembly memory |
| Maximum RESP bulk string | 67,108,864 bytes (64 MiB) | Matches the value limit; rejected before allocation, never after |

A key or value exceeding its limit is rejected with a protocol error and never reaches the WAL. A stored key or value exceeding its limit on read indicates corruption and is reported as such.

## 1. Directory layout

```
data/
├── CURRENT              # text file naming the active manifest
├── MANIFEST-000001      # append-only log of version edits
├── 000004.wal           # WAL for the active memtable
├── 000007.wal           # WAL for an immutable memtable not yet flushed
├── 000002.sst           # SSTables, monotonically numbered
├── 000003.sst
└── LOCK                 # advisory lock preventing two processes on one directory
```

**File numbers** are a single monotonic sequence shared across all file types, allocated from a counter persisted in the manifest. A file number is never reused within the lifetime of a database, so `000004.wal` and `000004.sst` never both exist. Numbers are formatted as decimal, zero-padded to six digits, and widen naturally beyond 999999 rather than wrapping.

**CURRENT** contains the active manifest's filename followed by a single `\n`, and nothing else. It is written atomically: write `CURRENT.tmp`, fsync it, rename over `CURRENT`, then fsync the directory. A `CURRENT` that is empty, lacks a trailing newline, or names a file that does not exist is corruption.

**LOCK** is an empty file held with an exclusive `flock`. Its contents carry no meaning. Failure to acquire it means another process has the directory open and startup must abort.

## 2. WAL

### 2.1 Block framing

A WAL file is a sequence of 32,768-byte (32 KiB) blocks. The final block may be short. Records are packed into blocks and fragmented across block boundaries when necessary, so that a torn write damages at most one block and recovery can locate exactly where damage begins.

Each fragment carries a 7-byte header:

```
┌─────────┬──────────┬────────┬──────────────────┐
│ CRC32C  │ Length   │ Type   │ Payload          │
│ 4 bytes │ 2 bytes  │ 1 byte │ Length bytes     │
└─────────┴──────────┴────────┴──────────────────┘

offset 0  u32  crc32c     checksum over Type and Payload, in that order
offset 4  u16  length     payload length in bytes, not including this header
offset 6  u8   type       1 = FULL, 2 = FIRST, 3 = MIDDLE, 4 = LAST
offset 7  []byte payload  length bytes
```

The checksum covers the type byte and the payload, and does **not** cover the CRC or length fields themselves.

Because a header is 7 bytes, payload length never exceeds `32768 - 7 = 32761` bytes.

**Padding.** If fewer than 7 bytes remain in a block after writing a fragment, the writer fills the remainder with zero bytes and starts the next fragment in the next block. A reader that encounters fewer than 7 bytes remaining in a block skips to the next block boundary without interpreting those bytes. Zero padding is never a valid header, because type 0 is not a valid type.

**Fragment sequences.** A record is either a single FULL fragment, or a FIRST fragment followed by zero or more MIDDLE fragments followed by a LAST fragment. Any other sequence is malformed:

| Observed | Verdict |
|---|---|
| FULL after FIRST with no LAST | malformed: truncated record |
| MIDDLE or LAST without a preceding FIRST | malformed: orphaned fragment |
| FIRST immediately after FIRST | malformed: truncated record |
| type 0, or type above 4 | malformed: invalid type |

### 2.2 Record payload

The payload of a reassembled record is a write batch. Batching is the payload format from the first version so that multi-key writes are a compatible addition later.

```
u64      sequence_number      sequence of the batch's first record
u32      record_count         number of records that follow
  repeated record_count times:
    u8       kind             0 = SET, 1 = DELETE
    uvarint  key_len
    []byte   key              key_len bytes
    uvarint  value_len        present only when kind = SET
    []byte   value            value_len bytes, present only when kind = SET
```

The *i*th record in a batch has sequence number `sequence_number + i`. A DELETE record carries no value field at all — not a zero-length one — so a reader must branch on `kind` before reading further.

`record_count` of zero is legal and encodes an empty batch, which recovery skips.

A payload with trailing bytes after `record_count` records have been decoded is malformed. A payload that ends before `record_count` records are decoded is malformed.

### 2.3 Recovery

Recovery replays WAL files in ascending file-number order, and within a file, in offset order. It restores the sequence counter to the highest sequence number observed across all replayed records.

The distinction below is a correctness requirement, not an optimisation. Conflating these two cases means either rejecting healthy databases or silently discarding acknowledged writes.

**Clean end of log.** Any of the following, occurring at the *tail* of the last WAL file with no valid records after it, is the expected result of a crash mid-write. Recovery accepts every record before that point and stops:

- a truncated fragment header (fewer than 7 bytes remaining)
- a fragment whose declared length runs past the end of the file
- a checksum failure in the final fragment
- a FIRST or MIDDLE fragment with no following fragment

**Corruption.** Any of the following indicates real damage and is reported as an error rather than silently skipped:

- a checksum failure with at least one valid record following it
- an invalid fragment type
- an orphaned MIDDLE or LAST fragment
- a malformed batch payload in a fragment that passed its checksum
- any failure in a WAL file that is not the highest-numbered one being replayed

A checksum failure that passes the framing check but fails the payload decode is corruption, not truncation: the checksum proves the bytes arrived intact, so a malformed payload means the writer produced something invalid.

## 3. SSTable

```
┌────────────────────────────────────┐
│  Data block 0        (~4 KiB)      │
│  Data block 1                      │
│  ...                               │
│  Data block N                      │
├────────────────────────────────────┤
│  Bloom filter block                │
├────────────────────────────────────┤
│  Index block                       │
├────────────────────────────────────┤
│  Footer               (48 bytes)   │
└────────────────────────────────────┘
```

Sections are written in the order shown. Offsets are recorded as each section is completed, never patched afterwards.

### 3.1 Entry ordering

Every entry in an SSTable is ordered by the comparator used everywhere in Strata:

```
(user_key ascending, sequence_number descending)
```

Keys compare as unsigned byte strings, shortest-first on a shared prefix — identical to Go's `bytes.Compare`. Sequence descending means the newest version of a key sorts first, so a scan meeting a key for the first time has found its current value.

This exact comparator is used by the memtable, the block builder, the merge iterator, and compaction. A second, subtly different comparator anywhere is a bug.

### 3.2 Data block

An entry:

```
uvarint  shared_prefix_len    bytes shared with the previous key in this block
uvarint  unshared_len         bytes of key that follow
uvarint  value_len            length of the value
u8       kind                 0 = SET, 1 = DELETE
u64      sequence
[]byte   unshared_key_bytes   unshared_len bytes
[]byte   value_bytes          value_len bytes
```

The full key is the first `shared_prefix_len` bytes of the previous entry's key, followed by `unshared_key_bytes`.

A DELETE entry carries `value_len = 0` and no value bytes. Unlike the WAL, the `value_len` field is always present here, because the field ordering is fixed to keep decoding branch-free.

The block trailer:

```
u32[]  restart_offsets      restart_count entries, byte offsets from block start
u32    restart_count
u8     compression_type     0 = none, 1 = snappy (reserved, not written by v1)
u32    crc32c
```

The checksum covers every byte of the block from offset 0 up to and including the `compression_type` byte — that is, all entries plus the restart array plus the count plus the compression byte. It does not cover itself.

**Restart points.** Every 16th entry (indices 0, 16, 32, …) is a restart point and stores its key in full, with `shared_prefix_len = 0`. The restart interval is a build-time tunable; readers must use `restart_count` and the stored offsets rather than assuming 16. The first entry of a block is always a restart point, so a block is always decodable from its own bytes without reference to any other block.

Seeking within a block binary-searches the restart array to find the last restart point whose key is `<= target`, then scans forward from there.

**Size.** Blocks target 4,096 bytes, closed once adding another entry would exceed the target. A single entry larger than the target forms a block on its own, so a large value never forces truncation.

### 3.3 Bloom filter block

```
u32     bits_per_key
u32     num_probes           number of hash probes, k
u32     bit_array_len        length of the bit array in bytes
[]byte  bit_array            bit_array_len bytes
u32     crc32c               covers every preceding byte of this block
```

Bit *i* of the array is byte `i / 8`, bit `i % 8` counting from the least significant. Probes derive from a single 64-bit xxHash of the user key by double hashing: `h1 = uint32(h)`, `h2 = uint32(h >> 32)`, and probe *i* addresses bit `(h1 + i*h2) % (bit_array_len * 8)` for *i* in `[0, num_probes)`.

`h1 + i*h2` is computed in 32-bit arithmetic and wraps on overflow — that is, the sum is taken modulo 2³² before the modulo by the bit count. A reader that widened the accumulator to 64 bits would address different bits and report false negatives against filters this writer produced, so the width is normative, not incidental.

The hash is xxHash64 with seed 0 (the `XXH64` of the reference implementation), over the user key bytes exactly.

Keys are added by user key only, with the sequence number excluded, so all versions of a key share one filter entry.

`bits_per_key` and `num_probes` are recorded for diagnostics and for the reader to report; a reader must take the bit count from `bit_array_len` rather than recomputing it from `bits_per_key` and a key count, since the writer floors `bit_array_len` at 8 bytes so that very small tables still filter usefully.

A table with no keys writes `bit_array_len = 0` and every lookup reports absent.

### 3.4 Index block

One entry per data block, in the same order the data blocks appear:

```
uvarint  key_len
[]byte   largest_key          the largest user key in the block this entry describes
u64      block_offset         byte offset of the block from the start of the file
u32      block_length         length of the block in bytes, including its trailer
```

followed by:

```
u32  entry_count
u32  crc32c                   covers every preceding byte of this block
```

Index keys are stored in full, without prefix compression, because the index is loaded once at table open and searched repeatedly.

To locate a key, binary-search for the first index entry whose `largest_key` is `>= target`. If none exists, the key is not in this table.

### 3.5 Footer

Exactly 48 bytes at the end of the file, so it reads in a single seek from the end.

```
offset  0  u64  bloom_offset
offset  8  u64  bloom_length
offset 16  u64  index_offset
offset 24  u64  index_length
offset 32  u32  format_version    1
offset 36  u32  reserved          written as zero, ignored on read
offset 40  u64  magic             0x0053545241544100
```

The magic number is the ASCII bytes `STRATA` framed by NUL bytes, stored little-endian, so the last eight bytes of any valid SSTable read as `00 41 54 41 52 54 53 00`.

A file whose final 8 bytes are not the magic is not a Strata SSTable and is rejected before any other field is read. A file shorter than 48 bytes is rejected for the same reason. A recognised magic with an unknown `format_version` is rejected with a version-mismatch error rather than a parse error, so the diagnostic is useful.

## 4. Manifest

The manifest is an append-only log of *edits*. It reuses the WAL block framing from §2.1 exactly — same 32 KiB blocks, same 7-byte fragment header, same CRC32C, same padding rule — so one framing implementation serves both.

Each record's payload is one version edit:

```
u32  edit_type
```

followed by that type's fields:

| edit_type | Name | Fields |
|---|---|---|
| 1 | ADD_FILE | see below |
| 2 | DELETE_FILE | `u32 level`, `u64 file_number` |
| 3 | SET_LOG_NUMBER | `u64 log_number` |
| 4 | SET_NEXT_FILE_NUMBER | `u64 next_file_number` |
| 5 | SET_LAST_SEQUENCE | `u64 last_sequence` |

ADD_FILE:

```
u32      level
u64      file_number
u64      file_size
uvarint  smallest_key_len
[]byte   smallest_key
uvarint  largest_key_len
[]byte   largest_key
u64      smallest_seq
u64      largest_seq
```

An unknown `edit_type` is corruption, not a compatibility case — this format has no forward-compatibility provision in version 1.

### 4.1 Atomicity

A single manifest record may contain multiple edits, and the record is the atomic unit. A compaction replacing four inputs with two outputs writes one record containing four DELETE_FILE edits and two ADD_FILE edits, then fsyncs.

**That fsync is the commit point.** The system has exactly two valid states with respect to any compaction: crash before the fsync and the compaction never happened, leaving all inputs live and all outputs orphaned; crash after and it fully happened. There is no third state, and no reader ever observes a partial edit.

Orphaned outputs — `.sst` files present on disk but absent from the replayed manifest — are deleted at startup.

### 4.2 Replay

Replay begins from the empty version and applies every edit in the order it appears. Application is order-sensitive: an ADD_FILE followed later by a DELETE_FILE of the same file number leaves the file absent, regardless of how the reader accumulates state. A DELETE_FILE naming a file not currently present is corruption.

A truncated trailing record — the same conditions §2.3 treats as clean end of log — means the last edit did not commit. Replay accepts everything before it and treats the resulting version as current. A checksum failure with valid records following it is corruption.

## 5. Worked example

A complete data block containing three entries, with a restart interval of 16 so all three sit in one restart interval:

| # | key | value | kind | sequence |
|---|---|---|---|---|
| 0 | `apple` | `red` | SET | 0 |
| 1 | `apricot` | `orange` | SET | 2 |
| 2 | `banana` | `yellow` | SET | 3 |

The encoded block is 80 bytes:

```
00000000  00 05 03 00 00 00 00 00  00 00 00 00 61 70 70 6c  |............appl|
00000010  65 72 65 64 02 05 06 00  02 00 00 00 00 00 00 00  |ered............|
00000020  72 69 63 6f 74 6f 72 61  6e 67 65 00 06 06 00 03  |ricotorange.....|
00000030  00 00 00 00 00 00 00 62  61 6e 61 6e 61 79 65 6c  |.......bananayel|
00000040  6c 6f 77 00 00 00 00 01  00 00 00 00 1b 4b 29 21  |low..........K)!|
```

Read field by field:

**Entry 0, offset 0x00, 20 bytes** — `00 05 03 00 0000000000000000 "apple" "red"`

| Bytes | Field | Value |
|---|---|---|
| `00` | shared_prefix_len | 0 — first entry in the block, a restart point |
| `05` | unshared_len | 5 |
| `03` | value_len | 3 |
| `00` | kind | SET |
| `00 00 00 00 00 00 00 00` | sequence | 0 |
| `61 70 70 6c 65` | unshared_key | `apple` |
| `72 65 64` | value | `red` |

**Entry 1, offset 0x14, 23 bytes** — `02 05 06 00 0200000000000000 "ricot" "orange"`

| Bytes | Field | Value |
|---|---|---|
| `02` | shared_prefix_len | 2 — shares `ap` with `apple` |
| `05` | unshared_len | 5 |
| `06` | value_len | 6 |
| `00` | kind | SET |
| `02 00 00 00 00 00 00 00` | sequence | 2 |
| `72 69 63 6f 74` | unshared_key | `ricot`, giving `ap` + `ricot` = `apricot` |
| `6f 72 61 6e 67 65` | value | `orange` |

**Entry 2, offset 0x2b, 24 bytes** — `00 06 06 00 0300000000000000 "banana" "yellow"`

| Bytes | Field | Value |
|---|---|---|
| `00` | shared_prefix_len | 0 — shares nothing with `apricot` |
| `06` | unshared_len | 6 |
| `06` | value_len | 6 |
| `00` | kind | SET |
| `03 00 00 00 00 00 00 00` | sequence | 3 |
| `62 61 6e 61 6e 61` | unshared_key | `banana` |
| `79 65 6c 6c 6f 77` | value | `yellow` |

**Trailer, offset 0x43, 13 bytes**

| Bytes | Field | Value |
|---|---|---|
| `00 00 00 00` | restart_offsets[0] | 0 |
| `01 00 00 00` | restart_count | 1 |
| `00` | compression_type | none |
| `1b 4b 29 21` | crc32c | `0x21294b1b`, over bytes `0x00`–`0x4b` inclusive |

Note that entry 2 shares no prefix with entry 1 despite not being a restart point: prefix sharing is computed against the immediately preceding key, and `apricot` and `banana` share nothing.

## 6. Malformed input behaviour

Every case below produces a clear diagnostic error naming the file and offset. None produces a panic, and none produces a wrong answer.

| Input | Behaviour |
|---|---|
| SSTable shorter than 48 bytes | rejected: not an SSTable |
| SSTable with wrong magic | rejected: not an SSTable |
| SSTable with unknown format_version | rejected: version mismatch |
| Block checksum mismatch | rejected: corruption at that block, naming file and offset |
| Index or bloom checksum mismatch | rejected: table unusable |
| Block offset or length past end of file | rejected: corruption |
| Entry whose unshared_len runs past block end | rejected: corruption |
| Entry with shared_prefix_len exceeding the previous key length | rejected: corruption |
| Non-minimal or over-long uvarint | rejected: corruption |
| Key or value exceeding §0.1 limits on read | rejected: corruption |
| WAL trailing truncation | accepted as clean end of log |
| WAL mid-file checksum failure | rejected: corruption |
| WAL invalid fragment type | rejected: corruption |
| Manifest trailing truncation | accepted; last edit did not commit |
| Manifest unknown edit_type | rejected: corruption |
| Manifest DELETE_FILE of an absent file | rejected: corruption |
| CURRENT empty or naming a missing file | rejected: corruption |
| LOCK held by another process | startup aborts |

Checksums are verified on **every** read path that touches a block's bytes, including reads served from the block cache, so a bit flipped in memory after caching is caught rather than served.
