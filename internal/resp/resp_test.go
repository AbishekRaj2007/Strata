package resp

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
)

// readAll drives a Reader over a fixed input, which is how every framing test
// here checks that the reader consumes exactly the bytes it should and no more.
func readOne(t *testing.T, input string) (Value, error) {
	t.Helper()
	return NewReader(bufio.NewReader(strings.NewReader(input))).ReadValue()
}

func TestReadSimpleString(t *testing.T) {
	v, err := readOne(t, "+OK\r\n")
	if err != nil {
		t.Fatalf("ReadValue: %v", err)
	}
	if v.Type != SimpleString {
		t.Errorf("Type = %v, want SimpleString", v.Type)
	}
	if string(v.Bytes) != "OK" {
		t.Errorf("Bytes = %q, want %q", v.Bytes, "OK")
	}
}

func TestReadError(t *testing.T) {
	v, err := readOne(t, "-ERR unknown command\r\n")
	if err != nil {
		t.Fatalf("ReadValue: %v", err)
	}
	if v.Type != Error {
		t.Errorf("Type = %v, want Error", v.Type)
	}
	if string(v.Bytes) != "ERR unknown command" {
		t.Errorf("Bytes = %q", v.Bytes)
	}
}

func TestReadInteger(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  int64
	}{
		{"zero", ":0\r\n", 0},
		{"positive", ":42\r\n", 42},
		{"negative", ":-42\r\n", -42},
		{"explicit plus", ":+42\r\n", 42},
		{"max int64", ":9223372036854775807\r\n", 9223372036854775807},
		{"min int64", ":-9223372036854775808\r\n", -9223372036854775808},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v, err := readOne(t, tt.input)
			if err != nil {
				t.Fatalf("ReadValue: %v", err)
			}
			if v.Type != Integer {
				t.Fatalf("Type = %v, want Integer", v.Type)
			}
			if v.Int != tt.want {
				t.Errorf("Int = %d, want %d", v.Int, tt.want)
			}
		})
	}
}

func TestReadBulkString(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"ordinary", "$5\r\nhello\r\n", "hello"},
		{"empty", "$0\r\n\r\n", ""},
		{"embedded CRLF", "$6\r\na\r\nb!\r\n", "a\r\nb!"},
		{"binary safe", "$3\r\n\x00\x01\x02\r\n", "\x00\x01\x02"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v, err := readOne(t, tt.input)
			if err != nil {
				t.Fatalf("ReadValue: %v", err)
			}
			if v.Type != BulkString {
				t.Fatalf("Type = %v, want BulkString", v.Type)
			}
			if v.Null {
				t.Fatal("Null = true, want false")
			}
			if string(v.Bytes) != tt.want {
				t.Errorf("Bytes = %q, want %q", v.Bytes, tt.want)
			}
		})
	}
}

// TestNullAndEmptyBulkAreDistinct guards the distinction ADR-003 calls out as
// critical: null means the key is absent, empty means it holds a zero-length
// value. Collapsing them makes deleted keys read back as empty strings.
func TestNullAndEmptyBulkAreDistinct(t *testing.T) {
	null, err := readOne(t, "$-1\r\n")
	if err != nil {
		t.Fatalf("null bulk: %v", err)
	}
	empty, err := readOne(t, "$0\r\n\r\n")
	if err != nil {
		t.Fatalf("empty bulk: %v", err)
	}

	if !null.Null {
		t.Error("null bulk: Null = false, want true")
	}
	if empty.Null {
		t.Error("empty bulk: Null = true, want false")
	}
	if len(empty.Bytes) != 0 {
		t.Errorf("empty bulk: Bytes = %q, want empty", empty.Bytes)
	}

	// The round trip must preserve the distinction too, or the difference is
	// lost the moment a reply crosses the writer.
	var nullBuf, emptyBuf bytes.Buffer
	nw := NewWriter(bufio.NewWriter(&nullBuf))
	if err := nw.WriteNullBulkString(); err != nil {
		t.Fatalf("WriteNullBulkString: %v", err)
	}
	if err := nw.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	ew := NewWriter(bufio.NewWriter(&emptyBuf))
	if err := ew.WriteBulkString(nil); err != nil {
		t.Fatalf("WriteBulkString: %v", err)
	}
	if err := ew.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	if nullBuf.String() == emptyBuf.String() {
		t.Fatalf("null and empty encode identically as %q", nullBuf.String())
	}
	if got := nullBuf.String(); got != "$-1\r\n" {
		t.Errorf("null encoding = %q, want %q", got, "$-1\r\n")
	}
	if got := emptyBuf.String(); got != "$0\r\n\r\n" {
		t.Errorf("empty encoding = %q, want %q", got, "$0\r\n\r\n")
	}
}

func TestReadArray(t *testing.T) {
	v, err := readOne(t, "*2\r\n$3\r\nGET\r\n$3\r\nfoo\r\n")
	if err != nil {
		t.Fatalf("ReadValue: %v", err)
	}
	if v.Type != Array {
		t.Fatalf("Type = %v, want Array", v.Type)
	}
	if len(v.Array) != 2 {
		t.Fatalf("len(Array) = %d, want 2", len(v.Array))
	}
	if string(v.Array[0].Bytes) != "GET" || string(v.Array[1].Bytes) != "foo" {
		t.Errorf("Array = %q, %q", v.Array[0].Bytes, v.Array[1].Bytes)
	}
}

func TestReadEmptyArray(t *testing.T) {
	v, err := readOne(t, "*0\r\n")
	if err != nil {
		t.Fatalf("ReadValue: %v", err)
	}
	if v.Type != Array {
		t.Fatalf("Type = %v, want Array", v.Type)
	}
	if len(v.Array) != 0 {
		t.Errorf("len(Array) = %d, want 0", len(v.Array))
	}
}

func TestReadNullArray(t *testing.T) {
	v, err := readOne(t, "*-1\r\n")
	if err != nil {
		t.Fatalf("ReadValue: %v", err)
	}
	if v.Type != Array || !v.Null {
		t.Errorf("Type = %v, Null = %v, want Array and true", v.Type, v.Null)
	}
}

func TestReadNestedArray(t *testing.T) {
	v, err := readOne(t, "*2\r\n*1\r\n:1\r\n$3\r\nfoo\r\n")
	if err != nil {
		t.Fatalf("ReadValue: %v", err)
	}
	if len(v.Array) != 2 {
		t.Fatalf("len(Array) = %d, want 2", len(v.Array))
	}
	inner := v.Array[0]
	if inner.Type != Array || len(inner.Array) != 1 || inner.Array[0].Int != 1 {
		t.Errorf("inner array = %+v", inner)
	}
}

// TestMalformed is the core of T1.1: every way a peer can frame something
// wrong must produce an error, never a panic and never a partial value passed
// off as complete.
func TestMalformed(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{"empty input", ""},
		{"unknown type byte", "?3\r\nfoo\r\n"},
		{"bare CRLF", "\r\n"},
		{"simple string unterminated", "+OK"},
		{"simple string bare LF", "+OK\n"},
		{"simple string CR without LF", "+OK\rX"},
		{"error unterminated", "-ERR"},
		{"integer not a number", ":abc\r\n"},
		{"integer empty", ":\r\n"},
		{"integer overflows int64", ":9223372036854775808\r\n"},
		{"integer with trailing garbage", ":12x\r\n"},
		{"integer unterminated", ":42"},
		{"integer lone minus", ":-\r\n"},
		{"bulk length not a number", "$abc\r\nfoo\r\n"},
		{"bulk length empty", "$\r\n"},
		{"bulk negative other than -1", "$-2\r\n"},
		{"bulk truncated payload", "$5\r\nhel"},
		{"bulk payload missing CRLF", "$5\r\nhello"},
		{"bulk payload wrong terminator", "$5\r\nhelloXX"},
		{"bulk header unterminated", "$5"},
		{"bulk exceeds maximum", fmt.Sprintf("$%d\r\n", MaxBulkLength+1)},
		{"bulk absurd length", "$4294967296\r\n"},
		{"array length not a number", "*abc\r\n"},
		{"array negative other than -1", "*-2\r\n"},
		{"array truncated elements", "*2\r\n$3\r\nGET\r\n"},
		{"array element malformed", "*1\r\n?bad\r\n"},
		{"array exceeds maximum", fmt.Sprintf("*%d\r\n", MaxArrayLength+1)},
		{"array header unterminated", "*2"},
		{"nested array truncated", "*1\r\n*1\r\n"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("panic on %q: %v", tt.input, r)
				}
			}()

			if _, err := readOne(t, tt.input); err == nil {
				t.Errorf("ReadValue(%q) = nil error, want an error", tt.input)
			}
		})
	}
}

// TestOversizedBulkRejectedBeforeAllocation asserts the length prefix is
// checked against the limit before any buffer is sized from it. A reader that
// allocates first is a one-packet denial of service.
func TestOversizedBulkRejectedBeforeAllocation(t *testing.T) {
	// The declared length is enormous but only a few bytes back it, so a
	// reader that allocates eagerly either dies or takes a visible pause,
	// while a correct one rejects on the header alone.
	input := fmt.Sprintf("$%d\r\nshort", MaxBulkLength*16)

	_, err := readOne(t, input)
	if err == nil {
		t.Fatal("ReadValue = nil error, want an error")
	}
	if !errors.Is(err, ErrProtocol) {
		t.Errorf("error = %v, want it to wrap ErrProtocol", err)
	}
}

// TestTruncatedStreamIsNotPartialValue covers the mid-record disconnect: a
// stream that ends inside a record must error rather than yield the prefix it
// managed to read.
func TestTruncatedStreamIsNotPartialValue(t *testing.T) {
	prefixes := []string{
		"*3\r\n$3\r\nSET\r\n$3\r\nfoo\r\n$3\r\nba",
		"*3\r\n$3\r\nSET\r\n$3\r\nfo",
		"*3\r\n$3\r\nSE",
		"*3\r\n",
		"*",
	}

	for _, p := range prefixes {
		t.Run(fmt.Sprintf("%q", p), func(t *testing.T) {
			v, err := readOne(t, p)
			if err == nil {
				t.Fatalf("ReadValue(%q) = %+v, nil error; want an error", p, v)
			}
		})
	}
}

// TestCleanEOFIsDistinguishable matters for the connection loop in T1.2: a
// client that disconnects between commands is a normal close, not an error to
// log, and the two cases have to be told apart at the call site.
func TestCleanEOFIsDistinguishable(t *testing.T) {
	_, err := readOne(t, "")
	if !errors.Is(err, io.EOF) {
		t.Errorf("error on empty stream = %v, want io.EOF", err)
	}

	// A stream cut mid-record is a broken peer, not a clean close.
	_, err = readOne(t, "$5\r\nhel")
	if errors.Is(err, io.EOF) {
		t.Errorf("error on truncated record = %v, want something other than io.EOF", err)
	}
}

func TestSequentialReadsOnOneStream(t *testing.T) {
	const input = "+OK\r\n:7\r\n$3\r\nfoo\r\n*1\r\n$2\r\nhi\r\n"
	r := NewReader(bufio.NewReader(strings.NewReader(input)))

	for i, want := range []Type{SimpleString, Integer, BulkString, Array} {
		v, err := r.ReadValue()
		if err != nil {
			t.Fatalf("value %d: %v", i, err)
		}
		if v.Type != want {
			t.Errorf("value %d: Type = %v, want %v", i, v.Type, want)
		}
	}

	if _, err := r.ReadValue(); !errors.Is(err, io.EOF) {
		t.Errorf("after last value: err = %v, want io.EOF", err)
	}
}

func TestWriter(t *testing.T) {
	tests := []struct {
		name  string
		write func(*Writer) error
		want  string
	}{
		{"simple string", func(w *Writer) error { return w.WriteSimpleString("OK") }, "+OK\r\n"},
		{"error", func(w *Writer) error { return w.WriteError("ERR bad") }, "-ERR bad\r\n"},
		{"integer", func(w *Writer) error { return w.WriteInteger(42) }, ":42\r\n"},
		{"negative integer", func(w *Writer) error { return w.WriteInteger(-1) }, ":-1\r\n"},
		{"bulk string", func(w *Writer) error { return w.WriteBulkString([]byte("hello")) }, "$5\r\nhello\r\n"},
		{"empty bulk string", func(w *Writer) error { return w.WriteBulkString([]byte{}) }, "$0\r\n\r\n"},
		{"null bulk string", func(w *Writer) error { return w.WriteNullBulkString() }, "$-1\r\n"},
		{"empty array", func(w *Writer) error { return w.WriteArrayHeader(0) }, "*0\r\n"},
		{"array header", func(w *Writer) error { return w.WriteArrayHeader(2) }, "*2\r\n"},
		{
			"binary safe bulk",
			func(w *Writer) error { return w.WriteBulkString([]byte("a\r\nb")) },
			"$4\r\na\r\nb\r\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			w := NewWriter(bufio.NewWriter(&buf))
			if err := tt.write(w); err != nil {
				t.Fatalf("write: %v", err)
			}
			if err := w.Flush(); err != nil {
				t.Fatalf("Flush: %v", err)
			}
			if got := buf.String(); got != tt.want {
				t.Errorf("wrote %q, want %q", got, tt.want)
			}
		})
	}
}

// TestWriteArrayOfBulkStrings builds the shape SCAN replies with, confirming a
// header followed by elements frames correctly as one value.
func TestWriteArrayOfBulkStrings(t *testing.T) {
	var buf bytes.Buffer
	w := NewWriter(bufio.NewWriter(&buf))

	if err := w.WriteArrayHeader(2); err != nil {
		t.Fatalf("WriteArrayHeader: %v", err)
	}
	for _, s := range []string{"a", "bb"} {
		if err := w.WriteBulkString([]byte(s)); err != nil {
			t.Fatalf("WriteBulkString: %v", err)
		}
	}
	if err := w.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	const want = "*2\r\n$1\r\na\r\n$2\r\nbb\r\n"
	if got := buf.String(); got != want {
		t.Errorf("wrote %q, want %q", got, want)
	}

	// What the writer emits, the reader must accept.
	v, err := readOne(t, buf.String())
	if err != nil {
		t.Fatalf("re-reading own output: %v", err)
	}
	if len(v.Array) != 2 {
		t.Errorf("len(Array) = %d, want 2", len(v.Array))
	}
}

// TestWriterPropagatesErrors ensures a dead connection surfaces as an error
// rather than being swallowed, since the connection loop decides to drop the
// client based on it.
func TestWriterPropagatesErrors(t *testing.T) {
	w := NewWriter(bufio.NewWriter(failingWriter{}))

	// bufio absorbs small writes, so the failure may surface at either the
	// write or the flush; what matters is that it is not lost.
	err := w.WriteBulkString(bytes.Repeat([]byte("x"), 64*1024))
	if err == nil {
		err = w.Flush()
	}
	if err == nil {
		t.Error("no error from a writer whose underlying sink always fails")
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("connection reset") }

// FuzzReadValue is T1.1's no-panic guarantee over arbitrary bytes. Run longer
// than the default with:
//
//	go test ./internal/resp -run '^$' -fuzz FuzzReadValue -fuzztime 60s
func FuzzReadValue(f *testing.F) {
	seeds := []string{
		"+OK\r\n",
		"-ERR x\r\n",
		":42\r\n",
		"$5\r\nhello\r\n",
		"$0\r\n\r\n",
		"$-1\r\n",
		"*2\r\n$3\r\nGET\r\n$3\r\nfoo\r\n",
		"*0\r\n",
		"*-1\r\n",
		"*1\r\n*1\r\n:1\r\n",
		"$999999999999\r\n",
		"*99999999\r\n",
		"",
		"\r\n",
		"?bad\r\n",
	}
	for _, s := range seeds {
		f.Add([]byte(s))
	}

	f.Fuzz(func(t *testing.T, data []byte) {
		// The contract is only that arbitrary input never panics and never
		// hangs; any error is acceptable, and so is a successful parse.
		v, err := NewReader(bufio.NewReader(bytes.NewReader(data))).ReadValue()
		if err != nil {
			return
		}

		// A value parsed as non-null must not carry a nil payload where the
		// type promises bytes, since call sites index into it directly.
		if v.Type == BulkString && !v.Null && v.Bytes == nil {
			t.Fatalf("non-null bulk string with nil Bytes from %q", data)
		}
	})
}
