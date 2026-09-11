package engine

import (
	"fmt"
	"math/rand"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/AbishekRaj2007/Strata/internal/compaction"
	"github.com/AbishekRaj2007/Strata/internal/wal"
)

// T6.6's characterisation. The measurements here are the ones quoted in
// docs/benchmarks.md; nothing in that document is written by hand.
//
// The dataset size is a variable rather than the 1 GB plan.md names, because
// a gigabyte through a 4 MB memtable is several minutes of compaction and
// this has to stay part of `go test ./...`. The default is small enough to
// run every time and large enough to reach three levels, which is what the
// figures depend on. Set STRATA_AMPL_MB for the full-scale run:
//
//	STRATA_AMPL_MB=1024 go test ./internal/engine -run Amplification -v
//
// Whatever scale is used is reported alongside the result, so a number in
// the docs can never be read as having come from a different one.

const defaultAmplMB = 24

func amplDatasetMB(t *testing.T) int {
	t.Helper()

	raw := os.Getenv("STRATA_AMPL_MB")
	if raw == "" {
		return defaultAmplMB
	}
	mb, err := strconv.Atoi(raw)
	if err != nil || mb <= 0 {
		t.Fatalf("STRATA_AMPL_MB=%q is not a positive integer", raw)
	}
	return mb
}

// amplEngine opens an engine with a tree small enough that a modest dataset
// reaches several levels, which is where amplification becomes visible.
func amplEngine(t *testing.T, multiplier uint64) *LSM {
	t.Helper()

	e, err := Open(Options{
		Dir:       t.TempDir(),
		Threshold: 1 << 20,
		// SyncNever: the WAL is a fixed per-write cost that no compaction
		// policy changes, and fsyncing it here would make the run dominated
		// by a variable the study is not about.
		SyncPolicy: wal.SyncNever,
		Compaction: compaction.Options{
			BaseLevelBytes:  4 << 20,
			LevelMultiplier: multiplier,
			TargetFileBytes: 1 << 20,
		},
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = e.Close() })
	return e
}

// writeDataset writes mb megabytes of 1 KiB values over keyCount distinct
// keys, cycling through them so a dataset larger than the key space is an
// overwrite rather than growth.
func writeDataset(t *testing.T, e *LSM, mb, keyCount int) {
	t.Helper()

	value := strings.Repeat("x", 1024)
	writes := mb * 1024

	for i := 0; i < writes; i++ {
		key := fmt.Sprintf("key%08d", i%keyCount)
		if err := e.Put([]byte(key), []byte(value)); err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
	}
}

// TestSpaceAmplificationAfterFullOverwrite is the done-when condition: write
// the dataset, overwrite it entirely, settle, and confirm the disk holds
// close to one copy.
func TestSpaceAmplificationAfterFullOverwrite(t *testing.T) {
	if testing.Short() {
		t.Skip("writes tens of megabytes through the whole compaction path")
	}

	mb := amplDatasetMB(t)
	keyCount := mb * 1024 // one 1 KiB value per key, so the dataset is mb
	e := amplEngine(t, 10)

	writeDataset(t, e, mb, keyCount)
	if err := e.Compact(); err != nil {
		t.Fatalf("compact: %v", err)
	}
	first := e.Amplification()

	// Overwrite every key. The logical data is unchanged; only garbage has
	// been added.
	writeDataset(t, e, mb, keyCount)
	if err := e.Compact(); err != nil {
		t.Fatalf("compact after overwrite: %v", err)
	}
	after := e.Amplification()

	t.Logf("dataset %d MB over %d keys", mb, keyCount)
	t.Logf("after first write:   %s", first)
	t.Logf("after full overwrite: %s", after)
	t.Logf("levels: %s", levelShape(e.vs.Current()))

	if !after.LogicalMeasured {
		t.Fatal("Compact did not measure live logical bytes")
	}

	// The done-when threshold. Under 2x is the figure ADR-002 accepted as
	// the reason for choosing leveled over size-tiered, so it is asserted
	// rather than merely reported.
	if space := after.Space(); space >= 2.0 {
		t.Errorf("space amplification is %.2fx after settling, want under 2x", space)
	}

	// Overwriting the whole dataset must not leave the disk holding two
	// copies: that is precisely the garbage compaction exists to reclaim.
	if after.DiskBytesLive > first.DiskBytesLive*3/2 {
		t.Errorf("disk grew from %d to %d bytes across an overwrite that added no logical data",
			first.DiskBytesLive, after.DiskBytesLive)
	}
}

// TestReadAmplificationIsBoundedByLevels checks the property leveled
// compaction is chosen for: a lookup touches at most one file per level
// below L0, plus the L0 files.
func TestReadAmplificationIsBoundedByLevels(t *testing.T) {
	if testing.Short() {
		t.Skip("writes several megabytes through the whole compaction path")
	}

	e := amplEngine(t, 10)
	const keyCount = 8 * 1024
	writeDataset(t, e, 8, keyCount)
	if err := e.Compact(); err != nil {
		t.Fatalf("compact: %v", err)
	}

	// Read a sample of keys that are certainly on disk, so the ratio is not
	// diluted by memtable hits.
	rng := rand.New(rand.NewSource(20260911))
	before := e.Amplification()
	const samples = 500
	for i := 0; i < samples; i++ {
		key := fmt.Sprintf("key%08d", rng.Intn(keyCount))
		if _, err := e.Get([]byte(key)); err != nil {
			t.Fatalf("get %q: %v", key, err)
		}
	}
	after := e.Amplification()

	reads := after.TableReads - before.TableReads
	perGet := float64(reads) / float64(samples)

	v := e.vs.Current()
	levels := 0
	for level := 1; level < 7; level++ {
		if v.NumFiles(level) > 0 {
			levels++
		}
	}
	bound := float64(v.NumFiles(0) + levels)

	t.Logf("%d table reads over %d gets = %.2f per get; tree is %s (bound %.0f)",
		reads, samples, perGet, levelShape(v), bound)

	if perGet > bound {
		t.Errorf("read amplification is %.2f tables per get, above the structural bound of %.0f",
			perGet, bound)
	}
}

// TestAmplificationAcrossLevelMultipliers is the sweep: the same workload at
// three multipliers, so the three costs can be seen moving against each
// other rather than asserted about individually.
func TestAmplificationAcrossLevelMultipliers(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the whole workload three times")
	}

	mb := amplDatasetMB(t)
	keyCount := mb * 1024

	var report strings.Builder
	fmt.Fprintf(&report, "\nlevel multiplier sweep, %d MB over %d keys, 1 KiB values\n", mb, keyCount)
	fmt.Fprintf(&report, "%-12s %8s %8s %10s %14s %s\n",
		"multiplier", "write", "space", "read", "disk bytes", "levels")

	for _, multiplier := range []uint64{4, 10, 20} {
		t.Run(fmt.Sprintf("multiplier_%d", multiplier), func(t *testing.T) {
			e := amplEngine(t, multiplier)

			// Written twice: the first pass fills the tree, the second
			// makes compaction rewrite data that is already settled, which
			// is where write amplification actually comes from.
			writeDataset(t, e, mb, keyCount)
			writeDataset(t, e, mb, keyCount)
			if err := e.Compact(); err != nil {
				t.Fatalf("compact: %v", err)
			}

			// Sample reads so the read figure is measured rather than left
			// at whatever the writes happened to do.
			rng := rand.New(rand.NewSource(20260911))
			before := e.Amplification()
			const samples = 500
			for i := 0; i < samples; i++ {
				if _, err := e.Get([]byte(fmt.Sprintf("key%08d", rng.Intn(keyCount)))); err != nil {
					t.Fatalf("get: %v", err)
				}
			}
			a := e.Amplification()
			perGet := float64(a.TableReads-before.TableReads) / float64(samples)

			fmt.Fprintf(&report, "%-12d %7.2fx %7.2fx %9.2f %14d %s\n",
				multiplier, a.Write(), a.Space(), perGet, a.DiskBytesLive,
				strings.TrimSpace(levelShape(e.vs.Current())))

			if space := a.Space(); space >= 2.0 {
				t.Errorf("space amplification is %.2fx at multiplier %d, want under 2x", space, multiplier)
			}
		})
	}

	t.Log(report.String())
}
