package wal

import "fmt"

// SyncPolicy selects how aggressively the WAL is forced to physical media.
//
// The choice is a durability guarantee, not a performance knob, and the three
// values differ in what a crash can take with it. Writing to a file only
// reaches the OS page cache: a process crash still leaves the data intact for
// the OS to flush, but a machine losing power loses whatever the cache held.
// Only fsync moves data to media, and it costs roughly a millisecond even on
// NVMe.
type SyncPolicy uint8

// The three policies from plan.md Phase 2.
const (
	// SyncAlways fsyncs before acknowledging every write. Nothing that was
	// acknowledged is ever lost, including to power loss. Group commit is what
	// keeps this from costing a sync per write under concurrency.
	SyncAlways SyncPolicy = iota

	// SyncInterval fsyncs on a timer, so a crash loses at most the writes of
	// the last interval. Acknowledgement no longer implies durability, which
	// is the whole trade.
	SyncInterval

	// SyncNever leaves flushing to the OS. Data survives a process crash --
	// the page cache outlives the process -- but not a machine crash.
	SyncNever
)

// String returns the policy's flag spelling, which is also what INFO reports.
func (p SyncPolicy) String() string {
	switch p {
	case SyncAlways:
		return "always"
	case SyncInterval:
		return "interval"
	case SyncNever:
		return "never"
	default:
		return fmt.Sprintf("SyncPolicy(%d)", uint8(p))
	}
}

// AcknowledgementIsDurable reports whether a completed write under this policy
// carries the durability guarantee.
//
// This exists so the property is stated once, in code, rather than re-derived
// at each call site. The crash harness asserts the acknowledgement contract
// only where this is true; under the other two policies a lost acknowledged
// write is expected behaviour, not a bug.
func (p SyncPolicy) AcknowledgementIsDurable() bool {
	return p == SyncAlways
}

// ParseSyncPolicy converts the command-line spelling to a policy.
func ParseSyncPolicy(s string) (SyncPolicy, error) {
	switch s {
	case "always":
		return SyncAlways, nil
	case "interval":
		return SyncInterval, nil
	case "never":
		return SyncNever, nil
	default:
		return 0, fmt.Errorf("invalid sync policy %q: want always, interval, or never", s)
	}
}
