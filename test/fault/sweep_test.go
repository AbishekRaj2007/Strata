package fault

import (
	"testing"
	"time"

	"github.com/AbishekRaj2007/Strata/internal/vfs"
)

// TestWorkloadIsRepresentative pins what the sweep is sweeping.
//
// A sweep over a workload that never compacts proves nothing about
// compaction, and the failure mode is silent: the sweep still passes, just
// over less. Asserting the shape of the I/O keeps the coverage claim honest.
func TestWorkloadIsRepresentative(t *testing.T) {
	in := vfs.NewInjector(nil)
	in.Trace(true)

	out := RunWorkload(Config{Dir: t.TempDir(), FS: in, Ops: sweepOps})
	if out.Err != nil {
		t.Fatalf("the unfaulted workload failed: %v", out.Err)
	}

	kinds := make(map[vfs.OpKind]int)
	suffixes := make(map[string]int)
	for _, rec := range in.Trace(false) {
		kinds[rec.Kind]++
		switch {
		case len(rec.Path) > 4 && rec.Path[len(rec.Path)-4:] == ".wal":
			suffixes["wal"]++
		case len(rec.Path) > 4 && rec.Path[len(rec.Path)-4:] == ".sst":
			suffixes["sst"]++
		}
	}

	for _, kind := range []vfs.OpKind{
		vfs.OpCreate, vfs.OpOpen, vfs.OpOpenDir, vfs.OpWrite,
		vfs.OpReadAt, vfs.OpSync, vfs.OpClose, vfs.OpRemove, vfs.OpReadDir,
	} {
		if kinds[kind] == 0 {
			t.Errorf("the workload never performs a %s", kind)
		}
	}
	for _, want := range []string{"wal", "sst"} {
		if suffixes[want] == 0 {
			t.Errorf("the workload never touches a .%s file", want)
		}
	}
	t.Logf("%d I/O operations: %v", in.Count(), kinds)
}

// sweepOps is the workload length. The sweep runs the whole workload once per
// I/O operation the workload performs, so its cost is quadratic in this.
const sweepOps = 40

// TestFaultSweep is T7.2's done-when: every I/O operation in a representative
// workload is failed individually, and the database recovers in every case.
//
// "Recovers" is a specific claim, and the loose readings of it are what make
// most fault injection suites worthless. It does not mean the workload
// survived -- the workload is expected to fail, that is the point. It means
// that after the failure, reopening the directory on a healthy filesystem
// succeeds, every acknowledged write is still there, every acknowledged
// delete is still gone, and the tree's structural invariants hold.
func TestFaultSweep(t *testing.T) {
	if testing.Short() {
		t.Skip("the sweep runs the workload once per I/O operation")
	}

	// A first unfaulted run to count the operations there are to fail.
	baseline := vfs.NewInjector(nil)
	if out := RunWorkload(Config{Dir: t.TempDir(), FS: baseline, Ops: sweepOps}); out.Err != nil {
		t.Fatalf("the unfaulted workload failed: %v", out.Err)
	}
	total := baseline.Count()
	if total == 0 {
		t.Fatal("the workload performed no I/O")
	}

	// Every mode, cycled across the indices rather than run as four full
	// sweeps. A full sweep per mode would quadruple an already quadratic
	// test for a much smaller increase in what it finds: what matters is
	// that each mode reaches every kind of operation, which cycling
	// achieves because the modes and the operation kinds are coprime in
	// practice.
	modes := []vfs.Fault{
		{Mode: vfs.ModeError},
		{Mode: vfs.ModeTorn},
		{Mode: vfs.ModeShort},
		{Mode: vfs.ModeLatency, Delay: time.Millisecond},
	}

	var failed, neverFired int
	start := time.Now()

	for i := int64(0); i < total; i++ {
		fault := modes[int(i)%len(modes)]

		dir := t.TempDir()
		in := vfs.NewInjector(nil)
		in.ArmAt(i, fault)

		out := RunWorkload(Config{Dir: dir, FS: in, Ops: sweepOps})

		rec, fired := in.Fired()
		if !fired {
			// The engine flushes and compacts on their own goroutines, so
			// the operation at a given index can shift between runs and a
			// late index is sometimes never reached. Counted rather than
			// ignored: a sweep where this is common is testing far less
			// than it claims.
			neverFired++
			continue
		}

		if err := Verify(dir, out.Acked); err != nil {
			failed++
			t.Errorf("fault %s at %s: %v\n  workload stopped after %d operations with: %v",
				fault.Mode, rec, err, out.Ops, out.Err)
			if failed >= 10 {
				t.Fatalf("stopping after %d failures", failed)
			}
		}
	}

	t.Logf("failed %d I/O operations individually in %s; %d never fired",
		total, time.Since(start).Truncate(time.Millisecond), neverFired)

	// A sweep that mostly missed is a sweep that proved little, and it must
	// not be able to pass quietly.
	if limit := int(total / 10); neverFired > limit {
		t.Errorf("%d of %d injected faults never fired, more than the %d tolerated for scheduling drift",
			neverFired, total, limit)
	}
}

// TestSweepCatchesABrokenRecovery is the sweep's own control.
//
// Without it, a passing sweep might mean the engine is durable or might mean
// Verify does not look at anything. Corrupting what recovery reads proves the
// verification has teeth.
func TestSweepCatchesABrokenRecovery(t *testing.T) {
	dir := t.TempDir()

	out := RunWorkload(Config{Dir: dir, Ops: sweepOps})
	if out.Err != nil {
		t.Fatalf("the unfaulted workload failed: %v", out.Err)
	}
	if err := Verify(dir, out.Acked); err != nil {
		t.Fatalf("a clean run did not verify: %v", err)
	}

	// Claim a write that was never acknowledged. Verify must object.
	out.Acked.put("never-written", "never-acknowledged")
	if err := Verify(dir, out.Acked); err == nil {
		t.Fatal("Verify accepted a database missing an acknowledged write")
	} else {
		t.Logf("Verify correctly objected: %v", err)
	}
}
