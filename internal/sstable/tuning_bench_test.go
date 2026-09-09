package sstable

import (
	"fmt"
	"testing"

	"github.com/AbishekRaj2007/Strata/internal/cache"
	"github.com/AbishekRaj2007/Strata/internal/memtable"
)

// The T5.3 tuning study lives here rather than in a separate harness because
// it measures the table read path directly. An engine-level benchmark would
// fold in the memtable, the manifest and the LSM's level walk, and the
// parameter under study would be a small term in a large number -- which is
// how a sweep ends up measuring noise and calling it a finding.
//
// Reproduce with:
//
//	go test ./internal/sstable/ -run '^$' -bench 'Tuning' -benchtime 200000x -count 3
//
// docs/benchmarks.md records the hardware, the medians and the variance.

// tuningKeys is the table size every sweep uses: large enough for the index
// binary search and the block layout to matter, small enough that three runs
// of the whole study finish in a minute.
const tuningKeys = 100_000

func tuningEntries() []memtable.Entry {
	entries := make([]memtable.Entry, tuningKeys)
	for i := range entries {
		entries[i] = memtable.Entry{
			Key:      []byte(fmt.Sprintf("present:%08d", i)),
			Value:    []byte(fmt.Sprintf("value-payload-%08d", i)),
			Sequence: uint64(i + 1),
		}
	}
	sortEntries(entries)
	return entries
}

// buildTuningTable writes a table at the given parameters and opens it,
// optionally against a cache.
func buildTuningTable(tb testing.TB, opts WriterOptions, c *cache.Cache) (*Table, Info) {
	tb.Helper()

	dir := tb.TempDir()
	info, err := WriteTableOpts(dir, 1, newSliceIterator(tuningEntries()), opts)
	if err != nil {
		tb.Fatalf("WriteTableOpts: %v", err)
	}
	tbl, err := OpenWith(info.Path, OpenOptions{Number: 1, Cache: c})
	if err != nil {
		tb.Fatalf("OpenWith: %v", err)
	}
	tb.Cleanup(func() { _ = tbl.Close() })
	return tbl, info
}

func tuningAbsentKey(i int) []byte {
	return []byte(fmt.Sprintf("absentt:%08d", i))
}

func tuningPresentKey(i int) []byte {
	return []byte(fmt.Sprintf("present:%08d", i%tuningKeys))
}

// BenchmarkTuningAbsentKey is the headline measurement: what the bloom filter
// buys on a key the table does not hold.
//
// Both arms run the identical read path -- same table, same index, same block
// decoder -- differing only in whether the filter is consulted. Comparing
// against a separately built no-filter table would confound the filter with
// whatever else changed about the file.
//
// Warm and cold are both measured because they answer different questions.
// Warm (a populated block cache) is the honest steady state of a running
// server and isolates the CPU cost the filter removes. Cold is where the
// filter matters most, since the block read it skips is a syscall.
func BenchmarkTuningAbsentKey(b *testing.B) {
	for _, cached := range []bool{false, true} {
		name := "cold"
		if cached {
			name = "warm"
		}

		b.Run(name+"/bloom", func(b *testing.B) {
			var c *cache.Cache
			if cached {
				c = cache.New(256 << 20)
			}
			tbl, _ := buildTuningTable(b, WriterOptions{}, c)
			if cached {
				warmEveryBlock(b, tbl)
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, found, err := tbl.Get(tuningAbsentKey(i)); err != nil || found {
					b.Fatalf("found=%v err=%v", found, err)
				}
			}
		})

		b.Run(name+"/nobloom", func(b *testing.B) {
			var c *cache.Cache
			if cached {
				c = cache.New(256 << 20)
			}
			tbl, _ := buildTuningTable(b, WriterOptions{}, c)
			if cached {
				warmEveryBlock(b, tbl)
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, found, err := tbl.getUnfiltered(tuningAbsentKey(i)); err != nil || found {
					b.Fatalf("found=%v err=%v", found, err)
				}
			}
		})
	}
}

// warmEveryBlock reads the whole table once so the cache is populated before
// the timed loop starts.
func warmEveryBlock(tb testing.TB, t *Table) {
	tb.Helper()
	it := t.NewIterator()
	for it.Next() {
	}
	if err := it.Err(); err != nil {
		tb.Fatalf("warming: %v", err)
	}
}

// BenchmarkTuningPresentKey is the control. The filter cannot help a key that
// is present -- it always says "maybe" and the block is read anyway -- so this
// arm measures the cost the filter adds rather than the time it saves. A
// study that only reported the win would be reporting half the trade.
func BenchmarkTuningPresentKey(b *testing.B) {
	for _, useFilter := range []bool{true, false} {
		name := "nobloom"
		if useFilter {
			name = "bloom"
		}
		b.Run(name, func(b *testing.B) {
			c := cache.New(256 << 20)
			tbl, _ := buildTuningTable(b, WriterOptions{}, c)
			warmEveryBlock(b, tbl)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				var (
					found bool
					err   error
				)
				if useFilter {
					_, found, err = tbl.Get(tuningPresentKey(i))
				} else {
					_, found, err = tbl.getUnfiltered(tuningPresentKey(i))
				}
				if err != nil || !found {
					b.Fatalf("found=%v err=%v", found, err)
				}
			}
		})
	}
}

// BenchmarkTuningBitsPerKey sweeps the filter's memory/selectivity trade on
// absent-key lookups. The memory cost of each configuration is reported by
// TestTuningBitsPerKeyCost, so the two halves of the trade can be read
// together.
func BenchmarkTuningBitsPerKey(b *testing.B) {
	for _, bits := range []int{4, 8, 10, 16} {
		b.Run(fmt.Sprintf("bits=%d", bits), func(b *testing.B) {
			c := cache.New(256 << 20)
			tbl, _ := buildTuningTable(b, WriterOptions{BitsPerKey: bits}, c)
			warmEveryBlock(b, tbl)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, found, err := tbl.Get(tuningAbsentKey(i)); err != nil || found {
					b.Fatalf("found=%v err=%v", found, err)
				}
			}
		})
	}
}

// BenchmarkTuningBlockSize sweeps block size on present-key lookups, which is
// where it matters: a bigger block is one seek for more bytes, and the
// read-amplification side of that is measured by TestTuningBlockSizeCost.
func BenchmarkTuningBlockSize(b *testing.B) {
	for _, size := range []int{1 << 10, 4 << 10, 16 << 10, 64 << 10} {
		b.Run(fmt.Sprintf("block=%dKiB", size>>10), func(b *testing.B) {
			tbl, _ := buildTuningTable(b, WriterOptions{BlockSize: size}, nil)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, found, err := tbl.Get(tuningPresentKey(i)); err != nil || !found {
					b.Fatalf("found=%v err=%v", found, err)
				}
			}
		})
	}
}

// TestTuningBitsPerKeyCost records the memory price and the measured false
// positive rate beside each other, so a bits-per-key recommendation can cite
// both terms of the trade rather than one.
func TestTuningBitsPerKeyCost(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping the tuning study measurement in -short")
	}

	t.Log("bits/key |  k | bloom bytes | bytes/key | measured FP rate")
	for _, bits := range []int{4, 8, 10, 16} {
		tbl, info := buildTuningTable(t, WriterOptions{BitsPerKey: bits}, nil)

		const trials = 100_000
		for i := 0; i < trials; i++ {
			if _, found, err := tbl.Get(tuningAbsentKey(i)); err != nil || found {
				t.Fatalf("found=%v err=%v", found, err)
			}
		}
		probes, rejects := tbl.BloomStats()
		fp := float64(probes-rejects) / float64(probes)

		t.Logf("%8d | %2d | %11d | %9.2f | %15.3f%%",
			bits, tbl.Filter().Probes(), info.BloomBytes,
			float64(info.BloomBytes)/float64(tuningKeys), fp*100)
	}
}

// TestTuningBlockSizeCost records read amplification: bytes pulled from the
// file per lookup, against the ~30 bytes a caller actually asked for.
//
// This is the number that makes the block-size choice legible. Latency alone
// would suggest bigger blocks are free; the byte count shows what they cost
// in bandwidth and in cache space per useful key.
func TestTuningBlockSizeCost(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping the tuning study measurement in -short")
	}

	t.Log("block size | data blocks | file bytes | bytes read per lookup")
	for _, size := range []int{1 << 10, 4 << 10, 16 << 10, 64 << 10} {
		tbl, info := buildTuningTable(t, WriterOptions{BlockSize: size}, nil)

		const lookups = 10_000
		for i := 0; i < lookups; i++ {
			// Spread the lookups across the keyspace so consecutive reads do
			// not land in the same block and flatter the larger sizes.
			if _, found, err := tbl.Get(tuningPresentKey(i * 7919)); err != nil || !found {
				t.Fatalf("found=%v err=%v", found, err)
			}
		}

		t.Logf("%9dK | %11d | %10d | %21.1f",
			size>>10, len(tbl.index), info.Size,
			float64(tbl.BytesRead())/float64(lookups))
	}
}
