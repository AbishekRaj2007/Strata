package compaction

import (
	"errors"
	"sync"
	"time"

	"github.com/AbishekRaj2007/Strata/internal/log"
	"github.com/AbishekRaj2007/Strata/internal/manifest"
)

// ErrStopped is returned to a writer waiting on backpressure when the
// scheduler shuts down. The write has not happened and must not be
// acknowledged.
var ErrStopped = errors.New("compaction: scheduler stopped")

// ErrCanceled is what the executor returns when a shutdown interrupts a merge
// in flight. It is a clean outcome, not a failure: the outputs are removed
// and nothing was committed, so the tree is exactly as it was.
var ErrCanceled = errors.New("compaction: canceled")

// DefaultSoftDelay is the pause added per L0 file above the soft threshold.
// Small enough to be invisible per write, large enough that a sustained
// overload is felt before the hard stall arrives.
const DefaultSoftDelay = 500 * time.Microsecond

// Scheduler runs compactions on a dedicated goroutine and applies
// backpressure to writers when they outrun it.
//
// Stalling writes is counterintuitive, and it is the point of this type. If
// the write path is allowed to outrun compaction indefinitely, L0 grows
// without bound -- and every L0 file must be consulted on every read whose
// key range it admits, so read latency degrades for everyone, without limit,
// with no signal that anything is wrong. Slowing writers converts that into a
// bounded, measurable, reportable cost paid by the workload causing it. A
// stall is a designed outcome, not a failure, which is why the count and the
// total duration are reported in INFO rather than logged and forgotten.
//
// The two thresholds do different jobs. The soft one adds a small delay that
// grows with the backlog, which is enough to match a writer to compaction
// throughput without a visible pause. The hard one blocks outright, and
// exists because a delay that scales smoothly still has a maximum, and a
// workload can exceed it.
type Scheduler struct {
	versions  *manifest.VersionSet
	picker    *Picker
	executor  *Executor
	committer *Committer
	logger    log.Logger
	opts      Options

	softThreshold int
	hardThreshold int
	softDelay     time.Duration

	trigger chan struct{}
	quit    chan struct{}
	wg      sync.WaitGroup

	// runMu serialises whole compactions. Pick, Run and Commit are each safe
	// on their own, but a compaction spans all three, and two overlapping
	// ones pick from the same version, choose the same files, and the second
	// to commit names inputs the first already deleted.
	//
	// It is needed because compaction has two drivers: the background
	// goroutine, and any caller of Drain -- which is what FLUSHDB and
	// COMPACT use to settle the tree synchronously. Compaction is serial by
	// design anyway, so the lock costs nothing it was not already paying.
	runMu sync.Mutex

	// mu guards everything below, and is the lock stalled writers wait on.
	mu      sync.Mutex
	cond    *sync.Cond
	stopped bool
	err     error
	stats   Stats
}

// Stats is what the scheduler reports to INFO.
type Stats struct {
	// L0Files is the current L0 file count, the number backpressure is
	// decided from.
	L0Files int

	// Compactions is how many have committed, and CompactionErrors how many
	// failed. A non-zero error count means the tree is no longer being
	// maintained, whatever the other numbers say.
	Compactions      uint64
	CompactionErrors uint64

	// Stalls counts writers held at the hard threshold and StallDuration
	// their total wait. Reported as a count and a total rather than a rate
	// so one long stall can be told from many short ones: they point at
	// different problems.
	Stalls        uint64
	StallDuration time.Duration

	// SoftDelays counts writers slowed short of a full stall, and
	// SoftDelayDuration their total. Watching this rise while Stalls stays
	// at zero is the signal that compaction is keeping up, but only just.
	SoftDelays        uint64
	SoftDelayDuration time.Duration

	// BytesRead and BytesWritten accumulate across every committed
	// compaction. Their ratio against the bytes the user wrote is write
	// amplification, which T6.6 reports.
	BytesRead    uint64
	BytesWritten uint64
}

// SchedulerConfig wires a scheduler to the pieces it drives.
type SchedulerConfig struct {
	Versions  *manifest.VersionSet
	Picker    *Picker
	Executor  *Executor
	Committer *Committer

	// Logger receives one line per committed compaction at debug level.
	// Nil selects a discarding logger.
	Logger log.Logger
}

// NewScheduler returns a scheduler that is not yet running.
func NewScheduler(cfg SchedulerConfig) *Scheduler {
	opts := cfg.Picker.Options()

	logger := cfg.Logger
	if logger == nil {
		logger = log.Discard()
	}

	s := &Scheduler{
		versions:  cfg.Versions,
		picker:    cfg.Picker,
		executor:  cfg.Executor,
		committer: cfg.Committer,
		logger:    logger,
		opts:      opts,

		// The soft threshold sits midway between the point compaction starts
		// and the point writes stop, so there is a band in which a writer is
		// slowed before it is blocked.
		softThreshold: opts.L0Trigger + (opts.L0StallThreshold-opts.L0Trigger)/2,
		hardThreshold: opts.L0StallThreshold,
		softDelay:     DefaultSoftDelay,

		trigger: make(chan struct{}, 1),
		quit:    make(chan struct{}),
	}
	s.cond = sync.NewCond(&s.mu)

	// Shutdown cancels a merge in flight. Wiring it here rather than making
	// the caller do it means a scheduler that can be stopped always has an
	// executor that can be interrupted.
	if s.executor != nil && s.executor.Cancel == nil {
		s.executor.Cancel = s.quit
	}
	return s
}

// Start runs the compactor goroutine until Stop.
func (s *Scheduler) Start() {
	s.wg.Add(1)
	go s.run()
}

// Trigger asks the compactor to look for work. It never blocks: the channel
// is a one-slot signal, so a trigger arriving while one is pending is
// redundant and dropped rather than queued.
//
// The engine calls this after every flush and the compactor calls it after
// every commit, which is what makes compaction cascade -- an L0 compaction
// pushes bytes into L1, which may put L1 over budget, which the next pass
// picks up.
func (s *Scheduler) Trigger() {
	select {
	case s.trigger <- struct{}{}:
	default:
	}
}

// Stop shuts the compactor down and reports the first error it hit.
//
// A merge in flight is canceled rather than waited out: the executor checks
// for it between entries, removes the outputs it had written, and commits
// nothing. That is clean by construction, since a compaction that has not
// reached the manifest never happened.
func (s *Scheduler) Stop() error {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return s.Err()
	}
	s.stopped = true
	s.mu.Unlock()

	close(s.quit)
	// Release anyone waiting on the hard threshold; no further compaction is
	// coming, so waiting for one would hang shutdown.
	s.cond.Broadcast()

	s.wg.Wait()
	return s.Err()
}

// Err reports the first error the compactor hit. Compaction failing is not
// fatal to reads -- the tree is still correct, just no longer maintained --
// but it must not be silent.
func (s *Scheduler) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// Stats reports the counters INFO prints.
func (s *Scheduler) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := s.stats
	out.L0Files = s.versions.Current().NumFiles(0)
	return out
}

// Throttle applies backpressure to a writer, and must be called before a
// write is accepted rather than after.
//
// Below the soft threshold it returns immediately. In the soft band it pauses
// for a delay proportional to the backlog. At or above the hard threshold it
// blocks until a compaction brings L0 back down -- or returns ErrStopped if
// the scheduler shuts down first, because a write that cannot be admitted
// must fail rather than be acknowledged.
func (s *Scheduler) Throttle() error {
	backlog := s.versions.Current().NumFiles(0)
	if backlog < s.softThreshold {
		return nil
	}

	// Whatever happens next, compaction is behind. Ask for work before
	// waiting for it.
	s.Trigger()

	if backlog < s.hardThreshold {
		delay := time.Duration(backlog-s.softThreshold+1) * s.softDelay
		time.Sleep(delay)

		s.mu.Lock()
		s.stats.SoftDelays++
		s.stats.SoftDelayDuration += delay
		s.mu.Unlock()
		return nil
	}

	return s.stall()
}

// stall blocks a writer at the hard threshold until L0 drains.
func (s *Scheduler) stall() error {
	start := time.Now()

	s.mu.Lock()
	defer func() {
		s.stats.Stalls++
		s.stats.StallDuration += time.Since(start)
		s.mu.Unlock()
	}()

	for {
		switch {
		case s.err != nil:
			// Compaction is dead, so L0 will never drain. Blocking forever
			// would turn a compaction failure into a hung server.
			return s.err
		case s.stopped:
			return ErrStopped
		case s.versions.Current().NumFiles(0) < s.hardThreshold:
			return nil
		}
		s.cond.Wait()
	}
}

func (s *Scheduler) run() {
	defer s.wg.Done()

	for {
		worked, err := s.CompactOnce()
		if err != nil {
			if errors.Is(err, ErrCanceled) {
				return
			}
			s.mu.Lock()
			s.stats.CompactionErrors++
			if s.err == nil {
				s.err = err
			}
			s.mu.Unlock()

			s.logger.Error("compaction failed", "err", err)
			// Wake stalled writers so they fail with this error rather than
			// waiting for a compaction that is no longer coming.
			s.cond.Broadcast()
			return
		}

		// A compaction that did work may have put the next level over
		// budget, so look again before sleeping.
		if worked {
			continue
		}

		select {
		case <-s.quit:
			return
		case <-s.trigger:
		}
	}
}

// CompactOnce runs at most one compaction and reports whether it did any
// work. It is exported so the engine's Flush can drive compaction to
// quiescence synchronously, and so tests can step it deterministically
// instead of racing the background loop.
func (s *Scheduler) CompactOnce() (bool, error) {
	s.runMu.Lock()
	defer s.runMu.Unlock()

	// Acquire, not Current: the merge opens the input files, so the version
	// naming them must be held for the whole compaction or they could be
	// deleted out from under it by a concurrent commit.
	v := s.versions.Acquire()
	defer s.versions.Release(v)

	c := s.picker.Pick(v)
	if c == nil {
		return false, nil
	}

	res, err := s.executor.Run(c)
	if err != nil {
		return false, err
	}

	if _, err := s.committer.Commit(c, res); err != nil {
		return false, err
	}

	s.mu.Lock()
	s.stats.Compactions++
	s.stats.BytesRead += res.BytesRead
	s.stats.BytesWritten += res.BytesWritten
	s.mu.Unlock()

	// A writer stalled on L0 may now be able to proceed, and the level below
	// may now be over budget.
	s.cond.Broadcast()
	s.Trigger()

	s.logger.Debug("compaction committed",
		"level", c.Level,
		"inputs", len(c.Inputs()),
		"outputs", len(res.Outputs),
		"bytes_read", res.BytesRead,
		"bytes_written", res.BytesWritten,
		"tombstones_dropped", res.TombstonesDropped,
		"bottom_most", c.BottomMost,
	)

	return true, nil
}

// Drain compacts until no level is over budget. The engine uses it to make
// Flush mean "the tree is settled", and T6.6 needs it before measuring space
// amplification -- a measurement taken before compaction has quiesced is
// measuring the backlog, not the steady state.
func (s *Scheduler) Drain() error {
	for {
		worked, err := s.CompactOnce()
		if err != nil {
			return err
		}
		if !worked {
			return nil
		}
	}
}
