// Command loadgen drives T8.1's two measurements valkey-benchmark cannot
// produce on its own: full latency percentiles (p50/p95/p99/p99.9) for a
// single interleaved mixed read/write workload, and throughput sampled over
// time during sustained compaction. valkey-benchmark runs one command type
// per invocation and reports only up to p99 from its Summary block; both
// limits are why this exists rather than another shell script.
//
// Every command it prints is meant to be pasted into docs/benchmarks.md next
// to the numbers it produced, the same reproducibility rule test/bench's
// shell scripts already follow.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"math/rand"
	"net"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/AbishekRaj2007/Strata/internal/resp"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: loadgen <mixed|overtime> [flags]")
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]

	var err error
	switch cmd {
	case "mixed":
		err = runMixed(args)
	case "overtime":
		err = runOvertime(args)
	default:
		fmt.Fprintf(os.Stderr, "loadgen: unknown subcommand %q\n", cmd)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "loadgen: %v\n", err)
		os.Exit(1)
	}
}

// client is a single persistent RESP connection.
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

func (c *client) do(args ...string) error {
	if err := c.w.WriteArrayHeader(len(args)); err != nil {
		return err
	}
	for _, a := range args {
		if err := c.w.WriteBulkString([]byte(a)); err != nil {
			return err
		}
	}
	if err := c.w.Flush(); err != nil {
		return err
	}
	_, err := c.r.ReadValue()
	return err
}

func (c *client) close() { _ = c.conn.Close() }

// runMixed drives readFrac reads / (1-readFrac) writes over keyspace keys,
// interleaved on each of clients connections, for duration, and reports
// throughput and latency percentiles measured per-operation.
func runMixed(args []string) error {
	fs := flag.NewFlagSet("mixed", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:6380", "server address")
	clients := fs.Int("clients", 50, "concurrent clients")
	duration := fs.Duration("duration", 10*time.Second, "run duration")
	keyspace := fs.Int("keyspace", 100_000, "distinct keys")
	valueSize := fs.Int("value-size", 64, "value size in bytes")
	readFrac := fs.Float64("read-frac", 0.8, "fraction of operations that are GET rather than SET")
	seed := fs.Int64("seed", 1, "random seed")
	_ = fs.Parse(args) // flag.ExitOnError already exits on parse failure

	// Populate the keyspace first: a GET pass against an empty database
	// measures the miss path, and this row is meant to measure the hit path,
	// the same reasoning test/bench/baseline.sh's prepare() follows.
	if err := populate(*addr, *keyspace, *valueSize); err != nil {
		return fmt.Errorf("populate: %w", err)
	}

	var ops, errs atomic.Int64
	var latMu sync.Mutex
	var latencies []time.Duration // one entry per completed op, guarded by latMu

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < *clients; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			c, err := dial(*addr)
			if err != nil {
				errs.Add(1)
				return
			}
			defer c.close()

			rng := rand.New(rand.NewSource(*seed + int64(id)))
			local := make([]time.Duration, 0, 4096)

			for {
				select {
				case <-stop:
					latMu.Lock()
					latencies = append(latencies, local...)
					latMu.Unlock()
					return
				default:
				}

				key := fmt.Sprintf("k%d", rng.Intn(*keyspace))
				start := time.Now()
				var err error
				if rng.Float64() < *readFrac {
					err = c.do("GET", key)
				} else {
					err = c.do("SET", key, randomValue(rng, *valueSize))
				}
				elapsed := time.Since(start)
				if err != nil {
					errs.Add(1)
					return
				}
				local = append(local, elapsed)
				ops.Add(1)
			}
		}(i)
	}

	time.Sleep(*duration)
	close(stop)
	wg.Wait()

	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	if len(latencies) == 0 {
		return fmt.Errorf("no operations completed")
	}

	pct := func(p float64) time.Duration {
		idx := int(p * float64(len(latencies)))
		if idx >= len(latencies) {
			idx = len(latencies) - 1
		}
		return latencies[idx]
	}

	throughput := float64(ops.Load()) / duration.Seconds()
	fmt.Printf("ops=%d errors=%d duration=%s throughput=%.0f ops/sec\n", ops.Load(), errs.Load(), *duration, throughput)
	fmt.Printf("p50=%.3fms p95=%.3fms p99=%.3fms p99.9=%.3fms max=%.3fms\n",
		ms(pct(0.50)), ms(pct(0.95)), ms(pct(0.99)), ms(pct(0.999)), ms(latencies[len(latencies)-1]))
	fmt.Printf("reproduce: loadgen mixed -addr %s -clients %d -duration %s -keyspace %d -value-size %d -read-frac %.2f -seed %d\n",
		*addr, *clients, *duration, *keyspace, *valueSize, *readFrac, *seed)
	return nil
}

// runOvertime drives a pure-write workload and prints one CSV line per
// interval: elapsed seconds, operations completed in that interval, and the
// instantaneous ops/sec. Meant to run against a server started with a small
// compaction geometry, so the shape of the curve -- the stalls compaction
// causes and the recovery after -- is visible rather than averaged away.
func runOvertime(args []string) error {
	fs := flag.NewFlagSet("overtime", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:6380", "server address")
	clients := fs.Int("clients", 50, "concurrent clients")
	duration := fs.Duration("duration", 60*time.Second, "run duration")
	interval := fs.Duration("interval", time.Second, "sampling interval")
	keyspace := fs.Int("keyspace", 1_000_000, "distinct keys")
	valueSize := fs.Int("value-size", 128, "value size in bytes")
	seed := fs.Int64("seed", 1, "random seed")
	_ = fs.Parse(args) // flag.ExitOnError already exits on parse failure

	var ops atomic.Int64
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < *clients; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			c, err := dial(*addr)
			if err != nil {
				return
			}
			defer c.close()

			rng := rand.New(rand.NewSource(*seed + int64(id)))
			for {
				select {
				case <-stop:
					return
				default:
				}
				key := fmt.Sprintf("k%d", rng.Intn(*keyspace))
				if err := c.do("SET", key, randomValue(rng, *valueSize)); err != nil {
					return
				}
				ops.Add(1)
			}
		}(i)
	}

	fmt.Println("elapsed_s,ops_in_interval,ops_per_sec")
	ticker := time.NewTicker(*interval)
	defer ticker.Stop()

	deadline := time.Now().Add(*duration)
	var last int64
	elapsed := 0.0
	for time.Now().Before(deadline) {
		<-ticker.C
		elapsed += interval.Seconds()
		cur := ops.Load()
		delta := cur - last
		last = cur
		fmt.Printf("%.0f,%d,%.0f\n", elapsed, delta, float64(delta)/interval.Seconds())
	}

	close(stop)
	wg.Wait()
	fmt.Printf("# reproduce: loadgen overtime -addr %s -clients %d -duration %s -interval %s -keyspace %d -value-size %d -seed %d\n",
		*addr, *clients, *duration, *interval, *keyspace, *valueSize, *seed)
	return nil
}

func populate(addr string, keyspace, valueSize int) error {
	c, err := dial(addr)
	if err != nil {
		return err
	}
	defer c.close()

	rng := rand.New(rand.NewSource(1))
	for i := 0; i < keyspace; i++ {
		if err := c.do("SET", fmt.Sprintf("k%d", i), randomValue(rng, valueSize)); err != nil {
			return fmt.Errorf("populate key %d: %w", i, err)
		}
	}
	return nil
}

func randomValue(rng *rand.Rand, size int) string {
	b := make([]byte, size)
	rng.Read(b)
	for i := range b {
		b[i] = 'a' + b[i]%26
	}
	return string(b)
}

func ms(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }
