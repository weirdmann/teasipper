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
	raw           net.Conn
	session       *session
	readTimeout   time.Duration
	writeTimeout  time.Duration
	readGate      chan struct{}
	writeGate     chan struct{}
	readDeadline  deadlineState
	writeDeadline deadlineState
	closeOnce     sync.Once
}

var _ net.Conn = (*Conn)(nil)

// deadlineState combines an absolute, caller-managed deadline with the limit
// of one active operation. A canceled context wins until that operation ends.
// SetDeadline must be able to interrupt a Read or Write holding its gate, so
// this mutex is independent of the corresponding I/O gate.
type deadlineState struct {
	mu        sync.Mutex
	manual    time.Time
	operation time.Time
	canceled  bool
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

// SetDeadline sets an absolute deadline for future and pending reads and
// writes. A zero value removes the caller-managed limit. Configured per-call
// timeouts and context deadlines may impose an earlier limit.
func (c *Conn) SetDeadline(t time.Time) error {
	if err := c.SetReadDeadline(t); err != nil {
		return err
	}
	return c.SetWriteDeadline(t)
}

// SetReadDeadline sets an absolute deadline that persists across Read calls.
// It can interrupt a blocked read, including one using ReadContext.
func (c *Conn) SetReadDeadline(t time.Time) error {
	return c.readDeadline.setManual(t, c.raw.SetReadDeadline)
}

// SetWriteDeadline sets an absolute deadline that persists across Write calls.
// It can interrupt a blocked write, including one using WriteContext.
func (c *Conn) SetWriteDeadline(t time.Time) error {
	return c.writeDeadline.setManual(t, c.raw.SetWriteDeadline)
}

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
		if err := c.readDeadline.start(operationDeadline(ctx, c.readTimeout), c.raw.SetReadDeadline); err != nil {
			return 0, err
		}
		n, err := c.raw.Read(p)
		c.readDeadline.end(c.raw.SetReadDeadline)
		return n, err
	}
	cleanup, err := c.readDeadline.begin(ctx, c.readTimeout, c.raw.SetReadDeadline)
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
		if err := c.writeDeadline.start(operationDeadline(ctx, c.writeTimeout), c.raw.SetWriteDeadline); err != nil {
			return 0, err
		}
		n, err := writeAll(c.raw, p)
		c.writeDeadline.end(c.raw.SetWriteDeadline)
		return n, err
	}
	cleanup, err := c.writeDeadline.begin(ctx, c.writeTimeout, c.raw.SetWriteDeadline)
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

func (d *deadlineState) effective() time.Time {
	deadline := d.manual
	if !d.operation.IsZero() && (deadline.IsZero() || d.operation.Before(deadline)) {
		deadline = d.operation
	}
	if d.canceled {
		return time.Now()
	}
	return deadline
}

func (d *deadlineState) setManual(t time.Time, set func(time.Time) error) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	previous := d.manual
	d.manual = t
	if err := set(d.effective()); err != nil {
		d.manual = previous
		return err
	}
	return nil
}

func operationDeadline(ctx context.Context, timeout time.Duration) time.Time {
	var deadline time.Time
	if timeout > 0 {
		deadline = time.Now().Add(timeout)
	}
	if contextDeadline, ok := ctx.Deadline(); ok && (deadline.IsZero() || contextDeadline.Before(deadline)) {
		deadline = contextDeadline
	}
	return deadline
}

func (d *deadlineState) start(operation time.Time, set func(time.Time) error) error {
	d.mu.Lock()
	d.operation = operation
	err := set(d.effective())
	if err != nil {
		d.operation = time.Time{}
	}
	d.mu.Unlock()
	return err
}

func (d *deadlineState) end(set func(time.Time) error) {
	d.mu.Lock()
	d.operation = time.Time{}
	d.canceled = false
	_ = set(d.effective())
	d.mu.Unlock()
}

// begin installs the earliest of the manual, configured, and context
// deadlines. Cleanup waits for the cancellation callback before restoring the
// manual deadline, so an old callback cannot poison a later operation.
func (d *deadlineState) begin(ctx context.Context, timeout time.Duration, set func(time.Time) error) (func(), error) {
	if err := d.start(operationDeadline(ctx, timeout), set); err != nil {
		return nil, err
	}
	var done chan struct{}
	var stop func() bool
	if ctx.Done() != nil {
		done = make(chan struct{})
		stop = context.AfterFunc(ctx, func() {
			d.mu.Lock()
			d.canceled = true
			_ = set(d.effective())
			d.mu.Unlock()
			close(done)
		})
	}
	return func() {
		if stop != nil && !stop() {
			<-done
		}
		d.end(set)
	}, nil
}
