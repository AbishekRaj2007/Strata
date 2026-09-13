package fault

import (
	"errors"
	"fmt"

	"github.com/AbishekRaj2007/Strata/internal/compaction"
	"github.com/AbishekRaj2007/Strata/internal/engine"
	"github.com/AbishekRaj2007/Strata/internal/vfs"
	"github.com/AbishekRaj2007/Strata/internal/wal"
)

// Acked is the set of writes the database said it had accepted.
//
// This is the only thing the sweep is entitled to check. A write whose Put
// returned an error may be present or absent afterwards and both are correct;
// a write whose Put returned nil must be there. Tracking operations *sent*
// rather than operations *acknowledged* is the classic way to write a
// durability test that reports failures the contract never promised to
// prevent -- and, worse, to get used to ignoring them.
type Acked struct {
	// Present maps key to the value that must be readable.
	Present map[string]string

	// Absent is the keys whose deletion was acknowledged.
	Absent map[string]bool
}

func newAcked() *Acked {
	return &Acked{Present: make(map[string]string), Absent: make(map[string]bool)}
}

func (a *Acked) put(k, v string) {
	a.Present[k] = v
	delete(a.Absent, k)
}

func (a *Acked) delete(k string) {
	delete(a.Present, k)
	a.Absent[k] = true
}

// Outcome is what one workload run produced.
type Outcome struct {
	Acked *Acked

	// Err is the first error the workload hit, if any. It is expected: the
	// sweep injects a fault precisely so that one operation fails.
	Err error

	// Ops is how many workload operations completed before that.
	Ops int
}

// Config parameterises the workload.
type Config struct {
	Dir string
	FS  vfs.FS

	// Ops is how many operations the workload performs. It is small on
	// purpose: the sweep runs the whole workload once per I/O operation it
	// performs, so the cost of the sweep is quadratic in this number.
	Ops int

	// Threshold is the memtable rotation size. It is tiny so that a short
	// workload still flushes, compacts and deletes WALs -- the sweep is
	// interested in the durable transitions, and a workload that never
	// leaves the memtable performs almost no I/O to fail.
	Threshold int
}

// RunWorkload drives a representative workload against a fresh engine and
// returns what it acknowledged.
//
// Representative means it reaches every durable transition: WAL append and
// fsync, memtable rotation, flush to SSTable, manifest append, WAL deletion,
// compaction, and a reopen that replays all of it. A workload that only
// writes would leave most of the engine's I/O untouched, and the operations it
// never performs are the ones the sweep cannot fail.
func RunWorkload(cfg Config) Outcome {
	if cfg.Ops <= 0 {
		cfg.Ops = 60
	}
	if cfg.Threshold <= 0 {
		cfg.Threshold = 1 << 10
	}

	out := Outcome{Acked: newAcked()}

	// SyncAlways is not a detail. Under any other policy a Put may return
	// before its record is on media, so "acknowledged" would not mean
	// durable and the sweep would have nothing to assert.
	opts := engine.Options{
		Dir:        cfg.Dir,
		Threshold:  cfg.Threshold,
		SyncPolicy: wal.SyncAlways,
		FS:         cfg.FS,
	}

	e, err := engine.Open(opts)
	if err != nil {
		out.Err = fmt.Errorf("open: %w", err)
		return out
	}

	reopen := func() error {
		if err := e.Close(); err != nil {
			return fmt.Errorf("close: %w", err)
		}
		e, err = engine.Open(opts)
		if err != nil {
			return fmt.Errorf("reopen: %w", err)
		}
		return nil
	}

	for i := 0; i < cfg.Ops && out.Err == nil; i++ {
		key := fmt.Sprintf("key-%03d", i%17)
		value := fmt.Sprintf("value-%03d-%s", i, pad)

		switch {
		case i%13 == 12:
			out.Err = reopen()

		case i%11 == 10:
			if err := e.Compact(); err != nil {
				out.Err = fmt.Errorf("compact: %w", err)
			}

		case i%7 == 6:
			if err := e.Flush(); err != nil {
				out.Err = fmt.Errorf("flush: %w", err)
			}

		case i%5 == 4:
			if _, err := e.Delete([]byte(key)); err != nil {
				out.Err = fmt.Errorf("delete %s: %w", key, err)
			} else {
				out.Acked.delete(key)
			}

		default:
			if err := e.Put([]byte(key), []byte(value)); err != nil {
				out.Err = fmt.Errorf("put %s: %w", key, err)
			} else {
				out.Acked.put(key, value)
			}
		}
		out.Ops = i + 1
	}

	// The close may itself fail, and that is not a new failure worth
	// reporting over the one that caused it.
	if err := e.Close(); err != nil && out.Err == nil {
		out.Err = fmt.Errorf("close: %w", err)
	}
	return out
}

// pad makes values long enough that a handful of writes fills the tiny
// memtable the sweep uses, without making the files large enough to slow it
// down.
const pad = "0123456789abcdef0123456789abcdef"

// Verify reopens dir on the real filesystem and checks that everything
// acknowledged is still true.
//
// It deliberately does not use the injector: the question is what is actually
// on disk, and asking through a filesystem that was lying a moment ago would
// answer a different one.
func Verify(dir string, acked *Acked) error {
	e, err := engine.Open(engine.Options{Dir: dir, SyncPolicy: wal.SyncAlways})
	if err != nil {
		return fmt.Errorf("recovery failed to open the database: %w", err)
	}
	closed := false
	defer func() {
		if !closed {
			_ = e.Close()
		}
	}()

	for k, want := range acked.Present {
		got, err := e.Get([]byte(k))
		if err != nil {
			return fmt.Errorf("acknowledged write %q is gone after recovery: %w", k, err)
		}
		if string(got) != want {
			return fmt.Errorf("acknowledged write %q came back as %q, want %q", k, got, want)
		}
	}
	for k := range acked.Absent {
		_, err := e.Get([]byte(k))
		if err == nil {
			return fmt.Errorf("acknowledged delete of %q was resurrected by recovery", k)
		}
		if !errors.Is(err, engine.ErrNotFound) {
			return fmt.Errorf("reading deleted key %q after recovery: %w", k, err)
		}
	}

	// Structural invariants too. A database that returns the right answers
	// over a tree with overlapping L1 files is one compaction away from
	// returning wrong ones.
	//
	// The engine is closed first so the check runs at rest, which is the
	// only state in which the orphan rule holds: a running process
	// deliberately creates a durable table before the manifest names it.
	closed = true
	if err := e.Close(); err != nil {
		return fmt.Errorf("close after recovery: %w", err)
	}
	report, err := compaction.CheckDir(dir)
	if err != nil {
		return fmt.Errorf("invariant check: %w", err)
	}
	return report.Err()
}
