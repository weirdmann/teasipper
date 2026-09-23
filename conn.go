package teasipper

import (
	"context"
	"io"
	"net"
	"sync"
	"time"
)

// Conn is a session-owned TCP stream. Read and Write use caller-owned buffers
// only for the duration of each call; there is no message framing or queue.
// One Read and one Write may run concurrently. Calls in the same direction
// are serialized so their deadlines cannot interfere with each other.
type Conn struct {
	raw          net.Conn
	session      *session
	readTimeout  time.Duration
	writeTimeout time.Duration
	readGate     chan struct{}
	writeGate    chan struct{}
	closeOnce    sync.Once
}

func newConn(raw net.Conn, s *session, cfg Config) *Conn {
	c := &Conn{
		raw: raw, session: s, readTimeout: cfg.ReadTimeout, writeTimeout: cfg.WriteTimeout,
		readGate: make(chan struct{}, 1), writeGate: make(chan struct{}, 1),
	}
	c.readGate <- struct{}{}
	c.writeGate <- struct{}{}
	return c
}

func (c *Conn) LocalAddr() net.Addr  { return c.raw.LocalAddr() }
func (c *Conn) RemoteAddr() net.Addr { return c.raw.RemoteAddr() }

// Close is idempotent and unblocks pending I/O on this Conn.
func (c *Conn) Close() error {
	var err error
	c.closeOnce.Do(func() {
		err = c.raw.Close()
		c.session.unregister(c)
	})
	return err
}

func (c *Conn) Read(p []byte) (int, error) {
	return c.ReadContext(context.Background(), p)
}

func (c *Conn) Write(p []byte) (int, error) {
	return c.WriteContext(context.Background(), p)
}

// ReadContext cancels a blocked read without closing the connection. If data
// and an error arrive together, it returns both, as io.Reader permits.
func (c *Conn) ReadContext(ctx context.Context, p []byte) (int, error) {
	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	case <-c.readGate:
	}
	defer func() { c.readGate <- struct{}{} }()
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if len(p) == 0 {
		return 0, nil
	}
	if c.readTimeout == 0 && ctx.Done() == nil {
		return c.raw.Read(p)
	}
	if ctx.Done() == nil {
		if err := c.raw.SetReadDeadline(time.Now().Add(c.readTimeout)); err != nil {
			return 0, err
		}
		n, err := c.raw.Read(p)
		_ = c.raw.SetReadDeadline(time.Time{})
		return n, err
	}
	cleanup, err := ioDeadline(ctx, c.readTimeout, c.raw.SetReadDeadline)
	if err != nil {
		return 0, err
	}
	n, err := c.raw.Read(p)
	cleanup()
	if ctx.Err() != nil && err != nil {
		return n, ctx.Err()
	}
	return n, err
}

// WriteContext writes all of p or returns the number of bytes accepted and an
// error. Cancellation interrupts a blocked write without closing the Conn.
func (c *Conn) WriteContext(ctx context.Context, p []byte) (int, error) {
	select {
	case <-ctx.Done():
		return 0, ctx.Err()
	case <-c.writeGate:
	}
	defer func() { c.writeGate <- struct{}{} }()
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if len(p) == 0 {
		return 0, nil
	}
	if c.writeTimeout == 0 && ctx.Done() == nil {
		return writeAll(c.raw, p)
	}
	if ctx.Done() == nil {
		if err := c.raw.SetWriteDeadline(time.Now().Add(c.writeTimeout)); err != nil {
			return 0, err
		}
		n, err := writeAll(c.raw, p)
		_ = c.raw.SetWriteDeadline(time.Time{})
		return n, err
	}
	cleanup, err := ioDeadline(ctx, c.writeTimeout, c.raw.SetWriteDeadline)
	if err != nil {
		return 0, err
	}
	defer cleanup()
	n, err := writeAll(c.raw, p)
	if ctx.Err() != nil && err != nil {
		return n, ctx.Err()
	}
	return n, err
}

func writeAll(dst io.Writer, p []byte) (int, error) {
	total := 0
	for total < len(p) {
		n, err := dst.Write(p[total:])
		total += n
		if err != nil {
			return total, err
		}
		if n == 0 {
			return total, io.ErrShortWrite
		}
	}
	return total, nil
}

// ioDeadline installs one deadline for this operation and restores the idle
// state only after an optional context cancellation callback has finished.
func ioDeadline(ctx context.Context, timeout time.Duration, set func(time.Time) error) (func(), error) {
	if timeout == 0 && ctx.Done() == nil {
		return func() {}, nil
	}
	var deadline time.Time
	if timeout > 0 {
		deadline = time.Now().Add(timeout)
	}
	if d, ok := ctx.Deadline(); ok && (deadline.IsZero() || d.Before(deadline)) {
		deadline = d
	}
	if err := set(deadline); err != nil {
		return nil, err
	}
	var done chan struct{}
	var stop func() bool
	if ctx.Done() != nil {
		done = make(chan struct{})
		stop = context.AfterFunc(ctx, func() {
			_ = set(time.Now())
			close(done)
		})
	}
	return func() {
		if stop != nil && !stop() {
			<-done
		}
		_ = set(time.Time{})
	}, nil
}
