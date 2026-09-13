// Package model compares the real engine against a trivially correct
// reference implementation under randomised operation sequences.
package model

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/AbishekRaj2007/Strata/internal/engine"
)

// Reference is the obviously-correct engine: a sorted map with no durability,
// no levels, and no concurrency.
//
// Its whole value is that it is too simple to be wrong. Every divergence
// between it and the real engine is therefore a bug in the real engine, which
// is what makes a failure here worth acting on rather than worth arguing
// about.
type Reference struct {
	data map[string]string

	// sorted is the key set in order, rebuilt lazily after a mutation.
	//
	// Caching it is not an optimisation for its own sake: a full scan pages
	// through the key set, and sorting every key on every page made the
	// reference quadratic in the database size. At soak scale that cost
	// dominated the engine it exists to check, which is the wrong thing for
	// a harness to spend its time on.
	sorted []string
	dirty  bool
}

func NewReference() *Reference {
	return &Reference{data: make(map[string]string), dirty: true}
}

func (r *Reference) Put(key, value []byte) error {
	if _, existed := r.data[string(key)]; !existed {
		r.dirty = true
	}
	r.data[string(key)] = string(value)
	return nil
}

func (r *Reference) Get(key []byte) ([]byte, error) {
	v, ok := r.data[string(key)]
	if !ok {
		return nil, engine.ErrNotFound
	}
	return []byte(v), nil
}

func (r *Reference) Delete(key []byte) (bool, error) {
	_, existed := r.data[string(key)]
	if existed {
		r.dirty = true
	}
	delete(r.data, string(key))
	return existed, nil
}

// keys returns the key set in order.
func (r *Reference) keys() []string {
	if r.dirty {
		r.sorted = r.sorted[:0]
		for k := range r.data {
			r.sorted = append(r.sorted, k)
		}
		sort.Strings(r.sorted)
		r.dirty = false
	}
	return r.sorted
}

// Scan mirrors the engine's contract: keys strictly after the cursor, in
// order, with a nil cursor meaning the iteration is complete.
func (r *Reference) Scan(cursor []byte, count int) (engine.ScanResult, error) {
	if count <= 0 {
		count = 10
	}

	keys := r.keys()

	start := 0
	if cursor != nil {
		start = sort.SearchStrings(keys, string(cursor))
		for start < len(keys) && keys[start] <= string(cursor) {
			start++
		}
	}
	if start >= len(keys) {
		return engine.ScanResult{}, nil
	}

	end := start + count
	if end > len(keys) {
		end = len(keys)
	}

	page := make([][]byte, 0, end-start)
	for _, k := range keys[start:end] {
		page = append(page, []byte(k))
	}

	var next []byte
	if end < len(keys) {
		next = append([]byte(nil), keys[end-1]...)
	}
	return engine.ScanResult{Keys: page, Cursor: next}, nil
}

// OpKind names an operation in a generated sequence.
type OpKind int

const (
	OpPut OpKind = iota
	OpGet
	OpDelete
	OpScan
	OpFlush
	OpCompact
	OpReopen
)

func (k OpKind) String() string {
	switch k {
	case OpPut:
		return "PUT"
	case OpGet:
		return "GET"
	case OpDelete:
		return "DEL"
	case OpScan:
		return "SCAN"
	case OpFlush:
		return "FLUSH"
	case OpCompact:
		return "COMPACT"
	case OpReopen:
		return "REOPEN"
	}
	return "?"
}

// Op is one generated operation.
type Op struct {
	Kind  OpKind
	Key   string
	Value string
	Count int
}

func (o Op) String() string {
	switch o.Kind {
	case OpPut:
		return fmt.Sprintf("PUT %q %q", o.Key, o.Value)
	case OpGet:
		return fmt.Sprintf("GET %q", o.Key)
	case OpDelete:
		return fmt.Sprintf("DEL %q", o.Key)
	case OpScan:
		return fmt.Sprintf("SCAN count=%d", o.Count)
	default:
		return o.Kind.String()
	}
}

// FormatSequence renders a sequence as a runnable-looking script, which is
// what a failure report needs to be actionable.
func FormatSequence(ops []Op) string {
	var b strings.Builder
	for i, op := range ops {
		fmt.Fprintf(&b, "%3d: %s\n", i, op)
	}
	return b.String()
}

// Divergence describes the first operation whose result differed.
type Divergence struct {
	Index int
	Op    Op
	Want  string
	Got   string
}

func (d Divergence) Error() string {
	return fmt.Sprintf("op %d (%s): engine returned %s, reference returned %s",
		d.Index, d.Op, d.Got, d.Want)
}

// System is what a run drives: the real engine plus the two controls the
// reference has no equivalent for.
type System interface {
	Put(key, value []byte) error
	Get(key []byte) ([]byte, error)
	Delete(key []byte) (bool, error)
	Scan(cursor []byte, count int) (engine.ScanResult, error)

	// Flush forces the active memtable to an SSTable.
	Flush() error

	// Compact settles the whole tree, so that a sequence can put a
	// compaction at a chosen point rather than wait for one.
	Compact() error

	// Reopen closes and reopens the engine, so recovery is exercised inside
	// the operation sequence rather than only at the end.
	Reopen() error
}

// Run replays ops against both implementations and reports the first
// divergence, or nil if they agreed throughout.
//
// Flush, Compact and Reopen have no reference equivalent by design: they must
// be invisible. A durable engine that returns different answers after a
// flush, a compaction or a restart is exactly the failure this harness exists
// to catch, so the reference simply ignores them and any resulting difference
// shows up as a divergence on the next read.
func Run(sys System, ref *Reference, ops []Op) error {
	for i, op := range ops {
		switch op.Kind {
		case OpPut:
			if err := sys.Put([]byte(op.Key), []byte(op.Value)); err != nil {
				return Divergence{Index: i, Op: op, Want: "no error", Got: err.Error()}
			}
			_ = ref.Put([]byte(op.Key), []byte(op.Value))

		case OpGet:
			got, gotErr := sys.Get([]byte(op.Key))
			want, wantErr := ref.Get([]byte(op.Key))

			gotMissing := errors.Is(gotErr, engine.ErrNotFound)
			wantMissing := errors.Is(wantErr, engine.ErrNotFound)
			if gotErr != nil && !gotMissing {
				return Divergence{Index: i, Op: op, Want: describeGet(want, wantMissing), Got: gotErr.Error()}
			}
			if gotMissing != wantMissing || (!gotMissing && string(got) != string(want)) {
				return Divergence{Index: i, Op: op,
					Want: describeGet(want, wantMissing),
					Got:  describeGet(got, gotMissing)}
			}

		case OpDelete:
			got, err := sys.Delete([]byte(op.Key))
			if err != nil {
				return Divergence{Index: i, Op: op, Want: "no error", Got: err.Error()}
			}
			want, _ := ref.Delete([]byte(op.Key))
			if got != want {
				return Divergence{Index: i, Op: op,
					Want: fmt.Sprintf("existed=%v", want),
					Got:  fmt.Sprintf("existed=%v", got)}
			}

		case OpScan:
			got, err := scanAll(sys, op.Count)
			if err != nil {
				return Divergence{Index: i, Op: op, Want: "no error", Got: err.Error()}
			}
			want, err := scanAllRef(ref, op.Count)
			if err != nil {
				return Divergence{Index: i, Op: op, Want: "no error", Got: err.Error()}
			}
			if !equalStrings(got, want) {
				return Divergence{Index: i, Op: op,
					Want: fmt.Sprintf("%d keys %v", len(want), truncate(want)),
					Got:  fmt.Sprintf("%d keys %v", len(got), truncate(got))}
			}

		case OpFlush:
			if err := sys.Flush(); err != nil {
				return Divergence{Index: i, Op: op, Want: "no error", Got: err.Error()}
			}

		case OpCompact:
			if err := sys.Compact(); err != nil {
				return Divergence{Index: i, Op: op, Want: "no error", Got: err.Error()}
			}

		case OpReopen:
			if err := sys.Reopen(); err != nil {
				return Divergence{Index: i, Op: op, Want: "no error", Got: err.Error()}
			}
		}
	}
	return nil
}

func describeGet(v []byte, missing bool) string {
	if missing {
		return "not found"
	}
	return fmt.Sprintf("%q", v)
}

// scanAll pages a full iteration, which is what a client actually does and so
// the thing worth comparing.
func scanAll(sys System, count int) ([]string, error) {
	var out []string
	var cursor []byte
	for i := 0; ; i++ {
		if i > 100_000 {
			return nil, fmt.Errorf("scan did not terminate")
		}
		res, err := sys.Scan(cursor, count)
		if err != nil {
			return nil, err
		}
		for _, k := range res.Keys {
			out = append(out, string(k))
		}
		if res.Cursor == nil {
			return out, nil
		}
		cursor = res.Cursor
	}
}

func scanAllRef(ref *Reference, count int) ([]string, error) {
	var out []string
	var cursor []byte
	for i := 0; ; i++ {
		if i > 100_000 {
			return nil, fmt.Errorf("reference scan did not terminate")
		}
		res, err := ref.Scan(cursor, count)
		if err != nil {
			return nil, err
		}
		for _, k := range res.Keys {
			out = append(out, string(k))
		}
		if res.Cursor == nil {
			return out, nil
		}
		cursor = res.Cursor
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func truncate(keys []string) []string {
	if len(keys) <= 8 {
		return keys
	}
	return append(append([]string{}, keys[:8]...), "...")
}
