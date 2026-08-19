package resp

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strconv"
)

// Type identifies which of the five RESP2 value forms a Value holds.
type Type uint8

// The five RESP2 types, in the order the specification introduces them.
const (
	SimpleString Type = iota
	Error
	Integer
	BulkString
	Array
)

// String renders the type name, so test failures and log lines read as
// "SimpleString" rather than "0".
func (t Type) String() string {
	switch t {
	case SimpleString:
		return "SimpleString"
	case Error:
		return "Error"
	case Integer:
		return "Integer"
	case BulkString:
		return "BulkString"
	case Array:
		return "Array"
	default:
		return fmt.Sprintf("Type(%d)", uint8(t))
	}
}

// Value is one decoded RESP2 value.
//
// Null is carried separately from Bytes because a null bulk string and an
// empty one are different values (ADR-003): len(Bytes) == 0 holds for both,
// so it cannot distinguish "key absent" from "key holds a zero-length value".
type Value struct {
	Type  Type
	Bytes []byte  // SimpleString, Error, BulkString
	Int   int64   // Integer
	Array []Value // Array
	Null  bool    // the $-1 and *-1 forms
}

// Protocol limits. Both are checked against the length prefix before anything
// is sized from it: a reader that allocates from an attacker-supplied number
// and then discovers it was too large has already lost.
const (
	// MaxBulkLength caps a single bulk string payload, per docs/format.md.
	MaxBulkLength = 64 << 20 // 64 MiB

	// MaxArrayLength caps element count. A command array is a verb plus its
	// arguments, so a million elements is already far past anything legitimate.
	MaxArrayLength = 1 << 20
)

// ErrProtocol is the class every malformed-input error wraps, so callers can
// tell "this peer is broken" from "this connection died".
var ErrProtocol = errors.New("protocol error")

// Reader decodes RESP2 values from a buffered stream.
type Reader struct {
	br *bufio.Reader
}

// NewReader returns a Reader decoding from br.
func NewReader(br *bufio.Reader) *Reader {
	return &Reader{br: br}
}

// Buffered reports whether unread bytes are already in the buffer, i.e.
// whether the client pipelined another command behind this one. The connection
// loop uses it to defer its flush until nothing further is buffered, turning a
// pipelined batch of N commands into one write syscall instead of N.
func (r *Reader) Buffered() bool {
	return r.br.Buffered() > 0
}

// ReadValue decodes the next value from the stream.
//
// A stream that ends cleanly between values returns io.EOF; one that ends
// inside a value returns io.ErrUnexpectedEOF. The connection loop depends on
// telling those apart, since the first is a normal client disconnect and the
// second is a broken peer.
func (r *Reader) ReadValue() (Value, error) {
	prefix, err := r.br.ReadByte()
	if err != nil {
		// Only here is EOF clean: no bytes of a value have been consumed yet.
		return Value{}, err
	}

	switch prefix {
	case '+':
		return r.readSimple(SimpleString)
	case '-':
		return r.readSimple(Error)
	case ':':
		return r.readInteger()
	case '$':
		return r.readBulk()
	case '*':
		return r.readArray()
	default:
		return Value{}, fmt.Errorf("%w: unknown type byte %q", ErrProtocol, prefix)
	}
}

// readLine reads through the next CRLF and returns the payload before it.
//
// The terminator is CRLF, never a bare LF: accepting "+OK\n" would admit
// frames that a real Redis rejects, so the CR is verified rather than assumed.
func (r *Reader) readLine() ([]byte, error) {
	line, err := r.br.ReadBytes('\n')
	if err != nil {
		// A line that never terminates is a truncated value, not a clean close,
		// even when the underlying stream reports io.EOF.
		if errors.Is(err, io.EOF) {
			return nil, io.ErrUnexpectedEOF
		}
		return nil, err
	}
	if len(line) < 2 || line[len(line)-2] != '\r' {
		return nil, fmt.Errorf("%w: line not terminated by CRLF", ErrProtocol)
	}
	return line[:len(line)-2], nil
}

func (r *Reader) readSimple(t Type) (Value, error) {
	line, err := r.readLine()
	if err != nil {
		return Value{}, err
	}
	// A zero-length payload is legal here ("+\r\n"), but Bytes must stay
	// non-nil so call sites can index it without a nil check.
	return Value{Type: t, Bytes: append([]byte{}, line...)}, nil
}

func (r *Reader) readInteger() (Value, error) {
	line, err := r.readLine()
	if err != nil {
		return Value{}, err
	}
	n, err := parseInt(line)
	if err != nil {
		return Value{}, err
	}
	return Value{Type: Integer, Int: n}, nil
}

// parseInt converts a numeric field, rejecting anything ParseInt would not
// accept whole: an empty field, trailing garbage, a lone sign, or a value
// outside int64. Hand-rolled digit loops silently accept most of these.
func parseInt(field []byte) (int64, error) {
	if len(field) == 0 {
		return 0, fmt.Errorf("%w: empty numeric field", ErrProtocol)
	}
	n, err := strconv.ParseInt(string(field), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%w: bad numeric field %q: %v", ErrProtocol, field, err)
	}
	return n, nil
}

func (r *Reader) readBulk() (Value, error) {
	line, err := r.readLine()
	if err != nil {
		return Value{}, err
	}
	n, err := parseInt(line)
	if err != nil {
		return Value{}, err
	}

	if n < 0 {
		if n != -1 {
			return Value{}, fmt.Errorf("%w: bulk length %d is negative and not -1", ErrProtocol, n)
		}
		return Value{Type: BulkString, Null: true}, nil
	}
	// Checked before the make below, which is the whole point of the limit.
	if n > MaxBulkLength {
		return Value{}, fmt.Errorf("%w: bulk length %d exceeds maximum %d", ErrProtocol, n, MaxBulkLength)
	}

	// Read exactly n bytes and then verify the terminator. Scanning for CRLF
	// would corrupt any payload containing one, and the payload is binary safe.
	buf := make([]byte, n)
	if _, err := io.ReadFull(r.br, buf); err != nil {
		return Value{}, unexpectedEOF(err)
	}

	var crlf [2]byte
	if _, err := io.ReadFull(r.br, crlf[:]); err != nil {
		return Value{}, unexpectedEOF(err)
	}
	if crlf[0] != '\r' || crlf[1] != '\n' {
		return Value{}, fmt.Errorf("%w: bulk payload not terminated by CRLF", ErrProtocol)
	}

	return Value{Type: BulkString, Bytes: buf}, nil
}

func (r *Reader) readArray() (Value, error) {
	line, err := r.readLine()
	if err != nil {
		return Value{}, err
	}
	n, err := parseInt(line)
	if err != nil {
		return Value{}, err
	}

	if n < 0 {
		if n != -1 {
			return Value{}, fmt.Errorf("%w: array length %d is negative and not -1", ErrProtocol, n)
		}
		return Value{Type: Array, Null: true}, nil
	}
	if n > MaxArrayLength {
		return Value{}, fmt.Errorf("%w: array length %d exceeds maximum %d", ErrProtocol, n, MaxArrayLength)
	}

	// The length is bounded above, but it is still peer-supplied: elements are
	// appended as they arrive so a large claim backed by no data costs nothing.
	elems := make([]Value, 0, initialCap(n))
	for i := int64(0); i < n; i++ {
		v, err := r.ReadValue()
		if err != nil {
			// Mid-array, even a clean stream end means the value was truncated.
			return Value{}, unexpectedEOF(err)
		}
		elems = append(elems, v)
	}
	return Value{Type: Array, Array: elems}, nil
}

// initialCap bounds the eager allocation for an array whose declared length is
// large, so "*1048576\r\n" with nothing behind it does not reserve for a
// million elements before the first read fails.
func initialCap(n int64) int64 {
	const cap = 64
	if n < cap {
		return n
	}
	return cap
}

// unexpectedEOF reclassifies a clean stream end that occurs partway through a
// value. Only ReadValue's very first byte may legitimately report io.EOF.
func unexpectedEOF(err error) error {
	if errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}

// Writer encodes RESP2 replies onto a buffered stream.
//
// Writes go straight through to the bufio.Writer with no buffering of their
// own; the connection loop calls Flush once per command batch rather than per
// reply, which is what makes pipelining cheap.
type Writer struct {
	bw *bufio.Writer

	// scratch formats length and integer fields without allocating per reply.
	scratch []byte
}

// NewWriter returns a Writer encoding onto bw.
func NewWriter(bw *bufio.Writer) *Writer {
	return &Writer{bw: bw, scratch: make([]byte, 0, 32)}
}

// WriteSimpleString writes s as a simple string.
func (w *Writer) WriteSimpleString(s string) error {
	return w.writeLine('+', s)
}

// WriteError writes s as an error reply.
func (w *Writer) WriteError(s string) error {
	return w.writeLine('-', s)
}

// WriteInteger writes n as an integer reply.
func (w *Writer) WriteInteger(n int64) error {
	if err := w.bw.WriteByte(':'); err != nil {
		return err
	}
	if err := w.writeNumber(n); err != nil {
		return err
	}
	return w.writeCRLF()
}

// WriteBulkString writes b as a bulk string. A nil slice writes an *empty*
// bulk string, never a null one — null is reachable only through
// WriteNullBulkString, so a nil value cannot silently become "key not found".
func (w *Writer) WriteBulkString(b []byte) error {
	if err := w.bw.WriteByte('$'); err != nil {
		return err
	}
	if err := w.writeNumber(int64(len(b))); err != nil {
		return err
	}
	if err := w.writeCRLF(); err != nil {
		return err
	}
	if _, err := w.bw.Write(b); err != nil {
		return err
	}
	return w.writeCRLF()
}

// WriteNullBulkString writes the null bulk string, $-1, which is how a missing
// key is reported.
func (w *Writer) WriteNullBulkString() error {
	_, err := w.bw.WriteString("$-1\r\n")
	return err
}

// WriteArrayHeader writes an array header declaring n elements. The caller
// writes the elements that follow.
func (w *Writer) WriteArrayHeader(n int) error {
	if err := w.bw.WriteByte('*'); err != nil {
		return err
	}
	if err := w.writeNumber(int64(n)); err != nil {
		return err
	}
	return w.writeCRLF()
}

// Flush drains the buffered writer to the underlying connection.
func (w *Writer) Flush() error {
	return w.bw.Flush()
}

func (w *Writer) writeLine(prefix byte, s string) error {
	if err := w.bw.WriteByte(prefix); err != nil {
		return err
	}
	if _, err := w.bw.WriteString(s); err != nil {
		return err
	}
	return w.writeCRLF()
}

func (w *Writer) writeNumber(n int64) error {
	w.scratch = strconv.AppendInt(w.scratch[:0], n, 10)
	_, err := w.bw.Write(w.scratch)
	return err
}

func (w *Writer) writeCRLF() error {
	_, err := w.bw.WriteString("\r\n")
	return err
}
