package compaction

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/AbishekRaj2007/Strata/internal/manifest"
	"github.com/AbishekRaj2007/Strata/internal/memtable"
	"github.com/AbishekRaj2007/Strata/internal/sstable"
)

// newExecutor returns an executor over dir with a file-number allocator
// starting above the numbers the test's input tables used.
func newExecutor(dir string, next uint64, opts Options) *Executor {
	n := next
	return &Executor{
		Dir:  dir,
		Opts: opts,
		NextFileNumber: func() uint64 {
			n++
			return n - 1
		},
	}
}

// tableEntries reads every entry of an output table in order.
func tableEntries(t *testing.T, dir string, f *manifest.FileMetadata) []memtable.Entry {
	t.Helper()

	tbl, err := sstable.Open(filepath.Join(dir, f.Name()))
	if err != nil {
		t.Fatalf("open %s: %v", f.Name(), err)
	}
	defer func() { _ = tbl.Close() }()

	var out []memtable.Entry
	for it := tbl.NewIterator(); it.Next(); {
		e := it.Entry()
		out = append(out, memtable.Entry{
			Key:       append([]byte(nil), e.Key...),
			Sequence:  e.Sequence,
			Value:     append([]byte(nil), e.Value...),
			Tombstone: e.Tombstone,
		})
	}
	if err := tbl.NewIterator().Err(); err != nil {
		t.Fatalf("iterate %s: %v", f.Name(), err)
	}
	return out
}

func TestExecutorKeepsOnlyTheNewestVersion(t *testing.T) {
	dir := t.TempDir()

	// Two overlapping inputs, each holding a different version of "b".
	older := writeTable(t, dir, 1, []memtable.Entry{
		entry("a", 1, "a1"),
		entry("b", 2, "b-old"),
	})
	newer := writeTable(t, dir, 2, []memtable.Entry{
		entry("b", 5, "b-new"),
		entry("c", 6, "c1"),
	})

	c := &Compaction{Level: 0, Base: []*manifest.FileMetadata{newer, older}, BottomMost: true}

	res, err := newExecutor(dir, 3, Options{}).Run(c)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(res.Outputs) != 1 {
		t.Fatalf("outputs = %d, want 1", len(res.Outputs))
	}

	got := tableEntries(t, dir, res.Outputs[0])
	want := []string{"a=a1", "b=b-new", "c=c1"}
	var have []string
	for _, e := range got {
		have = append(have, fmt.Sprintf("%s=%s", e.Key, e.Value))
	}
	if strings.Join(have, ",") != strings.Join(want, ",") {
		t.Errorf("output = %v, want %v", have, want)
	}

	if res.Keys != 3 || res.EntriesWritten != 3 {
		t.Errorf("keys/written = %d/%d, want 3/3", res.Keys, res.EntriesWritten)
	}
	// The superseded version of "b" is not counted anywhere -- the merge
	// collapses it internally -- but it is reclaimed, and the bytes show it.
	if res.BytesWritten >= res.BytesRead {
		t.Errorf("wrote %d bytes from %d read; merging two tables into one should reclaim",
			res.BytesWritten, res.BytesRead)
	}
}

// The done-when condition, run through the real executor rather than the
// hand-rolled merge the tombstone harness uses.
func TestExecutorNeverResurrectsADeletedKey(t *testing.T) {
	// The two cases are the two states the picker can report, each with the
	// tree that justifies it. Pairing them the other way round is the bug:
	// dropping a tombstone while a level below still holds the key.
	cases := []struct {
		name string
		// deepValue places an older version of "k" at a level below the
		// output, which is exactly what makes BottomMost false.
		deepValue  bool
		bottomMost bool
		wantKept   bool
	}{
		{
			name:       "an overlapping level below keeps the tombstone",
			deepValue:  true,
			bottomMost: false,
			wantKept:   true,
		},
		{
			name:       "nothing below reclaims it",
			deepValue:  false,
			bottomMost: true,
			wantKept:   false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()

			levels := [][]*manifest.FileMetadata{nil}
			if tc.deepValue {
				deep := writeTable(t, dir, 1, []memtable.Entry{entry("k", 1, "the old value")})
				levels = append(levels, []*manifest.FileMetadata{deep})
			}
			grave := writeTable(t, dir, 2, []memtable.Entry{deletion("k", 2)})

			c := &Compaction{
				Level:      1,
				Base:       []*manifest.FileMetadata{grave},
				BottomMost: tc.bottomMost,
			}
			res, err := newExecutor(dir, 3, Options{}).Run(c)
			if err != nil {
				t.Fatalf("run: %v", err)
			}

			// Either way the key must stay deleted. What differs is whether
			// the tombstone still has to be on disk to keep it that way.
			levels[0] = res.Outputs
			if value, visible := readLevels(t, dir, levels, "k"); visible {
				t.Errorf("the deleted key reads back as %q", value)
			}

			kept := res.TombstonesDropped == 0
			if kept != tc.wantKept {
				t.Errorf("tombstone kept = %v, want %v", kept, tc.wantKept)
			}
		})
	}
}

// The second half of T6.2's done-when: no output file may split a user key.
//
// The roll target is set to a single byte so the executor rolls at every
// opportunity -- the most hostile setting for this invariant.
func TestExecutorNeverSplitsAUserKeyAcrossOutputs(t *testing.T) {
	dir := t.TempDir()

	// Many versions of a few keys, spread over several inputs, so the merge
	// meets each key repeatedly and could roll in the middle of one.
	var inputs []*manifest.FileMetadata
	number := uint64(1)
	for file := 0; file < 4; file++ {
		var entries []memtable.Entry
		for k := 0; k < 8; k++ {
			key := fmt.Sprintf("key%02d", k)
			entries = append(entries, entry(key, uint64(file*100+k+1), strings.Repeat("v", 200)))
		}
		inputs = append(inputs, writeTable(t, dir, number, entries))
		number++
	}

	c := &Compaction{Level: 0, Base: inputs, BottomMost: true}
	res, err := newExecutor(dir, number, Options{TargetFileBytes: 1}).Run(c)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(res.Outputs) < 2 {
		t.Fatalf("outputs = %d; a 1-byte roll target should have produced several", len(res.Outputs))
	}

	where := map[string]uint64{}
	for _, out := range res.Outputs {
		for _, e := range tableEntries(t, dir, out) {
			key := string(e.Key)
			if prev, seen := where[key]; seen && prev != out.Number {
				t.Errorf("key %q appears in files %d and %d", key, prev, out.Number)
			}
			where[key] = out.Number
		}
	}
	if len(where) != 8 {
		t.Errorf("output holds %d distinct keys, want 8", len(where))
	}
}

// Outputs go into a level that guarantees non-overlap, so they must come out
// in key order with disjoint ranges. A roll that produced overlapping files
// would be rejected by the version the commit path builds -- better to catch
// it in the merge, where the cause is visible.
func TestExecutorOutputsAreOrderedAndDisjoint(t *testing.T) {
	dir := t.TempDir()

	var entries []memtable.Entry
	for k := 0; k < 400; k++ {
		entries = append(entries, entry(fmt.Sprintf("key%04d", k), uint64(k+1), strings.Repeat("v", 64)))
	}
	in := writeTable(t, dir, 1, entries)

	c := &Compaction{Level: 1, Base: []*manifest.FileMetadata{in}, BottomMost: true}
	res, err := newExecutor(dir, 2, Options{TargetFileBytes: 4096}).Run(c)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(res.Outputs) < 2 {
		t.Fatalf("outputs = %d, want several", len(res.Outputs))
	}

	if !sort.SliceIsSorted(res.Outputs, func(i, j int) bool {
		return string(res.Outputs[i].Smallest) < string(res.Outputs[j].Smallest)
	}) {
		t.Error("outputs are not in key order")
	}
	for i := 1; i < len(res.Outputs); i++ {
		prev, cur := res.Outputs[i-1], res.Outputs[i]
		if string(prev.Largest) >= string(cur.Smallest) {
			t.Errorf("outputs %d and %d overlap: %q..%q then %q..%q",
				prev.Number, cur.Number, prev.Smallest, prev.Largest, cur.Smallest, cur.Largest)
		}
	}

	// And every key survived the roll.
	seen := 0
	for _, out := range res.Outputs {
		seen += len(tableEntries(t, dir, out))
	}
	if seen != 400 {
		t.Errorf("output holds %d entries, want all 400", seen)
	}
}

// A compaction that is nothing but tombstones reaching the bottom reclaims
// everything and should leave no file behind at all -- not an empty one.
func TestExecutorWritesNoFileWhenEverythingIsReclaimed(t *testing.T) {
	dir := t.TempDir()

	in := writeTable(t, dir, 1, []memtable.Entry{deletion("a", 1), deletion("b", 2)})

	c := &Compaction{Level: 1, Base: []*manifest.FileMetadata{in}, BottomMost: true}
	res, err := newExecutor(dir, 2, Options{}).Run(c)
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	if len(res.Outputs) != 0 {
		t.Errorf("outputs = %d, want none", len(res.Outputs))
	}
	if res.TombstonesDropped != 2 {
		t.Errorf("dropped %d tombstones, want 2", res.TombstonesDropped)
	}

	// Nothing should be left in the directory but the input.
	names, err := filepath.Glob(filepath.Join(dir, "*.sst"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(names) != 1 {
		t.Errorf("directory holds %v, want only the input table", names)
	}
}

// A merge that fails partway must not leave its outputs behind. The failure
// is forced by an allocator that hands out a number already on disk, which
// Create refuses.
func TestExecutorRemovesOutputsAfterAFailure(t *testing.T) {
	dir := t.TempDir()

	var entries []memtable.Entry
	for k := 0; k < 200; k++ {
		entries = append(entries, entry(fmt.Sprintf("key%04d", k), uint64(k+1), strings.Repeat("v", 64)))
	}
	in := writeTable(t, dir, 1, entries)

	// 2 and 3 are free; the fourth allocation collides with the input.
	numbers := []uint64{2, 3, 4, 1}
	i := 0
	e := &Executor{
		Dir:  dir,
		Opts: Options{TargetFileBytes: 1024},
		NextFileNumber: func() uint64 {
			n := numbers[min(i, len(numbers)-1)]
			i++
			return n
		},
	}

	c := &Compaction{Level: 1, Base: []*manifest.FileMetadata{in}, BottomMost: true}
	if _, err := e.Run(c); err == nil {
		t.Fatal("run succeeded despite a colliding file number")
	}

	names, err := filepath.Glob(filepath.Join(dir, "*.sst"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(names) != 1 || filepath.Base(names[0]) != "000001.sst" {
		t.Errorf("directory holds %v, want only the untouched input", names)
	}
}
