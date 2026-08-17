package server

import (
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"

	"github.com/AbishekRaj2007/Strata/internal/engine"
)

// flusher is the subset of the temporary map engine that FLUSHDB needs. The
// LSM engine implements FLUSHDB by other means, so it stays off the Engine
// interface and is probed for here instead.
type flusher interface {
	Flush() error
}

// handler executes one command and writes its reply. Arity is checked by the
// dispatch table before a handler runs, so handlers may index args directly.
type handler func(c *conn, args [][]byte) error

// commandSpec describes one command in the dispatch table.
type commandSpec struct {
	// arity is the minimum number of arguments including the command name,
	// matching the convention redis-cli reports. A negative value means
	// "at least this many", which is how the variadic commands are spelled.
	arity  int
	handle handler
}

// commands maps lower-cased command names to their specs. plan.md §7.5 fixes
// the v1 set; anything outside it is a hard error rather than a silent no-op,
// because a client that thinks it wrote data through an ignored command is a
// worse failure than one that saw an error.
var commands = map[string]commandSpec{
	"ping": {arity: -1, handle: (*conn).cmdPing},
	"echo": {arity: 2, handle: (*conn).cmdEcho},
	// SET accepts extra arguments at the dispatch layer so that the handler
	// can reject EX and NX as a syntax error, which is what a client expects
	// on an unsupported option, rather than as an arity error.
	"set":      {arity: -3, handle: (*conn).cmdSet},
	"get":      {arity: 2, handle: (*conn).cmdGet},
	"del":      {arity: -2, handle: (*conn).cmdDel},
	"exists":   {arity: -2, handle: (*conn).cmdExists},
	"scan":     {arity: -2, handle: (*conn).cmdScan},
	"dbsize":   {arity: 1, handle: (*conn).cmdDBSize},
	"info":     {arity: -1, handle: (*conn).cmdInfo},
	"compact":  {arity: 1, handle: (*conn).cmdCompact},
	"flushdb":  {arity: 1, handle: (*conn).cmdFlushDB},
	"command":  {arity: -1, handle: (*conn).cmdCommand},
	"quit":     {arity: 1, handle: (*conn).cmdQuit},
	"hello":    {arity: -1, handle: (*conn).cmdHello},
	"select":   {arity: -1, handle: (*conn).cmdSelect},
	"shutdown": {arity: -1, handle: (*conn).cmdShutdown},
}

// errQuit signals that the client asked to disconnect. The connection loop
// treats it as a clean close rather than an error.
var errQuit = errors.New("client quit")

// dispatch routes one parsed command array to its handler.
func (c *conn) dispatch(args [][]byte) error {
	if len(args) == 0 {
		// An empty array is well-framed but meaningless. Redis ignores it,
		// and so do we, rather than dropping a client over it.
		return nil
	}

	name := strings.ToLower(string(args[0]))
	spec, ok := commands[name]
	if !ok {
		return c.w.WriteError(unknownCommandError(name, args[1:]))
	}

	if !spec.satisfiedBy(len(args)) {
		return c.w.WriteError(fmt.Sprintf("ERR wrong number of arguments for '%s' command", name))
	}

	return spec.handle(c, args)
}

// satisfiedBy reports whether an argument count meets the spec's arity.
func (s commandSpec) satisfiedBy(n int) bool {
	if s.arity < 0 {
		return n >= -s.arity
	}
	return n == s.arity
}

// unknownCommandError mirrors the format real Redis uses, because client
// libraries parse it when reporting errors to their callers.
func unknownCommandError(name string, rest [][]byte) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "ERR unknown command '%s', with args beginning with: ", name)
	for i, a := range rest {
		if i > 0 {
			sb.WriteString(", ")
		}
		fmt.Fprintf(&sb, "'%s'", a)
	}
	return sb.String()
}

func (c *conn) cmdPing(args [][]byte) error {
	if len(args) == 1 {
		return c.w.WriteSimpleString("PONG")
	}
	if len(args) > 2 {
		return c.w.WriteError("ERR wrong number of arguments for 'ping' command")
	}
	// PING with a payload echoes it as a bulk string, not a simple string.
	return c.w.WriteBulkString(args[1])
}

func (c *conn) cmdEcho(args [][]byte) error {
	return c.w.WriteBulkString(args[1])
}

func (c *conn) cmdSet(args [][]byte) error {
	// EX and NX are stretch goals (plan.md §7.5). Rejecting them is honest;
	// accepting and ignoring them would silently break expiry semantics for
	// any client that sends them.
	if len(args) > 3 {
		return c.w.WriteError("ERR syntax error")
	}

	if err := c.engine.Put(args[1], args[2]); err != nil {
		return c.replyEngineError(err)
	}
	return c.w.WriteSimpleString("OK")
}

func (c *conn) cmdGet(args [][]byte) error {
	v, err := c.engine.Get(args[1])
	if errors.Is(err, engine.ErrNotFound) {
		// The null bulk string, distinct from an empty one. A key holding
		// an empty value replies $0\r\n\r\n and reaches the branch below.
		return c.w.WriteNullBulkString()
	}
	if err != nil {
		return c.replyEngineError(err)
	}
	return c.w.WriteBulkString(v)
}

func (c *conn) cmdDel(args [][]byte) error {
	var n int64
	for _, key := range args[1:] {
		existed, err := c.engine.Delete(key)
		if err != nil {
			return c.replyEngineError(err)
		}
		if existed {
			n++
		}
	}
	return c.w.WriteInteger(n)
}

func (c *conn) cmdExists(args [][]byte) error {
	// EXISTS counts every occurrence, so EXISTS k k on a present key is 2.
	var n int64
	for _, key := range args[1:] {
		_, err := c.engine.Get(key)
		switch {
		case err == nil:
			n++
		case errors.Is(err, engine.ErrNotFound):
		default:
			return c.replyEngineError(err)
		}
	}
	return c.w.WriteInteger(n)
}

func (c *conn) cmdScan(args [][]byte) error {
	cursor, err := strconv.ParseUint(string(args[1]), 10, 64)
	if err != nil {
		return c.w.WriteError("ERR invalid cursor")
	}

	count := 10
	var match string
	for i := 2; i < len(args); i += 2 {
		if i+1 >= len(args) {
			return c.w.WriteError("ERR syntax error")
		}

		switch strings.ToLower(string(args[i])) {
		case "count":
			count, err = strconv.Atoi(string(args[i+1]))
			if err != nil || count < 1 {
				return c.w.WriteError("ERR value is not an integer or out of range")
			}
		case "match":
			match = string(args[i+1])
			if _, err := path.Match(match, ""); err != nil {
				return c.w.WriteError("ERR invalid pattern")
			}
		default:
			return c.w.WriteError("ERR syntax error")
		}
	}

	res, err := c.engine.Scan(cursor, count)
	if err != nil {
		return c.replyEngineError(err)
	}

	keys := res.Keys
	if match != "" {
		// Filtering after the fact means a page may come back empty while
		// the cursor still advances, which is exactly how Redis behaves and
		// why clients must loop until the cursor is zero.
		keys = filterMatching(keys, match)
	}

	// SCAN replies with a two-element array: the next cursor as a bulk
	// string, then the array of keys.
	if err := c.w.WriteArrayHeader(2); err != nil {
		return err
	}
	if err := c.w.WriteBulkString([]byte(strconv.FormatUint(res.Cursor, 10))); err != nil {
		return err
	}
	if err := c.w.WriteArrayHeader(len(keys)); err != nil {
		return err
	}
	for _, k := range keys {
		if err := c.w.WriteBulkString(k); err != nil {
			return err
		}
	}
	return nil
}

func filterMatching(keys [][]byte, pattern string) [][]byte {
	out := keys[:0:0]
	for _, k := range keys {
		// path.Match is glob-like but not identical to Redis's matcher; the
		// difference is documented in the README rather than papered over.
		if ok, err := path.Match(pattern, string(k)); err == nil && ok {
			out = append(out, k)
		}
	}
	return out
}

func (c *conn) cmdDBSize(args [][]byte) error {
	s, err := c.engine.Stats()
	if err != nil {
		return c.replyEngineError(err)
	}
	return c.w.WriteInteger(int64(s.Keys))
}

func (c *conn) cmdInfo(args [][]byte) error {
	s, err := c.engine.Stats()
	if err != nil {
		return c.replyEngineError(err)
	}

	// ADR-003 constrains INFO to a text blob, since RESP2 has no map type.
	var sb strings.Builder
	sb.WriteString("# Server\r\n")
	fmt.Fprintf(&sb, "strata_version:%s\r\n", c.srv.version)
	fmt.Fprintf(&sb, "process_id:%d\r\n", c.srv.pid)
	sb.WriteString("# Clients\r\n")
	fmt.Fprintf(&sb, "connected_clients:%d\r\n", c.srv.ConnCount())
	sb.WriteString("# Keyspace\r\n")
	// Approximate once compaction runs, per plan.md §7.5.
	fmt.Fprintf(&sb, "db0:keys=%d (approximate)\r\n", s.Keys)
	sb.WriteString("# Persistence\r\n")
	fmt.Fprintf(&sb, "sync_policy:%s\r\n", s.SyncPolicy)

	return c.w.WriteBulkString([]byte(sb.String()))
}

func (c *conn) cmdCompact(args [][]byte) error {
	// Non-standard, and a no-op until Phase 6 gives it something to do. It
	// replies OK rather than erroring so the test harness that calls it can
	// be written now and stay unchanged.
	return c.w.WriteSimpleString("OK")
}

func (c *conn) cmdFlushDB(args [][]byte) error {
	f, ok := c.engine.(flusher)
	if !ok {
		return c.w.WriteError("ERR FLUSHDB is not supported by this engine")
	}
	if err := f.Flush(); err != nil {
		return c.replyEngineError(err)
	}
	return c.w.WriteSimpleString("OK")
}

// cmdCommand answers the handshake modern redis-cli performs on connect.
// Without a reply here the client blocks before showing a prompt, which looks
// exactly like a broken server (plan.md §7.5).
func (c *conn) cmdCommand(args [][]byte) error {
	return c.w.WriteArrayHeader(0)
}

func (c *conn) cmdQuit(args [][]byte) error {
	if err := c.w.WriteSimpleString("OK"); err != nil {
		return err
	}
	return errQuit
}

// cmdHello refuses the protocol upgrade. ADR-003 fixes RESP2, and an error
// here is the documented path that makes conforming clients fall back.
func (c *conn) cmdHello(args [][]byte) error {
	return c.w.WriteError("NOPROTO unsupported protocol version")
}

// cmdSelect accepts database 0 only. Strata has a single keyspace, and
// clients configured with a database index send this on connect.
func (c *conn) cmdSelect(args [][]byte) error {
	if len(args) != 2 {
		return c.w.WriteError("ERR wrong number of arguments for 'select' command")
	}
	if string(args[1]) != "0" {
		return c.w.WriteError("ERR DB index is out of range")
	}
	return c.w.WriteSimpleString("OK")
}

// cmdShutdown exists because redis-benchmark and some clients send it. It is
// refused: a network command that stops the server is not something this
// project needs, and SIGTERM already drains correctly.
func (c *conn) cmdShutdown(args [][]byte) error {
	return c.w.WriteError("ERR SHUTDOWN is not supported; send SIGTERM instead")
}

// replyEngineError converts an engine failure into a client-visible error.
// Engine errors are values returned up rather than logged and swallowed, so
// the client learns the write did not happen.
func (c *conn) replyEngineError(err error) error {
	switch {
	case errors.Is(err, engine.ErrKeyTooLarge):
		return c.w.WriteError("ERR key exceeds maximum size")
	case errors.Is(err, engine.ErrValueTooLarge):
		return c.w.WriteError("ERR value exceeds maximum size")
	case errors.Is(err, engine.ErrClosed):
		return c.w.WriteError("ERR server is shutting down")
	default:
		c.srv.log.Error("engine error", "err", err)
		return c.w.WriteError("ERR internal error")
	}
}

// commandNames lists the dispatch table's keys, for tests and for the startup
// banner.
func commandNames() []string {
	names := make([]string, 0, len(commands))
	for n := range commands {
		names = append(names, n)
	}
	return names
}
