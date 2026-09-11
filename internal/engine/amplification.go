package engine

import (
	"fmt"

	"github.com/AbishekRaj2007/Strata/internal/manifest"
	"github.com/AbishekRaj2007/Strata/internal/sstable"
)

// Amplification is the three costs a leveled LSM trades against each other.
//
// They are reported as raw counters plus derived ratios rather than ratios
// alone, because a ratio with no denominator in sight invites the wrong
// conclusion -- a write amplification of 1.0 over four bytes written means
// nothing at all.
type Amplification struct {
	// UserBytesWritten is the key and value bytes the application handed to
	// Put and Delete. It is the denominator of write amplification: what the
	// workload asked for.
	UserBytesWritten uint64

	// FlushBytesWritten and CompactionBytesWritten are what actually reached
	// the disk as SSTables. They are separate because they answer different
	// questions: the flush figure is close to unavoidable, while the
	// compaction figure is the price of the leveled layout and the thing the
	// level multiplier moves.
	//
	// Neither counts the WAL. The WAL is a fixed cost per write that no
	// compaction policy changes, so folding it in would blur the number this
	// study exists to compare.
	FlushBytesWritten      uint64
	CompactionBytesWritten uint64

	// Gets is the number of point lookups served, and TableReads the number
	// of SSTables opened to serve them. Their ratio is read amplification.
	//
	// A lookup answered from a memtable reads no table, so the ratio is a
	// property of the workload as well as of the tree -- which is the honest
	// framing, since that is also true of the latency it explains.
	Gets       uint64
	TableReads uint64

	// DiskBytesLive is the total size of every SSTable the current version
	// names.
	DiskBytesLive uint64

	// LogicalBytesLive is the key and value bytes of the live data, counting
	// each key once. It is zero until measured.
	//
	// It cannot be maintained incrementally without knowing the previous
	// size of every key being overwritten, which would mean a read on every
	// write. So it is measured on demand, by COMPACT, over a settled tree --
	// which is exactly when the measurement is meaningful anyway, since a
	// space figure taken mid-backlog measures the backlog.
	LogicalBytesLive uint64

	// LogicalMeasured says whether LogicalBytesLive is a measurement or
	// merely a zero. Without it a fresh database and an unmeasured one are
	// indistinguishable, and a space amplification of zero would be printed
	// as if it meant something.
	LogicalMeasured bool
}

// DiskBytesWritten is everything written to disk as tables.
func (a Amplification) DiskBytesWritten() uint64 {
	return a.FlushBytesWritten + a.CompactionBytesWritten
}

// Write is disk bytes written per user byte written. It is zero when nothing
// has been written, which is not a measurement of 0x.
func (a Amplification) Write() float64 {
	if a.UserBytesWritten == 0 {
		return 0
	}
	return float64(a.DiskBytesWritten()) / float64(a.UserBytesWritten)
}

// Read is SSTables opened per point lookup.
func (a Amplification) Read() float64 {
	if a.Gets == 0 {
		return 0
	}
	return float64(a.TableReads) / float64(a.Gets)
}

// Space is disk bytes held per live logical byte. It is zero until
// LogicalBytesLive has been measured; check LogicalMeasured.
func (a Amplification) Space() float64 {
	if !a.LogicalMeasured || a.LogicalBytesLive == 0 {
		return 0
	}
	return float64(a.DiskBytesLive) / float64(a.LogicalBytesLive)
}

func (a Amplification) String() string {
	return fmt.Sprintf("write %.2fx (%d user, %d disk), read %.2f tables/get, space %.2fx (%d disk, %d live)",
		a.Write(), a.UserBytesWritten, a.DiskBytesWritten(),
		a.Read(), a.Space(), a.DiskBytesLive, a.LogicalBytesLive)
}

// measureLogicalBytes reads the settled tree and sums the key and value bytes
// of the live data, counting each user key once.
//
// It reads every file, so it runs only from Compact. The tree must be settled
// first: over an unsettled tree the same key appears at several levels and
// the newest version has to be resolved per key, which is the read path's job
// rather than a measurement's. After settling, the non-overlap invariant
// below L0 means a key is at most once per level and the top-down walk is
// exact.
func measureLogicalBytes(dir string, v *manifest.Version) (uint64, error) {
	seen := make(map[string]struct{})
	var total uint64

	for level := 0; level < manifest.NumLevels; level++ {
		for _, f := range v.Files(level) {
			t, err := sstable.Open(fmt.Sprintf("%s/%s", dir, f.Name()))
			if err != nil {
				return 0, fmt.Errorf("engine: measure %s: %w", f.Name(), err)
			}

			it := t.NewIterator()
			for it.Next() {
				e := it.Entry()
				key := string(e.Key)
				// Levels are walked newest-first, so the first version of a
				// key wins -- the same rule the read path uses, for the same
				// reason.
				if _, dup := seen[key]; dup {
					continue
				}
				seen[key] = struct{}{}

				// A tombstone is live data structurally but holds no value
				// the user can read, so it counts as zero logical bytes
				// while still occupying disk. That is the honest accounting:
				// tombstones are part of what space amplification measures.
				if e.Tombstone {
					continue
				}
				total += uint64(len(e.Key) + len(e.Value))
			}

			err = it.Err()
			_ = t.Close()
			if err != nil {
				return 0, fmt.Errorf("engine: measure %s: %w", f.Name(), err)
			}
		}
	}
	return total, nil
}
