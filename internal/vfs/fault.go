package vfs

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"sync"
	"time"
)

// OpKind names a filesystem operation the injector can fail.
type OpKind string

// The operation kinds Injector can fail.
const (
	OpCreate   OpKind = "create"
	OpOpen     OpKind = "open"
	OpOpenDir  OpKind = "opendir"
	OpWrite    OpKind = "write"
	OpRead     OpKind = "read"
	OpReadAt   OpKind = "readat"
	OpSync     OpKind = "sync"
	OpClose    OpKind = "close"
	OpRemove   OpKind = "remove"
	OpRename   OpKind = "rename"
	OpMkdirAll OpKind = "mkdirall"
	OpReadDir  OpKind = "readdir"
	OpReadFile OpKind = "readfile"
	OpStat     OpKind = "stat"
)

// Mode selects what an injected fault does.
//
// The mode is a property of the fault rather than of the operation, and the
// injector translates it into whatever failing means for the operation it
// lands on. That is what makes "fail operation 4,217" a sentence the sweep
// can say without first knowing what operation 4,217 is.
type Mode int

const (
	// ModeError makes the operation fail outright, having done nothing.
	ModeError Mode = iota

	// ModeTorn makes a write land half its bytes and then fail, which is
	// the shape of a real short write and the one that leaves a file the
	// reader has to reject rather than misread. On operations that are not
	// writes it behaves as ModeError.
	ModeTorn

	// ModeShort makes a read return fewer bytes than asked for. A
	// sequential Read may legally do this with no error at all, which is
	// precisely why it is worth injecting: code that treats a short Read as
	// impossible is wrong on a real filesystem too.
	ModeShort

	// ModeLatency delays the operation and then lets it succeed. It finds
	// timeouts and ordering assumptions rather than error handling.
	ModeLatency
)

func (m Mode) String() string {
	switch m {
	case ModeError:
		return "error"
	case ModeTorn:
		return "torn"
	case ModeShort:
		return "short"
	case ModeLatency:
		return "latency"
	}
	return "?"
}

// ErrInjected is the error every injected failure carries. Tests assert on it
// so that a genuine filesystem error is never mistaken for an injected one.
var ErrInjected = errors.New("vfs: injected fault")

// Fault describes one injected failure.
type Fault struct {
	Mode Mode

	// Err is returned instead of ErrInjected when set. It is how a test
	// injects a specific errno -- ENOSPC and EIO are handled differently
	// from each other in places, and the difference is worth testing.
	Err error

	// Delay is how long ModeLatency waits.
	Delay time.Duration
}

func (f Fault) err() error {
	if f.Err != nil {
		return f.Err
	}
	return ErrInjected
}

// Record is one entry in an injector's trace.
type Record struct {
	Index int64
	Kind  OpKind
	Path  string
}

func (r Record) String() string {
	return fmt.Sprintf("#%d %s %s", r.Index, r.Kind, r.Path)
}

// Injector wraps an FS and counts every operation, optionally failing one of
// them.
//
// The unit of control is the operation index rather than a path or a call
// site. That is the whole design: it means a sweep can say "fail the Nth I/O
// operation this workload performs" for every N, without a list of the places
// I/O happens, and so cannot miss the one nobody thought of.
//
// # On reproducibility
//
// The index is exactly reproducible for a single-threaded workload. The
// engine is not single-threaded -- a flush and a compaction run on their own
// goroutines -- so the operation that index N names can shift between runs.
// The injector therefore records which operation it actually faulted, and
// callers report that rather than assuming. A sweep whose coverage is
// approximate but whose failure reports are exact is far more useful than the
// reverse.
type Injector struct {
	under FS

	mu      sync.Mutex
	count   int64
	armedAt int64
	fault   Fault
	fired   *Record
	trace   []Record
	tracing bool
}

// NewInjector wraps under. Nil means the real filesystem.
func NewInjector(under FS) *Injector {
	return &Injector{under: Or(under), armedAt: -1}
}

// ArmAt makes the injector apply f to the operation at the given index,
// counting from zero over the injector's whole lifetime.
func (in *Injector) ArmAt(index int64, f Fault) {
	in.mu.Lock()
	defer in.mu.Unlock()
	in.armedAt, in.fault, in.fired = index, f, nil
}

// Disarm stops injecting. Counting continues, which is what lets a caller
// measure a workload's I/O before deciding what to fail.
func (in *Injector) Disarm() {
	in.mu.Lock()
	defer in.mu.Unlock()
	in.armedAt = -1
}

// Count reports how many operations have gone through the injector.
func (in *Injector) Count() int64 {
	in.mu.Lock()
	defer in.mu.Unlock()
	return in.count
}

// Fired reports the operation the fault landed on, or nil if it never fired.
// A sweep step whose fault never fired tested nothing, and silently counting
// it as a pass is how a fault injection suite comes to prove nothing at all.
func (in *Injector) Fired() (Record, bool) {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.fired == nil {
		return Record{}, false
	}
	return *in.fired, true
}

// Trace turns on recording and returns everything recorded so far. Tracing is
// off by default because a soak run performs millions of operations and
// remembering them all is its own kind of failure.
func (in *Injector) Trace(on bool) []Record {
	in.mu.Lock()
	defer in.mu.Unlock()
	in.tracing = on
	out := make([]Record, len(in.trace))
	copy(out, in.trace)
	return out
}

// check advances the counter and reports the fault to apply, if any.
func (in *Injector) check(kind OpKind, path string) (Fault, bool) {
	in.mu.Lock()
	defer in.mu.Unlock()

	idx := in.count
	in.count++
	if in.tracing {
		in.trace = append(in.trace, Record{Index: idx, Kind: kind, Path: path})
	}

	if in.armedAt != idx {
		return Fault{}, false
	}
	// A fault fires once. Re-firing on a retry would turn "this one
	// operation failed" into "this operation always fails", which is a
	// different scenario and not the one being swept.
	in.armedAt = -1
	rec := Record{Index: idx, Kind: kind, Path: path}
	in.fired = &rec
	return in.fault, true
}

// apply performs the non-write, non-read part of a fault: fail, or delay.
// It reports whether the caller should return the error instead of proceeding.
func (in *Injector) apply(kind OpKind, path string) error {
	f, ok := in.check(kind, path)
	if !ok {
		return nil
	}
	if f.Mode == ModeLatency {
		time.Sleep(f.Delay)
		return nil
	}
	return fmt.Errorf("%s %s: %w", kind, path, f.err())
}

// Create implements FS, applying any fault configured for OpCreate first.
func (in *Injector) Create(path string) (File, error) {
	if err := in.apply(OpCreate, path); err != nil {
		return nil, err
	}
	f, err := in.under.Create(path)
	return in.wrap(f, err)
}

// CreateTruncate implements FS, applying any fault configured for OpCreate first.
func (in *Injector) CreateTruncate(path string) (File, error) {
	if err := in.apply(OpCreate, path); err != nil {
		return nil, err
	}
	f, err := in.under.CreateTruncate(path)
	return in.wrap(f, err)
}

// Open implements FS, applying any fault configured for OpOpen first.
func (in *Injector) Open(path string) (File, error) {
	if err := in.apply(OpOpen, path); err != nil {
		return nil, err
	}
	f, err := in.under.Open(path)
	return in.wrap(f, err)
}

// OpenDir implements FS, applying any fault configured for OpOpenDir first.
func (in *Injector) OpenDir(path string) (File, error) {
	if err := in.apply(OpOpenDir, path); err != nil {
		return nil, err
	}
	f, err := in.under.OpenDir(path)
	return in.wrap(f, err)
}

func (in *Injector) wrap(f File, err error) (File, error) {
	if err != nil {
		return nil, err
	}
	return &faultFile{in: in, File: f}, nil
}

// Remove implements FS, applying any fault configured for OpRemove first.
func (in *Injector) Remove(path string) error {
	if err := in.apply(OpRemove, path); err != nil {
		return err
	}
	return in.under.Remove(path)
}

// Rename implements FS, applying any fault configured for OpRename first.
func (in *Injector) Rename(oldPath, newPath string) error {
	if err := in.apply(OpRename, oldPath); err != nil {
		return err
	}
	return in.under.Rename(oldPath, newPath)
}

// MkdirAll implements FS, applying any fault configured for OpMkdirAll first.
func (in *Injector) MkdirAll(path string, perm fs.FileMode) error {
	if err := in.apply(OpMkdirAll, path); err != nil {
		return err
	}
	return in.under.MkdirAll(path, perm)
}

// ReadDir implements FS, applying any fault configured for OpReadDir first.
func (in *Injector) ReadDir(path string) ([]os.DirEntry, error) {
	if err := in.apply(OpReadDir, path); err != nil {
		return nil, err
	}
	return in.under.ReadDir(path)
}

// ReadFile implements FS, applying any fault configured for OpReadFile first.
func (in *Injector) ReadFile(path string) ([]byte, error) {
	if err := in.apply(OpReadFile, path); err != nil {
		return nil, err
	}
	return in.under.ReadFile(path)
}

// Stat implements FS, applying any fault configured for OpStat first.
func (in *Injector) Stat(path string) (fs.FileInfo, error) {
	if err := in.apply(OpStat, path); err != nil {
		return nil, err
	}
	return in.under.Stat(path)
}

// faultFile applies faults to the per-file operations.
type faultFile struct {
	in *Injector
	File
}

func (f *faultFile) Write(p []byte) (int, error) {
	fault, ok := f.in.check(OpWrite, f.Name())
	if !ok {
		return f.File.Write(p)
	}

	switch fault.Mode {
	case ModeLatency:
		time.Sleep(fault.Delay)
		return f.File.Write(p)

	case ModeTorn:
		// Half the bytes reach the file and then the write fails. This is
		// the case that matters: the file is now longer than anything that
		// was ever acknowledged, and every reader of it has to notice.
		half := len(p) / 2
		n, err := f.File.Write(p[:half])
		if err != nil {
			return n, err
		}
		return n, fmt.Errorf("write %s: %w (torn after %d of %d bytes)",
			f.Name(), fault.err(), n, len(p))

	default:
		return 0, fmt.Errorf("write %s: %w", f.Name(), fault.err())
	}
}

// WriteAt is the WAL's write: a partial block is rewritten in place as it
// grows, so the same offset is written repeatedly.
func (f *faultFile) WriteAt(p []byte, off int64) (int, error) {
	fault, ok := f.in.check(OpWrite, f.Name())
	if !ok {
		return f.File.WriteAt(p, off)
	}

	switch fault.Mode {
	case ModeLatency:
		time.Sleep(fault.Delay)
		return f.File.WriteAt(p, off)

	case ModeTorn:
		half := len(p) / 2
		n, err := f.File.WriteAt(p[:half], off)
		if err != nil {
			return n, err
		}
		return n, fmt.Errorf("writeat %s: %w (torn after %d of %d bytes)",
			f.Name(), fault.err(), n, len(p))

	default:
		return 0, fmt.Errorf("writeat %s: %w", f.Name(), fault.err())
	}
}

func (f *faultFile) Read(p []byte) (int, error) {
	fault, ok := f.in.check(OpRead, f.Name())
	if !ok {
		return f.File.Read(p)
	}

	switch fault.Mode {
	case ModeLatency:
		time.Sleep(fault.Delay)
		return f.File.Read(p)

	case ModeShort:
		if len(p) > 1 {
			p = p[:len(p)/2]
		}
		return f.File.Read(p)

	default:
		return 0, fmt.Errorf("read %s: %w", f.Name(), fault.err())
	}
}

func (f *faultFile) ReadAt(p []byte, off int64) (int, error) {
	fault, ok := f.in.check(OpReadAt, f.Name())
	if !ok {
		return f.File.ReadAt(p, off)
	}

	switch fault.Mode {
	case ModeLatency:
		time.Sleep(fault.Delay)
		return f.File.ReadAt(p, off)

	case ModeShort:
		// ReadAt, unlike Read, must report an error when it returns fewer
		// bytes than asked for, so a short ReadAt is always visible.
		if len(p) > 1 {
			n, _ := f.File.ReadAt(p[:len(p)/2], off)
			return n, io.ErrUnexpectedEOF
		}
		return 0, io.ErrUnexpectedEOF

	default:
		return 0, fmt.Errorf("readat %s: %w", f.Name(), fault.err())
	}
}

func (f *faultFile) Sync() error {
	fault, ok := f.in.check(OpSync, f.Name())
	if !ok {
		return f.File.Sync()
	}
	if fault.Mode == ModeLatency {
		time.Sleep(fault.Delay)
		return f.File.Sync()
	}
	// A failed fsync is the nastiest fault in the set. On Linux the error is
	// reported once and the dirty page is dropped, so a caller that retries
	// gets success while its data is gone. The engine treats this as
	// unrecoverable; the injector's job is only to produce it.
	return fmt.Errorf("sync %s: %w", f.Name(), fault.err())
}

func (f *faultFile) Close() error {
	fault, ok := f.in.check(OpClose, f.Name())
	if !ok {
		return f.File.Close()
	}
	if fault.Mode == ModeLatency {
		time.Sleep(fault.Delay)
		return f.File.Close()
	}
	// The descriptor is still released: a close that both fails and leaks is
	// two faults, and the sweep is injecting one.
	_ = f.File.Close()
	return fmt.Errorf("close %s: %w", f.Name(), fault.err())
}

// Compile-time proof the injector is a drop-in filesystem.
var _ FS = (*Injector)(nil)
