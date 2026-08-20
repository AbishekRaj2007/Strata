# T2.1 / T2.3 — the API the WAL tests expect

The test suites are written; the framing writer and the recovery reader are
yours (plan.md §4, CLAUDE.md hand-write list). This file records the exact
surface the tests compile against, so the shape is settled before you start
and you are solving the framing problem rather than guessing at names.

The binding byte-level specification is `docs/format.md` §2. Where this file
and that one disagree, that one wins — this is an API sketch, not a format
spec.

Delete this file once T2.3 is closed; the doc comments on the real code
replace it.

## What already exists

`internal/wal/policy.go` and `internal/wal/syncer.go` are done (T2.2). The
`Syncer` coordinates fsyncs for any target satisfying:

```go
type Syncable interface {
    Offset() int64
    Sync() error
}
```

Your `*Writer` must satisfy this. That is the entire integration point: once
it does, `NewSyncer(w, policy)` gives you group commit for free, and the crash
harness in `test/crash` starts exercising the real durability path.

## Constants

```go
const (
    BlockSize      = 32768 // §2.1
    HeaderSize     = 7     // crc32c(4) + length(2) + type(1)
    MaxPayloadSize = BlockSize - HeaderSize // 32761
)
```

## Fragment types

```go
type FragmentType uint8

const (
    FragmentFull   FragmentType = 1
    FragmentFirst  FragmentType = 2
    FragmentMiddle FragmentType = 3
    FragmentLast   FragmentType = 4
)
```

Type 0 is deliberately not a valid type: that is what makes zero padding
unambiguous. `FragmentType` needs a `String()` method — the tests print it in
failure messages.

## Batch payload

The reassembled payload of a record is a batch (§2.2). The tests build and
inspect batches through these:

```go
type Kind uint8

const (
    KindSet    Kind = 0
    KindDelete Kind = 1
)

// Record is one key operation within a batch.
type Record struct {
    Kind  Kind
    Key   []byte
    Value []byte // nil and unread when Kind is KindDelete
}

// Batch is one atomically-logged group of records.
type Batch struct {
    Sequence uint64   // sequence of the batch's first record
    Records  []Record
}

// AppendSet and AppendDelete build a batch.
func (b *Batch) AppendSet(key, value []byte)
func (b *Batch) AppendDelete(key []byte)

// Encode appends the §2.2 wire form to dst and returns it.
func (b *Batch) Encode(dst []byte) []byte

// DecodeBatch parses the §2.2 wire form.
func DecodeBatch(payload []byte) (Batch, error)
```

`DecodeBatch` must reject trailing bytes after `record_count` records, and a
payload that ends early. Both are corruption, not truncation — the checksum
already proved the bytes arrived intact.

## Writer

```go
func NewWriter(f *os.File) *Writer

// Write frames one batch into the file, fragmenting across blocks as needed.
// It returns the file offset immediately after the last byte written, which
// is the value to hand to Syncer.AwaitDurable.
func (w *Writer) Write(b *Batch) (endOffset int64, err error)

func (w *Writer) Offset() int64 // Syncable
func (w *Writer) Sync() error   // Syncable
func (w *Writer) Close() error
```

`Write` must not return an offset that the caller could mistake for durable:
the offset means "these bytes are with the OS", and durability is the
`Syncer`'s business.

## Reader

```go
func NewReader(f *os.File) *Reader

// Next returns the next batch. At a clean end of log it returns io.EOF.
func (r *Reader) Next() (Batch, error)
```

## Errors

```go
// ErrCorrupt is the class every real-damage error wraps. It is distinct from
// a clean end of log, which is io.EOF, and that distinction is the whole of
// §2.3.
var ErrCorrupt = errors.New("wal corruption")
```

The taxonomy in §2.3 is a correctness requirement, not a stylistic one:

| Situation | Required result |
|---|---|
| Truncated header at the tail | `io.EOF` |
| Declared length runs past EOF, at the tail | `io.EOF` |
| Checksum failure in the final fragment | `io.EOF` |
| FIRST or MIDDLE with no follower, at the tail | `io.EOF` |
| Checksum failure with valid records after it | wraps `ErrCorrupt` |
| Invalid fragment type | wraps `ErrCorrupt` |
| Orphaned MIDDLE or LAST | wraps `ErrCorrupt` |
| Malformed batch payload behind a valid checksum | wraps `ErrCorrupt` |

The hard part is that "at the tail" is not knowable when you read the
fragment — you discover it only by finding nothing valid afterwards. The
every-offset truncation test exists to force you to get this right.

## Recovery

```go
// Recover replays every WAL file in dir in ascending file-number order,
// calling apply for each record in order. It returns the highest sequence
// number observed, which restores the engine's sequence counter.
func Recover(dir string, apply func(seq uint64, rec Record) error) (highest uint64, err error)
```

A failure in any file that is not the highest-numbered one is corruption, per
§2.3 — an earlier file cannot legitimately be truncated, because a later file
only exists if the earlier one was completed.

## Running the tests

```sh
go test ./internal/wal                    # table-driven and round-trip
go test ./internal/wal -race              # what CI runs
go test ./internal/wal -run TestTruncateAtEveryOffset -v
```

`make wal-fuzz` runs the framing fuzz target for 60s.

## The traps, restated

1. **A header straddling a block boundary.** With fewer than 7 bytes left, pad
   with zeros and start the next block. The tests construct a batch whose
   header lands at exactly `BlockSize-6` through `BlockSize-1`.
2. **The checksum covers type and payload, not the CRC or length fields.**
   Getting the covered range wrong still round-trips through your own reader
   and fails against the hex dump in §7.
3. **Truncation versus corruption.** Conflating them either rejects healthy
   databases or silently discards acknowledged writes.
4. **A DELETE record has no value field at all** — not a zero-length one.
   Branch on `kind` before reading further.
