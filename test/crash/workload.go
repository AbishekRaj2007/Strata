package crash

import (
	"bufio"
	"fmt"
	"math/rand"
	"net"
	"sync"
	"time"

	"github.com/AbishekRaj2007/Strata/internal/resp"
)

// Acked records the writes the server confirmed.
//
// This is the contract under test, and the distinction it encodes is the one
// T2.4 exists to protect: a write is acknowledged when the server's +OK has
// been *read back*, not when the command was sent. A write in flight at the
// moment of the kill may or may not survive, and both outcomes are correct.
// Recording sends instead of acknowledgements produces failures that look like
// durability bugs forever and are not.
type Acked struct {
	mu sync.Mutex
	kv map[string]string
}

// NewAcked returns an empty acknowledgement set.
func NewAcked() *Acked {
	return &Acked{kv: make(map[string]string)}
}

func (a *Acked) record(key, value string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.kv[key] = value
}

// Snapshot copies the acknowledged set. Taken after the kill, it is the exact
// set of writes that must be present after recovery.
func (a *Acked) Snapshot() map[string]string {
	a.mu.Lock()
	defer a.mu.Unlock()

	out := make(map[string]string, len(a.kv))
	for k, v := range a.kv {
		out[k] = v
	}
	return out
}

// Len reports how many distinct keys have been acknowledged.
func (a *Acked) Len() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.kv)
}

// Client is a single RESP connection to the server under test.
type Client struct {
	conn net.Conn
	r    *resp.Reader
	w    *resp.Writer
	bw   *bufio.Writer
}

// Dial connects to addr.
func Dial(addr string) (*Client, error) {
	c, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", addr, err)
	}
	bw := bufio.NewWriter(c)
	return &Client{
		conn: c,
		r:    resp.NewReader(bufio.NewReader(c)),
		w:    resp.NewWriter(bw),
		bw:   bw,
	}, nil
}

// Close releases the connection.
func (c *Client) Close() error { return c.conn.Close() }

// Do sends one command and reads its reply.
//
// Each command is flushed and its reply read before returning, so a successful
// return means the server processed it. That serialisation is deliberate: it
// is what makes an acknowledgement meaningful.
func (c *Client) Do(args ...string) (resp.Value, error) {
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

// Set issues a SET and reports whether the server acknowledged it with +OK.
// Any error, including a connection torn down by the kill, means unacknowledged.
func (c *Client) Set(key, value string) (bool, error) {
	v, err := c.Do("SET", key, value)
	if err != nil {
		return false, err
	}
	return v.Type == resp.SimpleString && string(v.Bytes) == "OK", nil
}

// Get returns the stored value and whether the key was present.
func (c *Client) Get(key string) (string, bool, error) {
	v, err := c.Do("GET", key)
	if err != nil {
		return "", false, err
	}
	if v.Null {
		return "", false, nil
	}
	return string(v.Bytes), true, nil
}

// WorkloadConfig parameterises the write load driven before a kill.
type WorkloadConfig struct {
	Addr        string
	Clients     int
	Rand        *rand.Rand
	ValueSizeer func(*rand.Rand) int
}

// RunWorkload drives concurrent writers against the server until stop is
// closed, recording every acknowledgement.
//
// Writers exiting on error is expected rather than exceptional: the kill tears
// every connection down mid-flight, and that is the event being tested. The
// return value is the acknowledgement set, which stays valid after the crash.
func RunWorkload(cfg WorkloadConfig, stop <-chan struct{}) *Acked {
	acked := NewAcked()

	var wg sync.WaitGroup
	for i := 0; i < cfg.Clients; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()

			c, err := Dial(cfg.Addr)
			if err != nil {
				return
			}
			defer func() { _ = c.Close() }()

			// Each writer owns a disjoint key space, so a value read back
			// after recovery has exactly one writer that could have set it.
			for seq := 0; ; seq++ {
				select {
				case <-stop:
					return
				default:
				}

				key := fmt.Sprintf("client-%d:key-%d", id, seq)
				value := fmt.Sprintf("client-%d:value-%d", id, seq)

				ok, err := c.Set(key, value)
				if err != nil {
					// The connection died, almost certainly the kill. Anything
					// already recorded stays recorded; this write does not.
					return
				}
				if ok {
					// Recorded only after +OK was read back off the wire.
					acked.record(key, value)
				}
			}
		}(i)
	}

	wg.Wait()
	return acked
}

// Verify checks that every acknowledged write is present with the right value
// after recovery, returning the discrepancies.
//
// Extra keys are not reported: a write in flight at the kill may legitimately
// have been persisted without the client learning of it. Only the absence or
// corruption of an acknowledged write violates the contract.
func Verify(addr string, want map[string]string) ([]string, error) {
	c, err := Dial(addr)
	if err != nil {
		return nil, err
	}
	defer func() { _ = c.Close() }()

	var problems []string
	for k, wantValue := range want {
		got, found, err := c.Get(k)
		if err != nil {
			return nil, fmt.Errorf("get %q: %w", k, err)
		}
		switch {
		case !found:
			problems = append(problems, fmt.Sprintf("acknowledged key %q is missing after recovery", k))
		case got != wantValue:
			problems = append(problems,
				fmt.Sprintf("key %q = %q after recovery, want %q", k, got, wantValue))
		}
	}
	return problems, nil
}
