package model

import (
	"math/rand"
	"testing"
)

// resurrectingSystem is the artificial bug T7.1's done-when calls for: a
// compaction that drops a tombstone while an older value for the same key
// still lives in a lower level, so the deleted value comes back.
//
// It is modelled on the real failure rather than invented, because the point
// of injecting a bug is to prove the harness would catch the bug it is meant
// to catch. Premature tombstone dropping is the mistake a leveled compactor
// is most likely to make and the one whose symptom -- data returning from the
// dead, long after the delete, only sometimes -- is hardest to find any other
// way.
//
// The wrapper reproduces the symptom without breaking the engine: it replays
// the last known value for every deleted key after each compaction. What the
// harness sees is indistinguishable from a compactor that dropped the
// tombstone too early.
type resurrectingSystem struct {
	*lsmSystem

	last    map[string]string
	deleted map[string]bool
}

func newResurrectingSystem(t *testing.T, dir string, threshold int) *resurrectingSystem {
	return &resurrectingSystem{
		lsmSystem: newLSMSystem(t, dir, threshold),
		last:      make(map[string]string),
		deleted:   make(map[string]bool),
	}
}

func (r *resurrectingSystem) Put(k, v []byte) error {
	r.last[string(k)] = string(v)
	delete(r.deleted, string(k))
	return r.lsmSystem.Put(k, v)
}

func (r *resurrectingSystem) Delete(k []byte) (bool, error) {
	r.deleted[string(k)] = true
	return r.lsmSystem.Delete(k)
}

func (r *resurrectingSystem) Compact() error {
	if err := r.lsmSystem.Compact(); err != nil {
		return err
	}
	for k := range r.deleted {
		v, ok := r.last[k]
		if !ok {
			continue // never had a value to resurrect
		}
		if err := r.lsmSystem.Put([]byte(k), []byte(v)); err != nil {
			return err
		}
		delete(r.deleted, k)
	}
	return nil
}

// TestInjectedBugIsCaughtAndShrunk is the second half of T7.1's done-when:
// an artificially injected bug is caught, and shrunk to under 20 operations.
//
// The shrinker's output is the deliverable, not the catch. A harness that
// reports "sequence of 400 operations diverged" has told you only that
// something is wrong. One that reports PUT, DEL, COMPACT, GET has told you
// what is wrong.
func TestInjectedBugIsCaughtAndShrunk(t *testing.T) {
	if testing.Short() {
		t.Skip("shrinking runs the engine once per candidate sequence")
	}

	ops := injectedSequence()

	// A fresh engine and a fresh reference per attempt: shrinking against a
	// reused directory measures the residue of previous attempts, not the
	// candidate. Each is closed as soon as its verdict is in, because the
	// shrinker runs thousands of them.
	failed := func(candidate []Op) bool {
		sys := newResurrectingSystem(t, t.TempDir(), 4<<10)
		defer sys.close()
		return Run(sys, NewReference(), candidate) != nil
	}

	if !failed(ops) {
		t.Fatal("the injected bug was not caught; the harness cannot see a resurrected key")
	}

	minimal := Shrink(ops, failed)

	if !failed(minimal) {
		t.Fatal("shrinking produced a sequence that no longer fails")
	}
	if len(minimal) >= 20 {
		t.Errorf("shrank %d operations to %d, want under 20:\n%s",
			len(ops), len(minimal), FormatSequence(minimal))
	}

	// The shape matters as much as the length: the minimal sequence has to
	// contain the delete and the compaction that resurrects it, or the
	// shrinker found some other way to fail.
	var sawDelete, sawCompact bool
	for _, op := range minimal {
		switch op.Kind {
		case OpDelete:
			sawDelete = true
		case OpCompact:
			sawCompact = true
		}
	}
	if !sawDelete || !sawCompact {
		t.Errorf("minimal sequence lacks the delete/compaction pair:\n%s", FormatSequence(minimal))
	}

	sys := newResurrectingSystem(t, t.TempDir(), 4<<10)
	defer sys.close()
	t.Logf("shrank %d operations to %d:\n%s\n%v",
		len(ops), len(minimal), FormatSequence(minimal), Run(sys, NewReference(), minimal))
}

// injectedSequence is the sequence both the injected-bug test and its control
// replay, so that a difference between them is the bug and nothing else.
//
// It is short on purpose. The shrinker runs the engine once per candidate and
// delta debugging needs O(n log n) candidates, so doubling the starting
// length roughly doubles a test that already dominates the package.
func injectedSequence() []Op {
	return Generate(rand.New(rand.NewSource(17)), GenConfig{
		Ops:          120,
		KeySpace:     8,
		Weights:      StructuralWeights,
		Distribution: DistZipfian,
	})
}

// TestCleanEngineSurvivesTheSameSequence is the control. Without it, a pass
// above could mean the harness objects to something in the sequence that has
// nothing to do with the injected bug.
func TestCleanEngineSurvivesTheSameSequence(t *testing.T) {
	ops := injectedSequence()

	if err := runSequence(t, ops, 4<<10); err != nil {
		t.Fatalf("the unmodified engine diverged on the control sequence: %v", err)
	}
}

// compile-time proof the wrapper still satisfies the harness contract.
var _ System = (*resurrectingSystem)(nil)
