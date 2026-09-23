package teasipper

import (
	"context"
	"errors"
	"net"
	"sync"
	"time"
)

// Endpoint owns one session at a time. The zero value is not usable; call
// NewEndpoint. Start and Reset serialize with each other; Close also cancels
// connection setup in progress.
type Endpoint struct {
	opMu        sync.Mutex
	closeMu     sync.Mutex
	mu          sync.Mutex
	cfg         Config
	session     *session
	startCancel context.CancelFunc
	closed      bool
}

type session struct {
	cfg        Config
	mode       Mode
	listener   *net.TCPListener
	client     *Conn
	acceptGate chan struct{}
	mu         sync.Mutex
	conns      map[*Conn]struct{}
	closed     bool
}

func NewEndpoint(cfg Config) (*Endpoint, error) {
	cfg, err := normalizeConfig(cfg)
	if err != nil {
		return nil, err
	}
	return &Endpoint{cfg: cfg}, nil
}

// Config returns the configuration that the next Start or Reset will use.
func (e *Endpoint) Config() Config {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.cfg
}

// Addr returns the current listener address (server) or local connection
// address (client). It returns nil while the endpoint is stopped.
func (e *Endpoint) Addr() net.Addr {
	e.mu.Lock()
	s := e.session
	e.mu.Unlock()
	if s == nil {
		return nil
	}
	if s.listener != nil {
		return s.listener.Addr()
	}
	return s.client.LocalAddr()
}

// Start binds a server listener or connects a client. ctx limits setup only;
// the resulting session remains active until Reset or Close.
func (e *Endpoint) Start(ctx context.Context) error {
	e.opMu.Lock()
	defer e.opMu.Unlock()
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return ErrClosed
	}
	if e.session != nil {
		e.mu.Unlock()
		return ErrRunning
	}
	e.mu.Unlock()
	return e.start(ctx)
}

// Reset interrupts and closes the old session before starting a replacement.
// A nil cfg keeps the previous configuration. A failed restart leaves the
// endpoint stopped with the new configuration available for another Reset.
func (e *Endpoint) Reset(ctx context.Context, cfg *Config) error {
	var next Config
	if cfg != nil {
		var err error
		next, err = normalizeConfig(*cfg)
		if err != nil {
			return err
		}
	}
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return ErrClosed
	}
	if e.startCancel != nil {
		e.startCancel()
	}
	e.mu.Unlock()

	e.opMu.Lock()
	defer e.opMu.Unlock()
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return ErrClosed
	}
	old := e.session
	e.session = nil
	if cfg != nil {
		e.cfg = next
	}
	e.mu.Unlock()
	if old != nil {
		old.close()
	}
	return e.start(ctx)
}

func (e *Endpoint) start(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	setupCtx, cancel := context.WithCancel(ctx)
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		cancel()
		return ErrClosed
	}
	e.startCancel = cancel
	cfg := e.cfg
	e.mu.Unlock()

	var s *session
	var err error
	if cfg.Mode == ModeServer {
		var l net.Listener
		l, err = (&net.ListenConfig{}).Listen(setupCtx, cfg.Network, cfg.Address)
		if err == nil {
			// A TCP network always yields a TCPListener.
			s = &session{cfg: cfg, mode: ModeServer, listener: l.(*net.TCPListener), conns: make(map[*Conn]struct{}), acceptGate: make(chan struct{}, 1)}
			s.acceptGate <- struct{}{}
		}
	} else {
		var local *net.TCPAddr
		if cfg.LocalAddress != "" {
			local, err = net.ResolveTCPAddr(cfg.Network, cfg.LocalAddress)
		}
		if err == nil {
			var raw net.Conn
			raw, err = (&net.Dialer{Timeout: cfg.DialTimeout, LocalAddr: local}).DialContext(setupCtx, cfg.Network, cfg.Address)
			if err == nil {
				s = &session{cfg: cfg, mode: ModeClient, conns: make(map[*Conn]struct{})}
				s.client = newConn(raw, s, cfg)
				s.conns[s.client] = struct{}{}
			}
		}
	}

	e.mu.Lock()
	e.startCancel = nil
	closed := e.closed
	setupErr := setupCtx.Err()
	if !closed && err == nil && setupErr == nil {
		e.session = s
	}
	e.mu.Unlock()
	defer cancel()
	if closed {
		if s != nil {
			s.close()
		}
		return ErrClosed
	}
	if err != nil {
		return err
	}
	if setupErr != nil {
		s.close()
		return setupErr
	}
	return nil
}

// Accept returns a managed connection in server mode. It may be called
// concurrently; calls are serialized on the listener. Canceling ctx does not
// close the listener. Reset and Close unblock a pending Accept.
func (e *Endpoint) Accept(ctx context.Context) (*Conn, error) {
	e.mu.Lock()
	s := e.session
	mode := e.cfg.Mode
	e.mu.Unlock()
	if s == nil {
		return nil, ErrNotRunning
	}
	if mode != ModeServer {
		return nil, ErrWrongMode
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.acceptGate:
	}
	defer func() { s.acceptGate <- struct{}{} }()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.isClosed() {
		return nil, ErrSessionClosed
	}
	l := s.listener
	if deadline, ok := ctx.Deadline(); ok {
		if err := l.SetDeadline(deadline); err != nil {
			return nil, ErrSessionClosed
		}
	}
	var done chan struct{}
	var stop func() bool
	if ctx.Done() != nil {
		done = make(chan struct{})
		stop = context.AfterFunc(ctx, func() {
			_ = l.SetDeadline(time.Now())
			close(done)
		})
	}
	raw, err := l.AcceptTCP()
	if stop != nil && !stop() {
		<-done
	}
	_ = l.SetDeadline(time.Time{})
	if ctx.Err() != nil {
		if raw != nil {
			_ = raw.Close()
		}
		return nil, ctx.Err()
	}
	if err != nil {
		if s.isClosed() || errors.Is(err, net.ErrClosed) {
			return nil, ErrSessionClosed
		}
		return nil, err
	}
	c := newConn(raw, s, s.cfg)
	if !s.register(c) {
		_ = c.Close()
		return nil, ErrSessionClosed
	}
	return c, nil
}

// Conn returns the current client connection. Reset closes the old Conn and
// creates a distinct one; callers should fetch it again after each reset.
func (e *Endpoint) Conn() (*Conn, error) {
	e.mu.Lock()
	s := e.session
	mode := e.cfg.Mode
	e.mu.Unlock()
	if s == nil {
		return nil, ErrNotRunning
	}
	if mode != ModeClient {
		return nil, ErrWrongMode
	}
	return s.client, nil
}

// Close permanently shuts down the endpoint. Repeated calls are safe.
func (e *Endpoint) Close() error {
	e.closeMu.Lock()
	defer e.closeMu.Unlock()
	e.mu.Lock()
	e.closed = true
	if e.startCancel != nil {
		e.startCancel()
	}
	s := e.session
	e.session = nil
	e.mu.Unlock()
	if s != nil {
		s.close()
	}
	e.opMu.Lock()
	e.opMu.Unlock()
	return nil
}

func (s *session) isClosed() bool {
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	return closed
}

func (s *session) register(c *Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	s.conns[c] = struct{}{}
	return true
}

func (s *session) unregister(c *Conn) {
	s.mu.Lock()
	delete(s.conns, c)
	s.mu.Unlock()
}

func (s *session) close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	conns := s.conns
	s.conns = nil
	s.mu.Unlock()
	if s.listener != nil {
		_ = s.listener.Close()
	}
	for c := range conns {
		_ = c.Close()
	}
}
