package server

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AbishekRaj2007/Strata/internal/engine"
	"github.com/AbishekRaj2007/Strata/internal/log"
	"github.com/AbishekRaj2007/Strata/internal/resp"
)

// newTestServer starts a server on an ephemeral port and returns it with a
// cleanup that shuts it down, so no test leaks a listener into the next.
func newTestServer(t *testing.T) *Server {
	t.Helper()

	s, err := New(Config{
		Addr:    "127.0.0.1:0",
		Engine:  engine.NewMemory(),
		Logger:  log.Discard(),
		Version: "test",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}

	served := make(chan error, 1)
	go func() { served <- s.Serve() }()

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.Shutdown(ctx); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
		if err := <-served; err != nil {
			t.Errorf("Serve: %v", err)
		}
	})

	return s
}

// client is a minimal RESP client, kept separate from the server's own codec
// usage so a test failure points at the server rather than at shared helper
// code.
type client struct {
	t  *testing.T
	nc net.Conn
	r  *resp.Reader
	w  *resp.Writer
	bw *bufio.Writer
}

func dial(t *testing.T, s *Server) *client {
	t.Helper()

	nc, err := net.Dial("tcp", s.Addr().String())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = nc.Close() })

	bw := bufio.NewWriter(nc)
	return &client{
		t:  t,
		nc: nc,
		r:  resp.NewReader(bufio.NewReader(nc)),
		w:  resp.NewWriter(bw),
		bw: bw,
	}
}

// send writes a command as the array of bulk strings a real client sends.
func (c *client) send(args ...string) {
	c.t.Helper()

	if err := c.w.WriteArrayHeader(len(args)); err != nil {
		c.t.Fatalf("WriteArrayHeader: %v", err)
	}
	for _, a := range args {
		if err := c.w.WriteBulkString([]byte(a)); err != nil {
			c.t.Fatalf("WriteBulkString: %v", err)
		}
	}
	if err := c.bw.Flush(); err != nil {
		c.t.Fatalf("Flush: %v", err)
	}
}

func (c *client) receive() resp.Value {
	c.t.Helper()

	if err := c.nc.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		c.t.Fatalf("SetReadDeadline: %v", err)
	}
	v, err := c.r.ReadValue()
	if err != nil {
		c.t.Fatalf("ReadValue: %v", err)
	}
	return v
}

func (c *client) do(args ...string) resp.Value {
	c.t.Helper()
	c.send(args...)
	return c.receive()
}

func TestPingPong(t *testing.T) {
	c := dial(t, newTestServer(t))

	v := c.do("PING")
	if v.Type != resp.SimpleString || string(v.Bytes) != "PONG" {
		t.Errorf("PING = %+v, want +PONG", v)
	}

	v = c.do("PING", "hello")
	if v.Type != resp.BulkString || string(v.Bytes) != "hello" {
		t.Errorf("PING hello = %+v, want bulk hello", v)
	}
}

func TestSetGet(t *testing.T) {
	c := dial(t, newTestServer(t))

	if v := c.do("SET", "k", "v"); v.Type != resp.SimpleString || string(v.Bytes) != "OK" {
		t.Fatalf("SET = %+v, want +OK", v)
	}
	if v := c.do("GET", "k"); v.Type != resp.BulkString || string(v.Bytes) != "v" {
		t.Errorf("GET = %+v, want bulk v", v)
	}
}

// TestGetMissingIsNullNotEmpty is the wire-level half of the distinction
// ADR-003 calls critical. A missing key must reply $-1, never $0.
func TestGetMissingIsNullNotEmpty(t *testing.T) {
	c := dial(t, newTestServer(t))

	v := c.do("GET", "absent")
	if !v.Null {
		t.Errorf("GET of absent key = %+v, want a null bulk string", v)
	}

	// A key holding an empty value is present, and must not look null.
	c.do("SET", "empty", "")
	v = c.do("GET", "empty")
	if v.Null {
		t.Error("GET of an empty value replied null; deleted keys and empty values are now indistinguishable")
	}
	if len(v.Bytes) != 0 {
		t.Errorf("GET of empty value = %q, want empty", v.Bytes)
	}
}

func TestDelAndExists(t *testing.T) {
	c := dial(t, newTestServer(t))
	c.do("SET", "a", "1")
	c.do("SET", "b", "2")

	if v := c.do("EXISTS", "a", "b", "missing"); v.Int != 2 {
		t.Errorf("EXISTS = %d, want 2", v.Int)
	}
	// EXISTS counts repeats, matching Redis.
	if v := c.do("EXISTS", "a", "a"); v.Int != 2 {
		t.Errorf("EXISTS a a = %d, want 2", v.Int)
	}
	if v := c.do("DEL", "a", "b", "missing"); v.Int != 2 {
		t.Errorf("DEL = %d, want 2", v.Int)
	}
	if v := c.do("EXISTS", "a"); v.Int != 0 {
		t.Errorf("EXISTS after DEL = %d, want 0", v.Int)
	}
}

func TestEcho(t *testing.T) {
	c := dial(t, newTestServer(t))
	if v := c.do("ECHO", "hi"); string(v.Bytes) != "hi" {
		t.Errorf("ECHO = %q, want %q", v.Bytes, "hi")
	}
}

func TestDBSizeAndFlushDB(t *testing.T) {
	c := dial(t, newTestServer(t))
	c.do("SET", "a", "1")
	c.do("SET", "b", "2")

	if v := c.do("DBSIZE"); v.Int != 2 {
		t.Errorf("DBSIZE = %d, want 2", v.Int)
	}
	if v := c.do("FLUSHDB"); string(v.Bytes) != "OK" {
		t.Errorf("FLUSHDB = %+v, want +OK", v)
	}
	if v := c.do("DBSIZE"); v.Int != 0 {
		t.Errorf("DBSIZE after FLUSHDB = %d, want 0", v.Int)
	}
}

func TestScan(t *testing.T) {
	c := dial(t, newTestServer(t))
	for i := range 5 {
		c.do("SET", fmt.Sprintf("key%d", i), "v")
	}

	seen := map[string]bool{}
	cursor := "0"
	for i := 0; ; i++ {
		if i > 20 {
			t.Fatal("SCAN did not terminate")
		}

		v := c.do("SCAN", cursor, "COUNT", "2")
		if v.Type != resp.Array || len(v.Array) != 2 {
			t.Fatalf("SCAN reply = %+v, want a two-element array", v)
		}

		cursor = string(v.Array[0].Bytes)
		for _, k := range v.Array[1].Array {
			seen[string(k.Bytes)] = true
		}
		if cursor == "0" {
			break
		}
	}

	if len(seen) != 5 {
		t.Errorf("SCAN saw %d keys, want 5", len(seen))
	}
}

func TestScanMatch(t *testing.T) {
	c := dial(t, newTestServer(t))
	c.do("SET", "user:1", "a")
	c.do("SET", "user:2", "b")
	c.do("SET", "post:1", "c")

	seen := map[string]bool{}
	cursor := "0"
	for i := 0; ; i++ {
		if i > 20 {
			t.Fatal("SCAN did not terminate")
		}

		v := c.do("SCAN", cursor, "MATCH", "user:*", "COUNT", "10")
		cursor = string(v.Array[0].Bytes)
		for _, k := range v.Array[1].Array {
			seen[string(k.Bytes)] = true
		}
		if cursor == "0" {
			break
		}
	}

	if len(seen) != 2 || !seen["user:1"] || !seen["user:2"] {
		t.Errorf("SCAN MATCH saw %v, want user:1 and user:2", seen)
	}
}

func TestInfo(t *testing.T) {
	c := dial(t, newTestServer(t))
	v := c.do("INFO")

	body := string(v.Bytes)
	for _, want := range []string{"strata_version:test", "connected_clients:", "db0:keys="} {
		if !strings.Contains(body, want) {
			t.Errorf("INFO missing %q in:\n%s", want, body)
		}
	}
}

// TestCommandDocs covers the handshake modern redis-cli performs on connect.
// Without a reply the client hangs before showing a prompt, which reads as a
// broken server (plan.md §7.5).
func TestCommandDocs(t *testing.T) {
	c := dial(t, newTestServer(t))

	v := c.do("COMMAND", "DOCS")
	if v.Type != resp.Array || len(v.Array) != 0 {
		t.Errorf("COMMAND DOCS = %+v, want an empty array", v)
	}
}

// TestHelloIsRefused confirms the RESP3 fallback path ADR-003 depends on.
func TestHelloIsRefused(t *testing.T) {
	c := dial(t, newTestServer(t))

	v := c.do("HELLO", "3")
	if v.Type != resp.Error {
		t.Errorf("HELLO 3 = %+v, want an error so clients fall back to RESP2", v)
	}
}

func TestErrorReplies(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{"unknown command", []string{"NOSUCHCOMMAND"}, "ERR unknown command"},
		{"set arity", []string{"SET", "k"}, "wrong number of arguments"},
		{"get arity", []string{"GET"}, "wrong number of arguments"},
		{"get too many", []string{"GET", "a", "b"}, "wrong number of arguments"},
		{"del arity", []string{"DEL"}, "wrong number of arguments"},
		{"echo arity", []string{"ECHO"}, "wrong number of arguments"},
		{"set with options", []string{"SET", "k", "v", "EX", "10"}, "syntax error"},
		{"scan bad cursor", []string{"SCAN", "notanumber"}, "invalid cursor"},
		{"scan bad option", []string{"SCAN", "0", "NOPE", "1"}, "syntax error"},
		{"scan dangling option", []string{"SCAN", "0", "COUNT"}, "syntax error"},
		{"scan bad count", []string{"SCAN", "0", "COUNT", "zero"}, "not an integer"},
		{"select wrong db", []string{"SELECT", "1"}, "out of range"},
		{"shutdown refused", []string{"SHUTDOWN"}, "not supported"},
	}

	s := newTestServer(t)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := dial(t, s)
			v := c.do(tt.args...)

			if v.Type != resp.Error {
				t.Fatalf("%v = %+v, want an error", tt.args, v)
			}
			if !strings.Contains(string(v.Bytes), tt.want) {
				t.Errorf("%v = %q, want it to contain %q", tt.args, v.Bytes, tt.want)
			}
		})
	}
}

// TestCaseInsensitiveCommands matters because clients vary: go-redis sends
// lower case, redis-cli upper.
func TestCaseInsensitiveCommands(t *testing.T) {
	c := dial(t, newTestServer(t))

	for _, name := range []string{"PING", "ping", "PiNg"} {
		if v := c.do(name); string(v.Bytes) != "PONG" {
			t.Errorf("%s = %+v, want +PONG", name, v)
		}
	}
}

// TestBinarySafeValues confirms a value carrying NUL and CRLF survives the
// round trip, since docs/format.md §0.1 makes values arbitrary byte strings.
func TestBinarySafeValues(t *testing.T) {
	c := dial(t, newTestServer(t))
	value := "bin\x00ary\r\nvalue"

	c.do("SET", "k", value)
	if v := c.do("GET", "k"); string(v.Bytes) != value {
		t.Errorf("GET = %q, want %q", v.Bytes, value)
	}
}

// TestPipelining sends a batch without reading between commands, which is the
// path T1.4 benchmarks and the reason the loop flushes on an empty buffer
// rather than once per reply.
func TestPipelining(t *testing.T) {
	c := dial(t, newTestServer(t))
	const n = 50

	for i := range n {
		c.send("SET", fmt.Sprintf("k%d", i), fmt.Sprintf("v%d", i))
	}
	for i := range n {
		if v := c.receive(); string(v.Bytes) != "OK" {
			t.Fatalf("reply %d = %+v, want +OK", i, v)
		}
	}

	for i := range n {
		c.send("GET", fmt.Sprintf("k%d", i))
	}
	for i := range n {
		want := fmt.Sprintf("v%d", i)
		if v := c.receive(); string(v.Bytes) != want {
			t.Fatalf("reply %d = %q, want %q", i, v.Bytes, want)
		}
	}
}

// TestQuit checks the client-initiated close path.
func TestQuit(t *testing.T) {
	c := dial(t, newTestServer(t))

	if v := c.do("QUIT"); string(v.Bytes) != "OK" {
		t.Errorf("QUIT = %+v, want +OK", v)
	}
	if _, err := c.r.ReadValue(); err == nil {
		t.Error("connection stayed open after QUIT")
	}
}

// TestMalformedInputDoesNotKillServer confirms one bad client cannot take the
// server down for everyone else.
func TestMalformedInputDoesNotKillServer(t *testing.T) {
	s := newTestServer(t)

	bad, err := net.Dial("tcp", s.Addr().String())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	if _, err := bad.Write([]byte("this is not RESP\r\n\x00\xff garbage")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	_ = bad.Close()

	// A healthy client must still be served.
	good := dial(t, s)
	if v := good.do("PING"); string(v.Bytes) != "PONG" {
		t.Errorf("PING after malformed client = %+v, want +PONG", v)
	}
}

// TestFiftyConcurrentConnections is T1.2's Done-when condition. Run under
// -race in CI, it is the check that connection registration and the shutdown
// drain do not race.
func TestFiftyConcurrentConnections(t *testing.T) {
	s := newTestServer(t)

	const clients = 50
	const opsPerClient = 20

	var wg sync.WaitGroup
	errs := make(chan error, clients)

	for i := range clients {
		wg.Add(1)
		go func() {
			defer wg.Done()

			nc, err := net.Dial("tcp", s.Addr().String())
			if err != nil {
				errs <- fmt.Errorf("client %d dial: %w", i, err)
				return
			}
			defer func() { _ = nc.Close() }()

			bw := bufio.NewWriter(nc)
			w := resp.NewWriter(bw)
			r := resp.NewReader(bufio.NewReader(nc))

			for op := range opsPerClient {
				key := fmt.Sprintf("client%d:key%d", i, op)

				if err := writeCommand(w, bw, "SET", key, "value"); err != nil {
					errs <- fmt.Errorf("client %d SET: %w", i, err)
					return
				}
				if err := nc.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
					errs <- err
					return
				}
				if _, err := r.ReadValue(); err != nil {
					errs <- fmt.Errorf("client %d SET reply: %w", i, err)
					return
				}

				if err := writeCommand(w, bw, "GET", key); err != nil {
					errs <- fmt.Errorf("client %d GET: %w", i, err)
					return
				}
				v, err := r.ReadValue()
				if err != nil {
					errs <- fmt.Errorf("client %d GET reply: %w", i, err)
					return
				}
				if string(v.Bytes) != "value" {
					errs <- fmt.Errorf("client %d GET = %q, want %q", i, v.Bytes, "value")
					return
				}
			}
		}()
	}

	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func writeCommand(w *resp.Writer, bw *bufio.Writer, args ...string) error {
	if err := w.WriteArrayHeader(len(args)); err != nil {
		return err
	}
	for _, a := range args {
		if err := w.WriteBulkString([]byte(a)); err != nil {
			return err
		}
	}
	return bw.Flush()
}

// TestShutdownDrainsInFlightCommands is the trap plan.md names for T1.2: a
// shutdown that closes the listener and exits looks correct until a command
// is in flight. Here the engine must stay open until the reply is written.
func TestShutdownDrainsInFlightCommands(t *testing.T) {
	s, err := New(Config{
		Addr:    "127.0.0.1:0",
		Engine:  engine.NewMemory(),
		Logger:  log.Discard(),
		Version: "test",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}

	served := make(chan error, 1)
	go func() { served <- s.Serve() }()

	c := dial(t, s)
	c.do("SET", "k", "v")

	// Keep a command in flight across the shutdown call.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 100 {
			c.send("GET", "k")
			v, err := c.r.ReadValue()
			if err != nil {
				return
			}
			if v.Type == resp.Error {
				t.Errorf("GET during shutdown = %q; the engine closed before the drain finished", v.Bytes)
				return
			}
		}
	}()

	time.Sleep(10 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		t.Errorf("Shutdown: %v", err)
	}
	<-done

	if err := <-served; err != nil {
		t.Errorf("Serve returned %v, want nil on a deliberate shutdown", err)
	}
}

// TestShutdownClosesIdleConnections covers the case that would otherwise hold
// shutdown for the full idle timeout: a client that connects and says nothing.
func TestShutdownClosesIdleConnections(t *testing.T) {
	s, err := New(Config{
		Addr:    "127.0.0.1:0",
		Engine:  engine.NewMemory(),
		Logger:  log.Discard(),
		Version: "test",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.Listen(); err != nil {
		t.Fatalf("Listen: %v", err)
	}

	served := make(chan error, 1)
	go func() { served <- s.Serve() }()

	for range 5 {
		nc, err := net.Dial("tcp", s.Addr().String())
		if err != nil {
			t.Fatalf("Dial: %v", err)
		}
		defer func() { _ = nc.Close() }()
	}

	// Let the accept loop register them before shutting down.
	deadline := time.Now().Add(2 * time.Second)
	for s.ConnCount() < 5 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}

	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		t.Errorf("Shutdown: %v", err)
	}

	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("Shutdown took %v; idle connections were not closed promptly", elapsed)
	}
	if err := <-served; err != nil {
		t.Errorf("Serve: %v", err)
	}
}

func TestShutdownIsIdempotent(t *testing.T) {
	s := newTestServer(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		t.Fatalf("first Shutdown: %v", err)
	}
	if err := s.Shutdown(ctx); err != nil {
		t.Errorf("second Shutdown: %v", err)
	}
}

func TestServeBeforeListenFails(t *testing.T) {
	s, err := New(Config{Engine: engine.NewMemory(), Logger: log.Discard()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := s.Serve(); err == nil {
		t.Error("Serve without Listen = nil, want an error")
	}
}

func TestNewRequiresEngine(t *testing.T) {
	if _, err := New(Config{Addr: "127.0.0.1:0"}); err == nil {
		t.Error("New without an engine = nil, want an error")
	}
}

func TestCommandTableCoversSpec(t *testing.T) {
	// plan.md §7.5 fixes the v1 command set; this fails if one is dropped.
	for _, name := range []string{
		"ping", "echo", "set", "get", "del", "exists",
		"scan", "dbsize", "info", "compact", "flushdb", "command",
	} {
		if _, ok := commands[name]; !ok {
			t.Errorf("command %q from plan.md §7.5 is missing from the dispatch table", name)
		}
	}

	if len(commandNames()) != len(commands) {
		t.Error("commandNames does not match the dispatch table")
	}
}
