// Package stress drives T7.4's concurrency audit: many concurrent clients
// against a real, durable server, with a small compaction geometry so reads
// and writes are guaranteed to overlap with compaction rather than merely
// permitted to. It checks for goroutine and file-descriptor leaks in the
// same process the server ran in, which an external client cannot do.
package stress

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"math/rand"
	"net"
	"os"
	"runtime"
	"runtime/pprof"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AbishekRaj2007/Strata/internal/compaction"
	"github.com/AbishekRaj2007/Strata/internal/engine"
	"github.com/AbishekRaj2007/Strata/internal/log"
	"github.com/AbishekRaj2007/Strata/internal/resp"
	"github.com/AbishekRaj2007/Strata/internal/server"
)

// Per commit this runs for seconds; T7.4's audit passes -stress.duration=1h.
// Both exercise identical code, so a duration-only failure is never
// explained by "the long run does something different" -- the same reason
// test/model's soak takes a knob instead of being two tests.
var (
	stressDuration = flag.Duration("stress.duration", 5*time.Second, "duration for TestMixedWorkloadStress; the audit passes 1h")
	stressClients  = flag.Int("stress.clients", 100, "concurrent clients for TestMixedWorkloadStress")
	stressSeed     = flag.Int64("stress.seed", 1, "seed for TestMixedWorkloadStress")
	stressProfile  = flag.String("stress.profile", "", "directory to write block.prof and mutex.prof into; empty disables profiling")
)

// client wraps one RESP connection. It is unexported and package-local
// rather than reused from test/crash, because that package's Client is
// shaped for the durability contract (Set/Get only, tracking acknowledgement)
// and this one needs the full mixed command set.
type client struct {
	conn net.Conn
	r    *resp.Reader
	w    *resp.Writer
}

func dial(addr string) (*client, error) {
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return nil, err
	}
	return &client{conn: c, r: resp.NewReader(bufio.NewReader(c)), w: resp.NewWriter(bufio.NewWriter(c))}, nil
}

func (c *client) do(args ...string) (resp.Value, error) {
	if err := c.w.WriteArrayHeader(len(args)); err != nil {
		return resp.Value{}, err
	}
	for _, a := range args {
		if err := c.w.WriteBulkString([]byte(a)); err != nil {
			return resp.Value{}, err
		}
	}
	if err := c.w.Flush(); err != nil {
		return resp.Value{}, err
	}
	return c.r.ReadValue()
}

func (c *client) close() { _ = c.conn.Close() }

// openFDCount reads /proc/self/fd, the same information lsof would report,
// without shelling out to it. Linux-only, which matches the project.
func openFDCount(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatalf("read /proc/self/fd: %v", err)
	}
	return len(entries)
}

// settle gives goroutines and finalizers a moment to unwind after Shutdown,
// so the leak check compares two quiescent states rather than catching a
// teardown still in flight and calling it a leak.
func settle() {
	for i := 0; i < 3; i++ {
		runtime.GC()
		time.Sleep(50 * time.Millisecond)
	}
}

// TestMixedWorkloadStress is T7.4's done-when: a stress run against 100
// concurrent clients driving SET, GET, DEL, EXISTS and SCAN, with a
// compaction geometry small enough that flushes and compactions run
// continuously through the whole window rather than only at the start.
//
// The goroutine and file-descriptor counts are captured in-process, before
// Open and after Close plus settle, because neither can be observed from
// outside the server's own address space -- the reason this lives beside the
// real engine and server rather than driving a separate strata-server binary
// the way test/crash does.
func TestMixedWorkloadStress(t *testing.T) {
	baselineGoroutines := runtime.NumGoroutine()
	baselineFDs := openFDCount(t)

	if *stressProfile != "" {
		runtime.SetBlockProfileRate(1)
		runtime.SetMutexProfileFraction(1)
		defer writeContentionProfiles(t, *stressProfile)
	}

	dir := t.TempDir()
	eng, err := engine.Open(engine.Options{
		Dir:          dir,
		Threshold:    64 << 10, // small: forces frequent rotation and flush
		MaxImmutable: 4,
		Compaction: compaction.Options{
			L0Trigger:        2, // small: keeps compaction running throughout
			L0StallThreshold: 12,
			BaseLevelBytes:   256 << 10,
			LevelMultiplier:  4,
			TargetFileBytes:  128 << 10,
			MaxInputBytes:    4 << 20,
			Verify:           true, // catches a tree invariant violation immediately, not three hours later
		},
		Logger: log.Discard(),
	})
	if err != nil {
		t.Fatalf("engine.Open: %v", err)
	}

	srv, err := server.New(server.Config{
		Addr:    "127.0.0.1:0",
		Engine:  eng,
		Logger:  log.Discard(),
		Version: "stress",
	})
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	if err := srv.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}
	served := make(chan error, 1)
	go func() { served <- srv.Serve() }()

	addr := srv.Addr().String()
	stop := make(chan struct{})
	var ops, errs atomic.Int64

	var wg sync.WaitGroup
	for i := 0; i < *stressClients; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			runClient(id, addr, stop, &ops, &errs)
		}(i)
	}

	t.Logf("stress: %d clients for %s", *stressClients, *stressDuration)
	time.Sleep(*stressDuration)
	close(stop)
	wg.Wait()

	t.Logf("stress: %d ops, %d errors", ops.Load(), errs.Load())
	if errs.Load() > 0 {
		t.Errorf("%d unexpected protocol errors during the stress run", errs.Load())
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if err := <-served; err != nil {
		t.Fatalf("Serve: %v", err)
	}
	if err := eng.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	settle()

	// A handful of goroutines is not a leak; a growth proportional to what
	// just ran is. The tolerance is generous on purpose -- this test's job is
	// to catch a server that never stops spawning, not to pin an exact count
	// that the next unrelated change to net/http's runtime internals breaks.
	if got := runtime.NumGoroutine(); got > baselineGoroutines+5 {
		t.Errorf("goroutine count %d after shutdown, want at most baseline+5 (%d); leaked %d\n%s",
			got, baselineGoroutines+5, got-baselineGoroutines, goroutineDump())
	}
	if got := openFDCount(t); got > baselineFDs+5 {
		t.Errorf("open FD count %d after shutdown, want at most baseline+5 (%d); leaked %d",
			got, baselineFDs+5, got-baselineFDs)
	}
}

// writeContentionProfiles dumps the block and mutex profiles accumulated
// since SetBlockProfileRate/SetMutexProfileFraction were enabled. It is the
// mechanism T7.4 asks for -- "capture block and mutex profiles to locate
// your worst contention" -- pointed at this stress run specifically, because
// a profile taken under a single-client benchmark cannot show lock
// contention that only concurrency creates.
func writeContentionProfiles(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Errorf("mkdir %s: %v", dir, err)
		return
	}
	for _, name := range []string{"block", "mutex"} {
		f, err := os.Create(fmt.Sprintf("%s/%s.prof", dir, name))
		if err != nil {
			t.Errorf("create %s.prof: %v", name, err)
			continue
		}
		if err := pprof.Lookup(name).WriteTo(f, 0); err != nil {
			t.Errorf("write %s.prof: %v", name, err)
		}
		f.Close()
	}
}

func goroutineDump() []byte {
	buf := make([]byte, 1<<20)
	n := runtime.Stack(buf, true)
	return buf[:n]
}

// runClient issues a weighted mix of commands against addr until stop
// closes. Errors from the connection closing at the end of the run are
// expected and not counted; only mid-run protocol errors are.
func runClient(id int, addr string, stop <-chan struct{}, ops, errs *atomic.Int64) {
	c, err := dial(addr)
	if err != nil {
		errs.Add(1)
		return
	}
	defer c.close()

	rng := rand.New(rand.NewSource(*stressSeed + int64(id)))
	keyspace := 500 // small keyspace: deliberately forces overlapping writes and reads on the same keys, not disjoint ranges

	for seq := 0; ; seq++ {
		select {
		case <-stop:
			return
		default:
		}

		key := fmt.Sprintf("k%d", rng.Intn(keyspace))
		var v resp.Value
		var err error

		switch r := rng.Float64(); {
		case r < 0.40:
			v, err = c.do("SET", key, fmt.Sprintf("client-%d-seq-%d-%s", id, seq, randomPadding(rng)))
		case r < 0.75:
			v, err = c.do("GET", key)
		case r < 0.85:
			v, err = c.do("DEL", key)
		case r < 0.92:
			v, err = c.do("EXISTS", key)
		default:
			v, err = c.do("SCAN", "0", "COUNT", "20")
		}

		if err != nil {
			// The stop channel races the in-flight request at shutdown; a
			// connection error right at that boundary is expected, not a bug.
			select {
			case <-stop:
				return
			default:
				errs.Add(1)
				return
			}
		}
		if v.Type == resp.Error {
			errs.Add(1)
			return
		}
		ops.Add(1)
	}
}

func randomPadding(rng *rand.Rand) string {
	n := rng.Intn(200)
	b := make([]byte, n)
	rng.Read(b)
	for i := range b {
		b[i] = 'a' + b[i]%26
	}
	return string(b)
}
