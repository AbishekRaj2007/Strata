package server

import (
	"testing"

	"github.com/AbishekRaj2007/Strata/internal/resp"
)

// BenchmarkCommandArgs isolates commandArgs' own allocation cost -- T8.2's
// heap profile found it among the top five allocation sites under load (see
// docs/profiles/t8.2-report.md) -- from RESP parsing and dispatch.
//
// Fresh mirrors the pre-T8.3 call site (dst == nil every time); Reused mirrors
// conn.serve's pooled argsBuf, which is what a real connection processing a
// sequence of commands actually does.
func BenchmarkCommandArgs(b *testing.B) {
	value := resp.Value{
		Type: resp.Array,
		Array: []resp.Value{
			{Type: resp.BulkString, Bytes: []byte("SET")},
			{Type: resp.BulkString, Bytes: []byte("key-000000000001")},
			{Type: resp.BulkString, Bytes: []byte("value-0123456789abcdef0123456789abcdef")},
		},
	}

	b.Run("Fresh", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, err := commandArgs(value, nil); err != nil {
				b.Fatalf("commandArgs: %v", err)
			}
		}
	})

	b.Run("Reused", func(b *testing.B) {
		var buf [][]byte
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			args, err := commandArgs(value, buf[:0])
			if err != nil {
				b.Fatalf("commandArgs: %v", err)
			}
			buf = args
		}
	})
}
