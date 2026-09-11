package compaction

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/AbishekRaj2007/Strata/internal/manifest"
	"github.com/AbishekRaj2007/Strata/internal/memtable"
	"github.com/AbishekRaj2007/Strata/internal/sstable"
)

// Violation is one broken invariant.
type Violation struct {
	// Invariant names the rule, stable enough to grep for.
	Invariant string

	// Detail says which files and keys broke it. A violation that does not
	// name the evidence is a second debugging session, not a diagnosis.
	Detail string
}

func (v Violation) String() string { return v.Invariant + ": " + v.Detail }

// Report is the outcome of a check.
type Report struct {
	Violations []Violation

	// FilesChecked and EntriesChecked say how much was actually examined, so
	// a clean report on an empty tree cannot be mistaken for a clean report
	// on a full one.
	FilesChecked   int
	EntriesChecked int
}

// OK reports whether every invariant held.
func (r *Report) OK() bool { return len(r.Violations) == 0 }

// Err returns an error describing every violation, or nil.
func (r *Report) Err() error {
	if r.OK() {
		return nil
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "%d invariant violations:", len(r.Violations))
	for _, v := range r.Violations {
		fmt.Fprintf(&sb, "\n  %s", v)
	}
	return errors.New(sb.String())
}

func (r *Report) fail(invariant, format string, args ...any) {
	r.Violations = append(r.Violations, Violation{
		Invariant: invariant,
		Detail:    fmt.Sprintf(format, args...),
	})
}

// Check verifies every structural invariant of a tree, reading the files
// rather than trusting the metadata that describes them.
//
// This is what turns a compaction bug from "reads sometimes return wrong
// data three hours in" into "violated at compaction 47". The difference is a
// day of debugging against ten minutes, which is why it runs after every
// compaction in verifying builds rather than only from the CLI: an invariant
// checked at shutdown catches nothing useful, because by then the compaction
// that broke it is hundreds of compactions in the past.
//
// It opens and reads every table, so it is far too slow for the hot path and
// is never enabled by default.
func Check(dir string, v *manifest.Version) (*Report, error) {
	return check(dir, v, true)
}

// CheckLive is Check without the orphan rule, for a tree that a running
// process is still writing to.
//
// The orphan rule cannot hold in a live process, and must not be made to.
// Both write paths deliberately create a durable table *before* the manifest
// names it -- that window is the whole basis of the crash-safety argument,
// since it is what makes "crash before the fsync" mean "the operation never
// happened". A flush in flight while a compaction verifies would be reported
// as debris when it is the system working exactly as designed.
//
// Everything else still applies, which is the part that catches compaction
// bugs: overlap, ordering, duplicate keys, and metadata that disagrees with
// the bytes.
func CheckLive(dir string, v *manifest.Version) (*Report, error) {
	return check(dir, v, false)
}

func check(dir string, v *manifest.Version, atRest bool) (*Report, error) {
	r := &Report{}

	// The cheap structural rules the version can answer on its own: L1+
	// non-overlap, per-level ordering, and no file at two levels.
	if err := v.CheckInvariants(); err != nil {
		r.fail("version-structure", "%v", err)
	}

	if err := checkManifestMatchesDisk(dir, v, r, atRest); err != nil {
		return nil, err
	}
	if err := checkContents(dir, v, r); err != nil {
		return nil, err
	}
	checkL0Ordering(v, r)

	return r, nil
}

// checkManifestMatchesDisk verifies the two directions separately, because
// they fail for opposite reasons and the fix differs.
//
// A named file missing from disk means something deleted a live file --
// the file-lifetime rule broken, and unrecoverable. An unnamed file on disk
// is an orphan: harmless, but it means a crash's debris was never swept, or
// a compaction leaked an output.
//
// Only the first direction is checkable in a live process; see CheckLive.
func checkManifestMatchesDisk(dir string, v *manifest.Version, r *Report, atRest bool) error {
	named := map[uint64]bool{}

	for level := 0; level < manifest.NumLevels; level++ {
		for _, f := range v.Files(level) {
			named[f.Number] = true

			info, err := os.Stat(filepath.Join(dir, f.Name()))
			if err != nil {
				r.fail("manifest-file-exists", "level %d names %s, which is not on disk (%v)",
					level, f.Name(), err)
				continue
			}
			if uint64(info.Size()) != f.Size {
				r.fail("manifest-file-size", "%s is %d bytes on disk, the manifest says %d",
					f.Name(), info.Size(), f.Size)
			}
		}
	}

	if !atRest {
		return nil
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("compaction: read dir %s: %w", dir, err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		number, ok := tableNumber(e.Name())
		if !ok || named[number] {
			continue
		}
		r.fail("no-orphan-tables", "%s is on disk but no level names it", e.Name())
	}
	return nil
}

// checkContents reads every file and verifies what is inside it against what
// the manifest claims, plus the two rules that only the contents can answer:
// entries in comparator order, and no user key at a level twice.
func checkContents(dir string, v *manifest.Version, r *Report) error {
	lastSeq := v.LastSequence()

	for level := 0; level < manifest.NumLevels; level++ {
		// Where each user key at this level was seen. Below L0 a key may
		// appear in at most one file, because the files do not overlap; at
		// L0 they do overlap and repetition is expected, so the rule does
		// not apply there.
		seen := map[string]uint64{}

		for _, f := range v.Files(level) {
			entries, err := readTable(dir, f)
			if err != nil {
				// A file the manifest names but that cannot be read is a
				// violation, not a checker failure.
				r.fail("table-readable", "%s: %v", f.Name(), err)
				continue
			}
			r.FilesChecked++
			r.EntriesChecked += len(entries)

			checkOneTable(f, entries, lastSeq, r)

			if level == 0 {
				continue
			}
			for _, e := range entries {
				key := string(e.Key)
				if prev, dup := seen[key]; dup && prev != f.Number {
					r.fail("one-version-per-key-per-level",
						"key %q appears at level %d in both %06d.sst and %s",
						key, level, prev, f.Name())
					continue
				}
				seen[key] = f.Number
			}
		}
	}
	return nil
}

// checkOneTable verifies a single file against its metadata.
func checkOneTable(f *manifest.FileMetadata, entries []memtable.Entry, lastSeq uint64, r *Report) {
	if len(entries) == 0 {
		r.fail("no-empty-tables", "%s holds no entries", f.Name())
		return
	}

	var prev memtable.Entry
	for i, e := range entries {
		if i > 0 && memtable.Compare(prev.Key, prev.Sequence, e.Key, e.Sequence) >= 0 {
			r.fail("comparator-order", "%s: entry %d (%q@%d) does not follow %q@%d",
				f.Name(), i, e.Key, e.Sequence, prev.Key, prev.Sequence)
		}
		prev = e

		// Sequence monotonicity, in the only sense that is checkable from a
		// tree at rest: no file may claim a sequence the database never
		// issued. A file above LastSequence would win against every future
		// write of the same key, which is the shape of the resurrection bug.
		if e.Sequence > lastSeq && lastSeq > 0 {
			r.fail("sequence-within-issued-range",
				"%s: key %q has sequence %d, above the manifest's last sequence %d",
				f.Name(), e.Key, e.Sequence, lastSeq)
		}

		if bytes.Compare(e.Key, f.Smallest) < 0 || bytes.Compare(e.Key, f.Largest) > 0 {
			r.fail("metadata-key-bounds", "%s: key %q is outside the recorded range %q..%q",
				f.Name(), e.Key, f.Smallest, f.Largest)
		}
		if e.Sequence < f.SmallestSeq || e.Sequence > f.LargestSeq {
			r.fail("metadata-sequence-bounds",
				"%s: key %q has sequence %d, outside the recorded range %d..%d",
				f.Name(), e.Key, e.Sequence, f.SmallestSeq, f.LargestSeq)
		}
	}
}

// checkL0Ordering verifies that a higher-numbered L0 file holds newer data.
//
// Only L0 carries this rule, and only L0 can: its files come from flushes,
// which happen in queue order, so file number and recency agree. The read
// path relies on exactly that -- it walks L0 newest file first and takes the
// first match -- so if the two ever disagreed, a stale value would shadow a
// fresh one.
func checkL0Ordering(v *manifest.Version, r *Report) {
	files := v.Files(0)
	for i := 1; i < len(files); i++ {
		newer, older := files[i-1], files[i]
		if newer.LargestSeq < older.LargestSeq {
			r.fail("l0-number-tracks-recency",
				"%s is numbered above %s but holds older sequences (%d against %d)",
				newer.Name(), older.Name(), newer.LargestSeq, older.LargestSeq)
		}
	}
}

// CheckResult verifies a finished compaction's own accounting: every key the
// merge surfaced was either written out or dropped as a tombstone.
//
// This is the "live data never shrinks except through deletes" rule, stated
// where it is cheap to check. A merge that lost a key satisfies every
// structural invariant above -- the tree stays perfectly well-formed with
// the key simply gone -- so the structural checks cannot catch it and this
// identity is the one that can.
func CheckResult(res *Result) error {
	if got, want := res.EntriesWritten+res.TombstonesDropped, res.Keys; got != want {
		return fmt.Errorf("compaction: accounting is short: %d keys merged, "+
			"%d written and %d tombstones dropped", want, res.EntriesWritten, res.TombstonesDropped)
	}
	return nil
}

// readTable reads every entry of a table in order.
func readTable(dir string, f *manifest.FileMetadata) ([]memtable.Entry, error) {
	t, err := sstable.Open(filepath.Join(dir, f.Name()))
	if err != nil {
		return nil, err
	}
	defer func() { _ = t.Close() }()

	var out []memtable.Entry
	it := t.NewIterator()
	for it.Next() {
		e := it.Entry()
		out = append(out, memtable.Entry{
			Key:       append([]byte(nil), e.Key...),
			Sequence:  e.Sequence,
			Value:     append([]byte(nil), e.Value...),
			Tombstone: e.Tombstone,
		})
	}
	return out, it.Err()
}

// tableNumber extracts the file number from an SSTable name, rejecting
// anything that is not exactly one so the checker never reports the WAL or
// the manifest as an orphan.
func tableNumber(name string) (uint64, bool) {
	digits, ok := strings.CutSuffix(name, ".sst")
	if !ok {
		return 0, false
	}
	number, err := strconv.ParseUint(digits, 10, 64)
	if err != nil || fmt.Sprintf("%06d.sst", number) != name {
		return 0, false
	}
	return number, true
}

// CheckDir validates a data directory from the outside, which is what
// `strata-cli validate` runs. It replays the manifest first, so it checks
// what a restart would actually see rather than what a running process
// believes.
func CheckDir(dir string) (*Report, error) {
	vs, err := manifest.Recover(dir)
	if err != nil {
		return nil, fmt.Errorf("compaction: recover %s: %w", dir, err)
	}
	return Check(dir, vs.Current())
}

// LevelSummary describes the tree's shape for the CLI, sorted by level.
type LevelSummary struct {
	Level int
	Files int
	Bytes uint64
}

// Summarise returns one entry per non-empty level.
func Summarise(v *manifest.Version) []LevelSummary {
	var out []LevelSummary
	for level := 0; level < manifest.NumLevels; level++ {
		if v.NumFiles(level) == 0 {
			continue
		}
		out = append(out, LevelSummary{
			Level: level,
			Files: v.NumFiles(level),
			Bytes: v.LevelBytes(level),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Level < out[j].Level })
	return out
}
