s# T1.1 — the API `resp_test.go` expects

The test suite is written; the implementation is yours (plan.md §4, CLAUDE.md
hand-write list). This file records the exact surface the tests compile
against, so the shape is settled before you start and you are solving the
framing problem rather than guessing at names.

Delete this file once T1.1 is closed — the doc comments on the real code
replace it.

## Types

```go
type Type uint8

const (
    SimpleString Type = iota
    Error
    Integer
    BulkString
    Array
)
```

`Type` needs a `String() string` method: the tests print it in failure
messages via `%v`, and `SimpleString` reads better than `0` at 2 a.m.

```go
// Value is one decoded RESP2 value.
type Value struct {
    Type  Type
    Bytes []byte  // SimpleString, Error, BulkString
    Int   int64   // Integer
    Array []Value // Array
    Null  bool    // the $-1 and *-1 forms
}
```

`Null` is a separate field on purpose. A null bulk string and an empty bulk
string are different values (ADR-003), and `len(Bytes) == 0` cannot tell them
apart.

## Limits

```go
const (
    MaxBulkLength = 64 << 20 // 64 MiB, per docs/format.md §limits
    MaxArrayLength = ...     // your choice; 1 << 20 is ample for a command
)
```

Both are checked **against the length prefix, before allocating**. A reader
that sizes a buffer from an attacker-supplied number and then discovers it is
too large has already lost.

## Errors

```go
// ErrProtocol is the class every malformed-input error wraps, so callers can
// tell "this peer is broken" from "this connection died".
var ErrProtocol = errors.New("protocol error")
```

Wrap with `%w` and include what was wrong — `fmt.Errorf("%w: bulk length %d exceeds maximum %d", ErrProtocol, n, MaxBulkLength)`.

Two cases the connection loop in T1.2 must distinguish:

| Situation | Required behaviour |
|---|---|
| Stream ends cleanly between values | error satisfying `errors.Is(err, io.EOF)` |
| Stream ends inside a value | an error that is **not** `io.EOF` |

The second is why `io.ReadFull` matters: it converts a mid-read EOF into
`io.ErrUnexpectedEOF`, which is precisely the distinction the tests assert.

## Reader

```go
func NewReader(r *bufio.Reader) *Reader
func (r *Reader) ReadValue() (Value, error)

// Buffered reports whether unread bytes are already in the buffer, i.e.
// whether the client pipelined another command behind this one. One line:
// return r.br.Buffered() > 0
func (r *Reader) Buffered() bool
```

`Buffered` is what makes pipelining fast. The connection loop flushes its
replies only when nothing further is buffered, so a pipelined batch of N
commands costs one write syscall instead of N. It is the difference T1.4
measures between the pipelined and unpipelined benchmark rows.

## Writer

```go
func NewWriter(w *bufio.Writer) *Writer
func (w *Writer) WriteSimpleString(s string) error
func (w *Writer) WriteError(s string) error
func (w *Writer) WriteInteger(n int64) error
func (w *Writer) WriteBulkString(b []byte) error  // nil and empty both emit $0\r\n\r\n
func (w *Writer) WriteNullBulkString() error      // emits $-1\r\n
func (w *Writer) WriteArrayHeader(n int) error
func (w *Writer) Flush() error
```

`WriteBulkString(nil)` writes an **empty** bulk string, not a null one. Null is
only ever `WriteNullBulkString`. Keeping the null path on its own method means
a caller cannot produce one by accident from a nil slice — which is the exact
mechanism by which deleted keys start reading back as empty strings.

Writing is header-then-payload with no buffering of its own beyond the
`bufio.Writer`; `Flush` is called once per command batch by the connection
loop, not once per reply, because that is what makes pipelining fast in T1.4.

## Running the tests

```sh
go test ./internal/resp                                   # table-driven
go test ./internal/resp -race                             # what CI runs
go test ./internal/resp -run '^$' -fuzz FuzzReadValue -fuzztime 60s
```

The last one is the *Done when* condition: 60 seconds of fuzzing with no
panic, alongside the 29 malformed inputs in `TestMalformed`.

## The traps, restated

1. **Null vs empty.** plan.md calls this out as the Phase 4 bug you will spend
   a day on. `TestNullAndEmptyBulkAreDistinct` fails if you collapse them.
2. **Allocate after validating, never before.**
3. **`\r\n` is the terminator, not `\n`.** A bare LF is malformed. Reading a
   line with `ReadString('\n')` and not verifying the preceding `\r` accepts
   frames a real Redis rejects.
4. **Bulk payloads are binary safe.** The length prefix is authoritative; a
   `\r\n` inside the payload is data. Never scan for the terminator to find
   the end of a bulk string — read exactly `n` bytes, then verify the two
   bytes that follow.
