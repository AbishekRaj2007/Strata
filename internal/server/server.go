package server

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/AbishekRaj2007/Strata/internal/engine"
	"github.com/AbishekRaj2007/Strata/internal/log"
	"github.com/AbishekRaj2007/Strata/internal/resp"
)

// Buffer sizes per connection. 16 KB matches plan.md T1.2 and is large enough
// that a pipelined batch is usually one read and one write.
const (
	readBufferSize  = 16 << 10
	writeBufferSize = 16 << 10
)

// DefaultIdleTimeout bounds how long a connection may sit without sending a
// command, so that dead peers are reaped rather than held forever. It is
// deliberately generous: redis-cli sessions idle for minutes between commands.
const DefaultIdleTimeout = 5 * time.Minute

// DefaultWriteTimeout bounds a single flush. It is far shorter than the idle
// timeout because the two mean different things: an idle connection is a
// client with nothing to say, while a connection that cannot absorb a reply
// within this window has stopped reading and is not coming back.
//
// Without it a client that pipelines a batch and then stops reading blocks its
// handler in Flush once the kernel send buffer fills. That handler is not idle,
// so shutdown will not close it, and the drain in Shutdown is left waiting on
// a goroutine that never returns.
const DefaultWriteTimeout = 30 * time.Second

// Config configures a Server.
type Config struct {
	Addr        string
	Engine      engine.Engine
	Logger      log.Logger
	Version     string
	IdleTimeout time.Duration

	// WriteTimeout bounds one flush. Zero selects DefaultWriteTimeout; a
	// negative value disables the deadline, which only a test should want.
	WriteTimeout time.Duration
}

// Server accepts connections and serves the Strata command set over RESP2.
type Server struct {
	cfg      Config
	log      log.Logger
	engine   engine.Engine
	version  string
	pid      int
	listener net.Listener

	// conns tracks live connections so shutdown can close them after the
	// accept loop stops. The mutex covers the map only; a connection's own
	// I/O happens outside it.
	mu    sync.Mutex
	conns map[*conn]struct{}

	// wg counts in-flight connection goroutines, and is what Shutdown waits
	// on to know every command has finished.
	wg sync.WaitGroup

	// closing flips once shutdown begins, so the accept loop can tell a
	// deliberate listener close from a real accept failure.
	closing atomic.Bool

	connCount atomic.Int64
}

// New returns a Server ready to Serve. It does not bind; Listen does that, so
// that a caller can report a bind failure before announcing startup.
func New(cfg Config) (*Server, error) {
	if cfg.Engine == nil {
		return nil, errors.New("server: Engine is required")
	}
	if cfg.Logger == nil {
		cfg.Logger = log.Discard()
	}
	if cfg.IdleTimeout == 0 {
		cfg.IdleTimeout = DefaultIdleTimeout
	}
	if cfg.WriteTimeout == 0 {
		cfg.WriteTimeout = DefaultWriteTimeout
	}

	return &Server{
		cfg:     cfg,
		log:     cfg.Logger,
		engine:  cfg.Engine,
		version: cfg.Version,
		pid:     os.Getpid(),
		conns:   make(map[*conn]struct{}),
	}, nil
}

// Listen binds the configured address. Separating it from Serve means a
// caller learns about a port conflict before it logs that the server started.
func (s *Server) Listen() error {
	ln, err := net.Listen("tcp", s.cfg.Addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", s.cfg.Addr, err)
	}
	s.listener = ln
	return nil
}

// Addr reports the bound address, which is how a test that binds :0 discovers
// the port the kernel chose.
func (s *Server) Addr() net.Addr {
	if s.listener == nil {
		return nil
	}
	return s.listener.Addr()
}

// ConnCount reports the number of live connections, for INFO.
func (s *Server) ConnCount() int64 {
	return s.connCount.Load()
}

// Serve accepts connections until Shutdown is called. It returns nil on a
// deliberate shutdown and an error on a genuine accept failure.
func (s *Server) Serve() error {
	if s.listener == nil {
		return errors.New("server: Listen must be called before Serve")
	}

	s.log.Info("accepting connections", "addr", s.listener.Addr().String())

	for {
		netConn, err := s.listener.Accept()
		if err != nil {
			// A closed listener during shutdown is the expected exit, not
			// a failure to report.
			if s.closing.Load() {
				return nil
			}
			return fmt.Errorf("accept: %w", err)
		}

		c := s.newConn(netConn)

		// Registering before starting the goroutine closes the window in
		// which a connection exists but shutdown cannot see it.
		if !s.register(c) {
			// Shutdown began between Accept and here; refuse the client
			// rather than serving it past the drain.
			_ = netConn.Close()
			return nil
		}

		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer s.unregister(c)
			c.serve()
		}()
	}
}

func (s *Server) register(c *conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closing.Load() {
		return false
	}
	s.conns[c] = struct{}{}
	s.connCount.Add(1)
	return true
}

func (s *Server) unregister(c *conn) {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.conns, c)
	s.connCount.Add(-1)
}

// Shutdown stops accepting, waits for in-flight commands to finish, closes
// every connection, and then closes the engine. The ordering is the point:
// the engine must outlive the last command that touches it, or a command in
// flight reads from a closed engine.
//
// If ctx expires first, remaining connections are closed underneath their
// handlers and the context's error is returned. The engine is still closed,
// because leaving it open on a shutdown path loses more than it protects.
func (s *Server) Shutdown(ctx context.Context) error {
	// Setting this before closing the listener means the accept loop reads
	// it as deliberate no matter which side wins the race.
	if !s.closing.CompareAndSwap(false, true) {
		return nil
	}

	var errs []error

	if s.listener != nil {
		if err := s.listener.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close listener: %w", err))
		}
	}

	s.log.Info("draining connections", "count", s.ConnCount())

	// Unblock connections parked in a read. Without this a client that
	// opened a socket and sent nothing holds shutdown until its idle
	// timeout, which is minutes.
	s.closeIdleConns()

	drained := make(chan struct{})
	go func() {
		s.wg.Wait()
		close(drained)
	}()

	select {
	case <-drained:
		s.log.Info("all connections drained")
	case <-ctx.Done():
		// Deadline hit: close everything outright. Handlers see a write
		// error on a closed socket and exit.
		s.log.Warn("drain deadline exceeded; closing connections", "remaining", s.ConnCount())
		s.closeAllConns()
		<-drained
		errs = append(errs, ctx.Err())
	}

	if err := s.engine.Close(); err != nil {
		errs = append(errs, fmt.Errorf("close engine: %w", err))
	}

	return errors.Join(errs...)
}

// closeIdleConns closes connections that are waiting on a read. A connection
// mid-command is left alone so its reply is written before it goes.
func (s *Server) closeIdleConns() {
	s.mu.Lock()
	defer s.mu.Unlock()

	for c := range s.conns {
		if c.idle.Load() {
			c.closeSocket()
		} else {
			// Mid-command: ask it to stop after this reply.
			c.quitting.Store(true)
		}
	}
}

func (s *Server) closeAllConns() {
	s.mu.Lock()
	defer s.mu.Unlock()

	for c := range s.conns {
		c.closeSocket()
	}
}

// conn is one client connection.
type conn struct {
	srv    *Server
	engine engine.Engine
	nc     net.Conn
	r      *resp.Reader
	w      *resp.Writer
	bw     *bufio.Writer

	// idle is true while the connection is blocked reading, which shutdown
	// uses to tell "waiting for a client" from "running a command".
	idle atomic.Bool

	// quitting asks the loop to stop after the current command completes.
	quitting atomic.Bool

	closeOnce sync.Once
}

func (s *Server) newConn(nc net.Conn) *conn {
	bw := bufio.NewWriterSize(nc, writeBufferSize)
	return &conn{
		srv:    s,
		engine: s.engine,
		nc:     nc,
		r:      resp.NewReader(bufio.NewReaderSize(nc, readBufferSize)),
		w:      resp.NewWriter(bw),
		bw:     bw,
	}
}

// serve runs the read-dispatch-reply loop until the client disconnects, the
// connection errors, or shutdown asks it to stop.
func (c *conn) serve() {
	remote := c.nc.RemoteAddr().String()
	c.srv.log.Debug("connection opened", "remote", remote)

	defer func() {
		// A panic in a handler must take down one connection, not the
		// server. The engine's own invariants are unaffected -- nothing
		// here holds a lock across the dispatch.
		if r := recover(); r != nil {
			c.srv.log.Error("panic serving connection", "remote", remote, "panic", r)
		}
		c.closeSocket()
		c.srv.log.Debug("connection closed", "remote", remote)
	}()

	for {
		if c.quitting.Load() {
			return
		}

		if c.srv.cfg.IdleTimeout > 0 {
			if err := c.nc.SetReadDeadline(time.Now().Add(c.srv.cfg.IdleTimeout)); err != nil {
				return
			}
		}

		c.idle.Store(true)
		value, err := c.r.ReadValue()
		c.idle.Store(false)

		if err != nil {
			c.reportReadError(err, remote)
			return
		}

		args, err := commandArgs(value)
		if err != nil {
			// A well-framed value that is not a command array is a client
			// bug worth reporting, but not worth dropping the connection.
			if werr := c.w.WriteError(err.Error()); werr != nil {
				return
			}
			if ferr := c.flush(); ferr != nil {
				return
			}
			continue
		}

		derr := c.dispatch(args)
		if derr != nil && !errors.Is(derr, errQuit) {
			c.srv.log.Debug("write failed", "remote", remote, "err", derr)
			return
		}

		// Flushing once per command rather than once per reply is what
		// makes a pipelined batch cheap: bufio coalesces the replies and
		// this is the single write syscall. A batch that fills the buffer
		// flushes earlier, which is the desired backpressure.
		if !c.r.Buffered() || errors.Is(derr, errQuit) {
			if err := c.flush(); err != nil {
				return
			}
		}

		if errors.Is(derr, errQuit) {
			return
		}
	}
}

// reportReadError classifies why a read ended. A clean disconnect between
// commands is normal and logged at debug; a malformed frame is the peer's
// fault and gets an error reply before the connection goes.
func (c *conn) reportReadError(err error, remote string) {
	switch {
	case errors.Is(err, resp.ErrProtocol):
		// Best-effort: the peer is already misbehaving, so a failed write
		// here changes nothing.
		_ = c.w.WriteError("ERR Protocol error: " + err.Error())
		_ = c.flush()
		c.srv.log.Debug("protocol error", "remote", remote, "err", err)
	case errors.Is(err, os.ErrDeadlineExceeded):
		c.srv.log.Debug("connection idle timeout", "remote", remote)
	default:
		c.srv.log.Debug("connection read ended", "remote", remote, "err", err)
	}
}

// flush writes buffered replies under a deadline, so that a peer which has
// stopped reading cannot pin this goroutine indefinitely and stall shutdown.
func (c *conn) flush() error {
	if c.srv.cfg.WriteTimeout > 0 {
		if err := c.nc.SetWriteDeadline(time.Now().Add(c.srv.cfg.WriteTimeout)); err != nil {
			return err
		}
	}
	return c.bw.Flush()
}

func (c *conn) closeSocket() {
	c.closeOnce.Do(func() {
		_ = c.nc.Close()
	})
}

// commandArgs converts a decoded value into command arguments. Clients always
// send commands as an array of bulk strings; anything else is a client error.
func commandArgs(v resp.Value) ([][]byte, error) {
	if v.Type != resp.Array || v.Null {
		return nil, errors.New("ERR Protocol error: expected an array of bulk strings")
	}

	args := make([][]byte, 0, len(v.Array))
	for _, elem := range v.Array {
		if elem.Type != resp.BulkString || elem.Null {
			return nil, errors.New("ERR Protocol error: expected a bulk string")
		}
		args = append(args, elem.Bytes)
	}
	return args, nil
}
