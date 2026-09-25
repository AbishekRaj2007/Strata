package wal

import (
	"os"
	"path/filepath"
	"testing"
)

// BenchmarkWriterWrite isolates Write's own allocation cost from fsync cost:
// no Syncer, no O_SYNC, just repeated framing of the same batch shape T8.2's
// heap profile found dominating (see docs/profiles/t8.2-report.md).
func BenchmarkWriterWrite(b *testing.B) {
	dir := b.TempDir()
	f, err := os.Create(filepath.Join(dir, "bench.wal"))
	if err != nil {
		b.Fatalf("create: %v", err)
	}
	b.Cleanup(func() { _ = f.Close() })

	w := NewWriter(f)
	key := make([]byte, 16)
	val := make([]byte, 64)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		batch := Batch{Sequence: uint64(i)}
		batch.AppendSet(key, val)
		if _, err := w.Write(&batch); err != nil {
			b.Fatalf("write: %v", err)
		}
	}
}
