package sstable

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/AbishekRaj2007/Strata/internal/memtable"
)

// writeTestTable builds a table from entries (sorted for the caller) and
// returns the opened Table along with the Info the writer reported.
func writeTestTable(t *testing.T, entries []memtable.Entry, opts WriterOptions) (*Table, Info) {
	t.Helper()

	dir := t.TempDir()
	sortEntries(entries)
	info, err := WriteTableOpts(dir, 1, newSliceIterator(entries), opts)
	if err != nil {
		t.Fatalf("WriteTableOpts: %v", err)
	}
	tbl, err := Open(info.Path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = tbl.Close() })
	return tbl, info
}

func presentEntries(n int) []memtable.Entry {
	entries := make([]memtable.Entry, n)
	for i := range entries {
		entries[i] = memtable.Entry{
			Key:      []byte(fmt.Sprintf("present:%06d", i)),
			Value:    []byte(fmt.Sprintf("value-%d", i)),
			Sequence: uint64(i + 1),
		}
	}
	return entries
}

// TestTableGetNeverMissesAKeyTheFilterGuards is the correctness half of the
// integration. The filter now short-circuits Get before the index is even
// searched, so a filter bug does not degrade a lookup -- it loses the key.
func TestTableGetNeverMissesAKeyTheFilterGuards(t *testing.T) {
	const n = 20_000
	entries := presentEntries(n)
	tbl, _ := writeTestTable(t, entries, WriterOptions{})

	for _, e := range entries {
		got, found, err := tbl.Get(e.Key)
		if err != nil {
			t.Fatalf("Get(%q): %v", e.Key, err)
		}
		if !found {
			t.Fatalf("Get(%q) reported absent; the filter rejected a key the table holds", e.Key)
		}
		if string(got.Value) != string(e.Value) {
			t.Fatalf("Get(%q) = %q, want %q", e.Key, got.Value, e.Value)
		}
	}

	_, rejects := tbl.BloomStats()
	if rejects != 0 {
		t.Fatalf("%d present-key lookups were rejected by the filter, want 0", rejects)
	}
}

// TestFilterRejectsAbsentKeysWithoutTouchingABlock is the performance half:
// the filter has to actually be consulted, and consulted before the read.
//
// Asserting on the reject counter rather than on wall time is deliberate --
// the counter is the mechanism, and a timing assertion would be a flaky proxy
// for it. T5.3 measures the latency this buys.
func TestFilterRejectsAbsentKeysWithoutTouchingABlock(t *testing.T) {
	const n = 20_000
	tbl, _ := writeTestTable(t, presentEntries(n), WriterOptions{})

	const probes = 20_000
	for i := 0; i < probes; i++ {
		_, found, err := tbl.Get([]byte(fmt.Sprintf("absentt:%06d", i)))
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if found {
			t.Fatalf("a key that was never written was found")
		}
	}

	total, rejects := tbl.BloomStats()
	if total != probes {
		t.Fatalf("BloomStats reported %d probes, want %d", total, probes)
	}
	rate := float64(rejects) / float64(probes)
	t.Logf("filter rejected %d/%d absent lookups (%.2f%%) before any block read", rejects, probes, rate*100)
	if rate < 0.95 {
		t.Fatalf("only %.2f%% of absent lookups were rejected by the filter; at 10 bits per key it should be about 99%%", rate*100)
	}
}

// TestFilterIsSizedByDistinctKeysNotEntries checks the writer feeds user keys,
// not versions. A table holding twenty versions of every key must not carry a
// filter twenty times larger than it needs.
func TestFilterIsSizedByDistinctKeysNotEntries(t *testing.T) {
	const (
		distinct = 1000
		versions = 20
	)
	var entries []memtable.Entry
	for i := 0; i < distinct; i++ {
		for v := 0; v < versions; v++ {
			entries = append(entries, memtable.Entry{
				Key:      []byte(fmt.Sprintf("key:%06d", i)),
				Value:    []byte(fmt.Sprintf("v%d", v)),
				Sequence: uint64(i*versions + v + 1),
			})
		}
	}

	tbl, info := writeTestTable(t, entries, WriterOptions{})

	if info.EntryCount != distinct*versions {
		t.Fatalf("EntryCount = %d, want %d", info.EntryCount, distinct*versions)
	}
	if info.DistinctKeys != distinct {
		t.Fatalf("DistinctKeys = %d, want %d", info.DistinctKeys, distinct)
	}

	// 1000 keys at 10 bits is 1250 bytes plus the 12-byte header and the
	// 4-byte checksum. Sizing by entry count would give ~25 KB.
	if want := 1250 + 12 + 4; info.BloomBytes != want {
		t.Fatalf("BloomBytes = %d, want %d", info.BloomBytes, want)
	}

	// Every version's key still resolves, and the newest version wins.
	for i := 0; i < distinct; i++ {
		key := []byte(fmt.Sprintf("key:%06d", i))
		got, found, err := tbl.Get(key)
		if err != nil || !found {
			t.Fatalf("Get(%q): found=%v err=%v", key, found, err)
		}
		if string(got.Value) != fmt.Sprintf("v%d", versions-1) {
			t.Fatalf("Get(%q) = %q, want the newest version", key, got.Value)
		}
	}
}

// TestBitsPerKeyChangesTheFilterSizeAndSelectivity proves the option is wired
// through the writer and read back off the file, which is what makes the T5.3
// sweep meaningful rather than four runs of the same configuration.
func TestBitsPerKeyChangesTheFilterSizeAndSelectivity(t *testing.T) {
	const n = 10_000

	type result struct {
		bytes  int
		probes int
		fpRate float64
	}
	results := map[int]result{}

	for _, bits := range []int{4, 8, 10, 16} {
		tbl, info := writeTestTable(t, presentEntries(n), WriterOptions{BitsPerKey: bits})

		if got := tbl.Filter().BitsPerKey(); got != bits {
			t.Fatalf("filter read back at %d bits per key, want %d", got, bits)
		}

		const trials = 20_000
		for i := 0; i < trials; i++ {
			if _, found, err := tbl.Get([]byte(fmt.Sprintf("absentt:%08d", i))); err != nil || found {
				t.Fatalf("absent probe: found=%v err=%v", found, err)
			}
		}
		total, rejects := tbl.BloomStats()
		results[bits] = result{
			bytes:  info.BloomBytes,
			probes: tbl.Filter().Probes(),
			fpRate: float64(total-rejects) / float64(total),
		}
	}

	for _, bits := range []int{4, 8, 10, 16} {
		r := results[bits]
		t.Logf("bitsPerKey=%2d: k=%2d bloom block %6d bytes, false positive rate %.3f%%", bits, r.probes, r.bytes, r.fpRate*100)
	}

	// More bits must cost more memory and buy better selectivity, monotonically.
	for _, pair := range [][2]int{{4, 8}, {8, 10}, {10, 16}} {
		lo, hi := results[pair[0]], results[pair[1]]
		if hi.bytes <= lo.bytes {
			t.Errorf("%d bits per key produced %d bytes, not more than %d bits' %d", pair[1], hi.bytes, pair[0], lo.bytes)
		}
		if hi.fpRate >= lo.fpRate {
			t.Errorf("%d bits per key gave a %.3f%% false positive rate, not better than %d bits' %.3f%%", pair[1], hi.fpRate*100, pair[0], lo.fpRate*100)
		}
	}
}

// TestBlockSizeOptionChangesTheBlockCount checks the other T5.3 knob.
func TestBlockSizeOptionChangesTheBlockCount(t *testing.T) {
	entries := presentEntries(20_000)

	var prev int
	for _, size := range []int{1 << 10, 4 << 10, 16 << 10, 64 << 10} {
		tbl, _ := writeTestTable(t, entries, WriterOptions{BlockSize: size})
		blocks := len(tbl.index)
		t.Logf("blockSize=%6d: %5d data blocks", size, blocks)
		if prev != 0 && blocks >= prev {
			t.Errorf("blockSize=%d produced %d blocks, not fewer than the previous size's %d", size, blocks, prev)
		}
		prev = blocks

		// Whatever the block size, the data still reads back.
		for _, e := range entries[:100] {
			if _, found, err := tbl.Get(e.Key); err != nil || !found {
				t.Fatalf("blockSize=%d: Get(%q): found=%v err=%v", size, e.Key, found, err)
			}
		}
	}
}

// TestEmptyTableFilterReportsAbsent covers docs/format.md §3.3's rule that a
// table with no keys writes bit_array_len = 0 and every lookup reports absent.
func TestEmptyTableFilterReportsAbsent(t *testing.T) {
	tbl, info := writeTestTable(t, nil, WriterOptions{})

	if info.DistinctKeys != 0 {
		t.Fatalf("DistinctKeys = %d, want 0", info.DistinctKeys)
	}
	if tbl.Filter().SizeBytes() != 0 {
		t.Fatalf("bit array is %d bytes, want 0 for an empty table", tbl.Filter().SizeBytes())
	}
	if _, found, err := tbl.Get([]byte("anything")); err != nil || found {
		t.Fatalf("Get on an empty table: found=%v err=%v", found, err)
	}
}

// TestCorruptBloomBlockIsRejectedAtOpen sweeps every byte of the bloom block.
//
// A flipped bit in the filter is not a benign performance change: clearing a
// one turns a present key into a reported absence, which the read path cannot
// distinguish from the key never having been written. It has to be caught at
// open, like every other section of the table.
func TestCorruptBloomBlockIsRejectedAtOpen(t *testing.T) {
	dir := t.TempDir()
	entries := presentEntries(500)
	sortEntries(entries)
	info, err := WriteTableOpts(dir, 1, newSliceIterator(entries), WriterOptions{})
	if err != nil {
		t.Fatalf("WriteTableOpts: %v", err)
	}

	clean, err := os.ReadFile(info.Path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	// The footer records where the bloom block lives; read it from there
	// rather than recomputing the layout, so the sweep stays correct if the
	// section order ever changes.
	footerStart := len(clean) - footerSize
	bloomOffset := int(binary.LittleEndian.Uint64(clean[footerStart : footerStart+8]))
	bloomLength := int(binary.LittleEndian.Uint64(clean[footerStart+8 : footerStart+16]))
	if bloomLength <= 16 {
		t.Fatalf("bloom block is %d bytes; the table has no filter to corrupt", bloomLength)
	}

	damagedPath := info.Path + ".damaged"
	for i := bloomOffset; i < bloomOffset+bloomLength; i++ {
		damaged := append([]byte(nil), clean...)
		damaged[i] ^= 0x01
		if err := os.WriteFile(damagedPath, damaged, 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		tbl, err := Open(damagedPath)
		if err == nil {
			_ = tbl.Close()
			t.Fatalf("flipping byte %d (offset %d into the bloom block) opened cleanly", i, i-bloomOffset)
		}
		if !errors.Is(err, ErrCorruptTable) {
			t.Fatalf("flipping byte %d gave err=%v, want ErrCorruptTable", i, err)
		}
	}
}
