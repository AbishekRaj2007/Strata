package model

import (
	"math/rand"
	"testing"

	"github.com/AbishekRaj2007/Strata/internal/engine"
	"github.com/AbishekRaj2007/Strata/internal/wal"
)

// lsmSystem adapts the real engine to the harness, adding the Flush and
// Reopen controls the reference has no equivalent for.
type lsmSystem struct {
	dir  string
	opts engine.Options
	e    *engine.LSM
}

func newLSMSystem(t *testing.T, dir string, threshold int) *lsmSystem {
	t.Helper()
	opts := engine.Options{
		Dir:          dir,
		Threshold:    threshold,
		MaxImmutable: 4,
		SyncPolicy:   wal.SyncNever, // durability is the crash harness's job, not this one's
	}
	e, err := engine.Open(opts)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	s := &lsmSystem{dir: dir, opts: opts, e: e}
	t.Cleanup(func() {
		if s.e != nil {
			_ = s.e.Close()
		}
	})
	return s
}

func (s *lsmSystem) Put(k, v []byte) error         { return s.e.Put(k, v) }
func (s *lsmSystem) Get(k []byte) ([]byte, error)  { return s.e.Get(k) }
func (s *lsmSystem) Delete(k []byte) (bool, error) { return s.e.Delete(k) }

func (s *lsmSystem) Scan(cursor []byte, count int) (engine.ScanResult, error) {
	return s.e.Scan(cursor, count)
}

func (s *lsmSystem) Flush() error { return s.e.Flush() }

func (s *lsmSystem) Reopen() error {
	if err := s.e.Close(); err != nil {
		return err
	}
	e, err := engine.Open(s.opts)
	if err != nil {
		s.e = nil
		return err
	}
	s.e = e
	return nil
}

// runSequence replays ops against a fresh engine and reference.
func runSequence(t *testing.T, ops []Op, threshold int) error {
	t.Helper()
	sys := newLSMSystem(t, t.TempDir(), threshold)
	return Run(sys, NewReference(), ops)
}

// TestModelFiftyThousandOperations is T4.5's done-when: 50,000 random
// operations with zero divergence from the reference.
//
// A small memtable threshold is the point. It forces rotations and flushes
// far more often than a realistic configuration would, so the sequence spends
// its time in the states that are hard to get right -- keys split across a
// memtable and several tables, tombstones in one level shadowing values in
// another -- rather than in a single memtable that never spills.
func TestModelFiftyThousandOperations(t *testing.T) {
	if testing.Short() {
		t.Skip("50k operations is slow under -short")
	}

	ops := Generate(rand.New(rand.NewSource(1)), GenConfig{
		Ops:      50_000,
		KeySpace: 512,
		Weights:  LongRunWeights,
	})

	if err := runSequence(t, ops, 64<<10); err != nil {
		t.Fatalf("divergence in a 50,000-operation sequence: %v", err)
	}
}

// TestModelManySeeds runs shorter sequences across many seeds. Breadth over
// depth: a bug reachable only from an unusual early state is likelier to show
// up in fifty different starts than in one long run.
func TestModelManySeeds(t *testing.T) {
	for seed := int64(0); seed < 50; seed++ {
		ops := Generate(rand.New(rand.NewSource(seed)), GenConfig{
			Ops:      1_000,
			KeySpace: 64,
			Weights:  DefaultWeights,
		})
		if err := runSequence(t, ops, 4<<10); err != nil {
			t.Fatalf("seed %d diverged: %v\n\nsequence:\n%s", seed, err, FormatSequence(ops))
		}
	}
}

// TestShrinkerFindsAMinimalSequence is the other half of T4.5's done-when:
// the runner must shrink a failure to a minimal reproducing sequence.
//
// It is checked against an injected fault rather than a real bug, because the
// real engine does not currently have one. The predicate fails only when a
// specific key is read after being deleted, so the minimal reproduction is
// exactly two operations, and anything larger means the shrinker is not doing
// its job.
func TestShrinkerFindsAMinimalSequence(t *testing.T) {
	ops := Generate(rand.New(rand.NewSource(9)), GenConfig{
		Ops:      2_000,
		KeySpace: 32,
		Weights:  DefaultWeights,
	})

	// Plant the pattern the injected predicate objects to.
	ops = append(ops,
		Op{Kind: OpDelete, Key: "canary"},
		Op{Kind: OpGet, Key: "canary"},
	)

	// A synthetic failure: DEL "canary" followed at any distance by GET
	// "canary".
	stillFails := func(candidate []Op) bool {
		deleted := false
		for _, op := range candidate {
			if op.Kind == OpDelete && op.Key == "canary" {
				deleted = true
			}
			if op.Kind == OpGet && op.Key == "canary" && deleted {
				return true
			}
		}
		return false
	}

	if !stillFails(ops) {
		t.Fatal("the planted sequence does not trigger the injected failure")
	}

	minimal := Shrink(ops, stillFails)

	if !stillFails(minimal) {
		t.Fatal("shrinking produced a sequence that no longer fails")
	}
	if len(minimal) != 2 {
		t.Errorf("shrank %d operations to %d, want 2:\n%s", len(ops), len(minimal), FormatSequence(minimal))
	}
	t.Logf("shrank %d operations to %d:\n%s", len(ops), len(minimal), FormatSequence(minimal))
}

// TestModelShrinksARealDivergence wires the shrinker to the real engine, so
// that a genuine future divergence is reported as a minimal sequence rather
// than as fifty thousand lines. It passes today because there is nothing to
// shrink; its job is to be already working when there is.
func TestModelShrinksARealDivergence(t *testing.T) {
	ops := Generate(rand.New(rand.NewSource(3)), GenConfig{
		Ops:      2_000,
		KeySpace: 32,
		Weights:  DefaultWeights,
	})

	failed := func(candidate []Op) bool {
		return runSequence(t, candidate, 4<<10) != nil
	}

	if !failed(ops) {
		return // no divergence, which is the expected outcome
	}

	minimal := Shrink(ops, failed)
	t.Fatalf("engine diverged from the reference; minimal reproduction (%d ops):\n%s\n%v",
		len(minimal), FormatSequence(minimal), runSequence(t, minimal, 4<<10))
}
