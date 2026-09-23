package teasipper

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"runtime"
	"sync"
	"testing"
	"time"
)

const integrationTimeout = 5 * time.Second

func integrationContext(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	return context.WithTimeout(context.Background(), integrationTimeout)
}

func integrationEndpoint(t *testing.T, cfg Config) *Endpoint {
	t.Helper()
	e, err := NewEndpoint(cfg)
	if err != nil {
		t.Fatalf("NewEndpoint(%+v): %v", cfg, err)
	}
	if err := e.Start(context.Background()); err != nil {
		t.Fatalf("Start(%+v): %v", cfg, err)
	}
	t.Cleanup(func() { _ = e.Close() })
	return e
}

func integrationDial(t *testing.T, addr net.Addr) net.Conn {
	t.Helper()
	if addr == nil {
		t.Fatal("started server has no address")
	}
	c, err := (&net.Dialer{Timeout: integrationTimeout}).Dial("tcp", addr.String())
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func integrationReadFull(ctx context.Context, c *Conn, dst []byte) error {
	for len(dst) != 0 {
		n, err := c.ReadContext(ctx, dst)
		if n > 0 {
			dst = dst[n:]
		}
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrNoProgress
		}
	}
	return nil
}

func integrationWrite(ctx context.Context, c *Conn, data []byte) error {
	n, err := c.WriteContext(ctx, data)
	if err != nil {
		return err
	}
	if n != len(data) {
		return fmt.Errorf("short write: %d of %d bytes", n, len(data))
	}
	return nil
}

func integrationReceiveError(t *testing.T, name string, result <-chan error) {
	t.Helper()
	select {
	case err := <-result:
		if err == nil {
			t.Fatalf("%s returned nil after reset", name)
		}
	case <-time.After(integrationTimeout):
		t.Fatalf("%s did not return after reset", name)
	}
}

func TestEndpointClientServerRoundTrip(t *testing.T) {
	server := integrationEndpoint(t, Config{Mode: ModeServer, Network: "tcp", Address: "127.0.0.1:0"})
	client := integrationEndpoint(t, Config{Mode: ModeClient, Network: "tcp", Address: server.Addr().String()})
	ctx, cancel := integrationContext(t)
	defer cancel()

	serverConn, err := server.Accept(ctx)
	if err != nil {
		t.Fatal(err)
	}
	clientConn, err := client.Conn()
	if err != nil {
		t.Fatal(err)
	}
	if got := server.Config(); got.Mode != ModeServer || got.Address != "127.0.0.1:0" {
		t.Fatalf("server config changed: %+v", got)
	}
	if serverConn.LocalAddr() == nil || serverConn.RemoteAddr() == nil {
		t.Fatal("accepted connection has no addresses")
	}

	for _, tc := range []struct {
		name string
		from *Conn
		to   *Conn
		data []byte
	}{
		{"client to server", clientConn, serverConn, []byte("request")},
		{"server to client", serverConn, clientConn, []byte("response")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := integrationWrite(ctx, tc.from, tc.data); err != nil {
				t.Fatal(err)
			}
			got := make([]byte, len(tc.data))
			if err := integrationReadFull(ctx, tc.to, got); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, tc.data) {
				t.Fatalf("received %q, want %q", got, tc.data)
			}
		})
	}
}

func TestEndpointResetInterruptsReadAndAccept(t *testing.T) {
	server := integrationEndpoint(t, Config{Mode: ModeServer, Network: "tcp", Address: "127.0.0.1:0"})
	remote := integrationDial(t, server.Addr())
	ctx, cancel := integrationContext(t)
	defer cancel()
	oldConn, err := server.Accept(ctx)
	if err != nil {
		t.Fatal(err)
	}

	readStarted := make(chan struct{})
	readResult := make(chan error, 1)
	go func() {
		close(readStarted)
		_, err := oldConn.ReadContext(context.Background(), make([]byte, 1))
		readResult <- err
	}()
	acceptStarted := make(chan struct{})
	acceptResult := make(chan error, 1)
	go func() {
		close(acceptStarted)
		_, err := server.Accept(context.Background())
		acceptResult <- err
	}()
	<-readStarted
	<-acceptStarted

	if err := server.Reset(ctx, nil); err != nil {
		t.Fatalf("Reset(nil): %v", err)
	}
	integrationReceiveError(t, "blocked ReadContext", readResult)
	integrationReceiveError(t, "blocked Accept", acceptResult)
	if _, err := oldConn.Write([]byte("stale")); err == nil {
		t.Fatal("old connection accepted a write after reset")
	}
	if got := server.Config(); got.Mode != ModeServer || got.Address != "127.0.0.1:0" {
		t.Fatalf("Reset(nil) did not preserve config: %+v", got)
	}

	newRemote := integrationDial(t, server.Addr())
	newConn, err := server.Accept(ctx)
	if err != nil {
		t.Fatalf("Accept after reset: %v", err)
	}
	if _, err := newRemote.Write([]byte("new")); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 3)
	if err := integrationReadFull(ctx, newConn, got); err != nil {
		t.Fatal(err)
	}
	if string(got) != "new" {
		t.Fatalf("new session received %q", got)
	}
	_ = remote.Close()
}

func TestEndpointResetChangesModeAndAddress(t *testing.T) {
	e := integrationEndpoint(t, Config{Mode: ModeServer, Network: "tcp", Address: "127.0.0.1:0"})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	ctx, cancel := integrationContext(t)
	defer cancel()

	clientCfg := Config{Mode: ModeClient, Network: "tcp", Address: listener.Addr().String(), DialTimeout: time.Second}
	if err := e.Reset(ctx, &clientCfg); err != nil {
		t.Fatalf("server to client reset: %v", err)
	}
	if got := e.Config(); got.Mode != ModeClient || got.Address != clientCfg.Address {
		t.Fatalf("client config: %+v", got)
	}
	if tcpListener, ok := listener.(*net.TCPListener); ok {
		_ = tcpListener.SetDeadline(time.Now().Add(integrationTimeout))
	}
	remote, err := listener.Accept()
	if err != nil {
		t.Fatalf("accept client connection: %v", err)
	}
	defer remote.Close()
	clientConn, err := e.Conn()
	if err != nil {
		t.Fatal(err)
	}
	if err := integrationWrite(ctx, clientConn, []byte("mode")); err != nil {
		t.Fatal(err)
	}
	_ = remote.SetReadDeadline(time.Now().Add(integrationTimeout))
	buf := make([]byte, 4)
	if _, err := io.ReadFull(remote, buf); err != nil || string(buf) != "mode" {
		t.Fatalf("client write: data=%q err=%v", buf, err)
	}

	serverCfg := Config{Mode: ModeServer, Network: "tcp", Address: "127.0.0.1:0"}
	if err := e.Reset(ctx, &serverCfg); err != nil {
		t.Fatalf("client to server reset: %v", err)
	}
	if _, err := clientConn.Write([]byte("stale")); err == nil {
		t.Fatal("old client connection survived mode switch")
	}
	if _, err := e.Conn(); err == nil {
		t.Fatal("Conn succeeded in server mode")
	}
	newRemote := integrationDial(t, e.Addr())
	accepted, err := e.Accept(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := newRemote.Write([]byte("server")); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 6)
	if err := integrationReadFull(ctx, accepted, got); err != nil || string(got) != "server" {
		t.Fatalf("server mode read: data=%q err=%v", got, err)
	}
}

func TestEndpointClientReconnectAfterRemoteClose(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if tcpListener, ok := listener.(*net.TCPListener); ok {
		_ = tcpListener.SetDeadline(time.Now().Add(integrationTimeout))
	}
	client := integrationEndpoint(t, Config{Mode: ModeClient, Network: "tcp", Address: listener.Addr().String(), DialTimeout: time.Second})
	ctx, cancel := integrationContext(t)
	defer cancel()
	firstRemote, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	first, err := client.Conn()
	if err != nil {
		t.Fatal(err)
	}
	_ = firstRemote.Close()
	if _, err := first.ReadContext(ctx, make([]byte, 1)); err == nil {
		t.Fatal("remote close did not end first connection")
	}
	if err := client.Reset(ctx, nil); err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	secondRemote, err := listener.Accept()
	if err != nil {
		t.Fatalf("accept reconnected client: %v", err)
	}
	defer secondRemote.Close()
	second, err := client.Conn()
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("reset returned the old connection")
	}
	if err := integrationWrite(ctx, second, []byte("again")); err != nil {
		t.Fatal(err)
	}
	_ = secondRemote.SetReadDeadline(time.Now().Add(integrationTimeout))
	got := make([]byte, 5)
	if _, err := io.ReadFull(secondRemote, got); err != nil || string(got) != "again" {
		t.Fatalf("reconnected write: data=%q err=%v", got, err)
	}
}

func TestEndpointReadTimeoutAndContextCancellation(t *testing.T) {
	server := integrationEndpoint(t, Config{
		Mode: ModeServer, Network: "tcp", Address: "127.0.0.1:0", ReadTimeout: 80 * time.Millisecond,
	})
	remote := integrationDial(t, server.Addr())
	ctx, cancel := integrationContext(t)
	defer cancel()
	conn, err := server.Accept(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Read(make([]byte, 1)); !integrationIsTimeout(err) {
		t.Fatalf("Read timeout: got %v", err)
	}
	if _, err := remote.Write([]byte("a")); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 1)
	if err := integrationReadFull(ctx, conn, got); err != nil || got[0] != 'a' {
		t.Fatalf("read after timeout: data=%q err=%v", got, err)
	}
	canceled, stop := context.WithCancel(ctx)
	stop()
	if _, err := conn.ReadContext(canceled, got); err == nil {
		t.Fatal("ReadContext with canceled context succeeded")
	}
	if _, err := remote.Write([]byte("b")); err != nil {
		t.Fatal(err)
	}
	if err := integrationReadFull(ctx, conn, got); err != nil || got[0] != 'b' {
		t.Fatalf("read after canceled ReadContext: data=%q err=%v", got, err)
	}
}

func integrationIsTimeout(err error) bool {
	var netErr net.Error
	return errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout())
}

func TestEndpointFailedResetCanBeRetried(t *testing.T) {
	e := integrationEndpoint(t, Config{Mode: ModeServer, Network: "tcp", Address: "127.0.0.1:0"})
	ctx, cancel := integrationContext(t)
	defer cancel()
	bad := Config{Mode: ModeClient, Network: "tcp", Address: "127.0.0.1:0", DialTimeout: 100 * time.Millisecond}
	if err := e.Reset(ctx, &bad); err == nil {
		t.Fatal("Reset to an unusable client address succeeded")
	}
	good := Config{Mode: ModeServer, Network: "tcp", Address: "127.0.0.1:0"}
	if err := e.Reset(ctx, &good); err != nil {
		t.Fatalf("Reset after failed restart: %v", err)
	}
	remote := integrationDial(t, e.Addr())
	if _, err := e.Accept(ctx); err != nil {
		t.Fatalf("Accept after recovery: %v", err)
	}
	_ = remote.Close()
}

func TestEndpointConcurrentResetAndClose(t *testing.T) {
	e := integrationEndpoint(t, Config{Mode: ModeServer, Network: "tcp", Address: "127.0.0.1:0"})
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			ctx, cancel := context.WithTimeout(context.Background(), integrationTimeout)
			defer cancel()
			_ = e.Reset(ctx, nil)
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		for range 100 {
			_ = e.Config()
			_ = e.Addr()
		}
		_ = e.Close()
	}()
	close(start)
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(integrationTimeout):
		t.Fatal("concurrent Reset and Close did not finish")
	}
	ctx, cancel := integrationContext(t)
	defer cancel()
	if err := e.Reset(ctx, nil); err == nil {
		t.Fatal("Reset succeeded after final Close")
	}
}

type integrationObservedReadConn struct {
	net.Conn
	entered chan struct{}
	once    sync.Once
}

func (c *integrationObservedReadConn) Read(p []byte) (int, error) {
	c.once.Do(func() { close(c.entered) })
	return c.Conn.Read(p)
}

type integrationObservedWriteConn struct {
	net.Conn
	entered chan struct{}
	once    sync.Once
}

func (c *integrationObservedWriteConn) Write(p []byte) (int, error) {
	c.once.Do(func() { close(c.entered) })
	return c.Conn.Write(p)
}

type integrationPartialWriter struct {
	net.Conn
	limit int
	data  bytes.Buffer
	calls int
}

type integrationBlockingCloseConn struct {
	net.Conn
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (c *integrationBlockingCloseConn) Close() error {
	c.once.Do(func() {
		close(c.entered)
		<-c.release
	})
	return c.Conn.Close()
}

func (c *integrationPartialWriter) Write(p []byte) (int, error) {
	c.calls++
	if len(p) > c.limit {
		p = p[:c.limit]
	}
	return c.data.Write(p)
}

func TestConnWriteRetriesPartialWrites(t *testing.T) {
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	partial := &integrationPartialWriter{Conn: left, limit: 2}
	c := newConn(partial, nil, Config{})
	data := []byte("partial write")
	n, err := c.Write(data)
	if err != nil || n != len(data) {
		t.Fatalf("Write returned n=%d err=%v", n, err)
	}
	if partial.calls < 2 || !bytes.Equal(partial.data.Bytes(), data) {
		t.Fatalf("Write made %d calls and sent %q; want %q", partial.calls, partial.data.Bytes(), data)
	}

	zero := &integrationPartialWriter{Conn: left, limit: 0}
	c = newConn(zero, nil, Config{})
	if n, err := c.Write(data); n != 0 || !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("zero-progress Write returned n=%d err=%v", n, err)
	}
}

func TestEndpointResetInterruptsBlockedWrite(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if tcpListener, ok := listener.(*net.TCPListener); ok {
		_ = tcpListener.SetDeadline(time.Now().Add(integrationTimeout))
	}
	cfg := Config{Mode: ModeClient, Network: "tcp", Address: listener.Addr().String()}
	e, err := NewEndpoint(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	left, right := net.Pipe()
	defer right.Close()
	observed := &integrationObservedWriteConn{Conn: left, entered: make(chan struct{})}
	s := &session{cfg: cfg, mode: ModeClient, conns: make(map[*Conn]struct{})}
	old := newConn(observed, s, cfg)
	s.client = old
	s.conns[old] = struct{}{}
	e.mu.Lock()
	e.session = s
	e.mu.Unlock()

	writeResult := make(chan error, 1)
	go func() {
		_, err := old.WriteContext(context.Background(), []byte("blocked"))
		writeResult <- err
	}()
	select {
	case <-observed.entered:
	case <-time.After(integrationTimeout):
		t.Fatal("Write did not enter net.Pipe")
	}
	ctx, cancel := integrationContext(t)
	defer cancel()
	if err := e.Reset(ctx, nil); err != nil {
		t.Fatalf("Reset during blocked Write: %v", err)
	}
	integrationReceiveError(t, "blocked WriteContext", writeResult)
	newRemote, err := listener.Accept()
	if err != nil {
		t.Fatalf("reconnect after blocked Write: %v", err)
	}
	defer newRemote.Close()
	if _, err := old.Write([]byte("stale")); err == nil {
		t.Fatal("old connection accepted a write after reset")
	}
}

func TestEndpointConcurrentCloseWaitsForShutdown(t *testing.T) {
	cfg := Config{Mode: ModeClient, Network: "tcp", Address: "127.0.0.1:1"}
	e, err := NewEndpoint(cfg)
	if err != nil {
		t.Fatal(err)
	}
	left, right := net.Pipe()
	defer right.Close()
	blocked := &integrationBlockingCloseConn{Conn: left, entered: make(chan struct{}), release: make(chan struct{})}
	var unblock sync.Once
	defer unblock.Do(func() { close(blocked.release) })
	s := &session{cfg: cfg, mode: ModeClient, conns: make(map[*Conn]struct{})}
	c := newConn(blocked, s, cfg)
	s.client = c
	s.conns[c] = struct{}{}
	e.mu.Lock()
	e.session = s
	e.mu.Unlock()

	firstResult := make(chan error, 1)
	go func() { firstResult <- e.Close() }()
	select {
	case <-blocked.entered:
	case <-time.After(integrationTimeout):
		t.Fatal("first Close did not reach connection shutdown")
	}
	secondStarted := make(chan struct{})
	secondResult := make(chan error, 1)
	go func() {
		close(secondStarted)
		secondResult <- e.Close()
	}()
	<-secondStarted
	select {
	case err := <-secondResult:
		unblock.Do(func() { close(blocked.release) })
		t.Fatalf("second Close returned before shutdown completed: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	unblock.Do(func() { close(blocked.release) })
	select {
	case err := <-firstResult:
		if err != nil {
			t.Fatalf("first Close: %v", err)
		}
	case <-time.After(integrationTimeout):
		t.Fatal("first Close did not finish")
	}
	select {
	case err := <-secondResult:
		if err != nil {
			t.Fatalf("second Close: %v", err)
		}
	case <-time.After(integrationTimeout):
		t.Fatal("second Close did not finish")
	}
}

func TestConnReadContextCancellationKeepsConnectionUsable(t *testing.T) {
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	observed := &integrationObservedReadConn{Conn: left, entered: make(chan struct{})}
	c := newConn(observed, nil, Config{})
	ctx, cancel := context.WithCancel(context.Background())
	readResult := make(chan error, 1)
	go func() {
		_, err := c.ReadContext(ctx, make([]byte, 1))
		readResult <- err
	}()
	select {
	case <-observed.entered:
	case <-time.After(integrationTimeout):
		t.Fatal("Read did not enter net.Pipe")
	}
	cancel()
	select {
	case err := <-readResult:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled ReadContext returned %v", err)
		}
	case <-time.After(integrationTimeout):
		t.Fatal("canceled ReadContext remained blocked")
	}
	writeResult := make(chan error, 1)
	go func() {
		_, err := right.Write([]byte("x"))
		writeResult <- err
	}()
	ctx2, cancel2 := integrationContext(t)
	defer cancel2()
	got := make([]byte, 1)
	if err := integrationReadFull(ctx2, c, got); err != nil || got[0] != 'x' {
		t.Fatalf("read after context cancellation: data=%q err=%v", got, err)
	}
	if err := <-writeResult; err != nil {
		t.Fatal(err)
	}
}

func TestConnQueuedReadContextCancellation(t *testing.T) {
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	observed := &integrationObservedReadConn{Conn: left, entered: make(chan struct{})}
	c := newConn(observed, nil, Config{})
	firstResult := make(chan error, 1)
	go func() {
		_, err := c.ReadContext(context.Background(), make([]byte, 1))
		firstResult <- err
	}()
	select {
	case <-observed.entered:
	case <-time.After(integrationTimeout):
		t.Fatal("first Read did not enter net.Pipe")
	}
	queuedCtx, cancel := context.WithCancel(context.Background())
	secondResult := make(chan error, 1)
	go func() {
		_, err := c.ReadContext(queuedCtx, make([]byte, 1))
		secondResult <- err
	}()
	cancel()
	select {
	case err := <-secondResult:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("queued ReadContext returned %v", err)
		}
	case <-time.After(integrationTimeout):
		t.Fatal("queued ReadContext ignored context cancellation")
	}
	_ = left.Close()
	select {
	case <-firstResult:
	case <-time.After(integrationTimeout):
		t.Fatal("first Read did not finish after closing pipe")
	}
}

func TestConnWriteTimeoutDoesNotPoisonNextWrite(t *testing.T) {
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	c := newConn(left, nil, Config{WriteTimeout: 80 * time.Millisecond})
	if n, err := c.Write([]byte("first")); n != 0 || !integrationIsTimeout(err) {
		t.Fatalf("Write timeout returned n=%d err=%v", n, err)
	}
	readResult := make(chan error, 1)
	go func() {
		got := make([]byte, 6)
		_, err := io.ReadFull(right, got)
		if err == nil && string(got) != "second" {
			err = fmt.Errorf("received %q, want second", got)
		}
		readResult <- err
	}()
	if n, err := c.Write([]byte("second")); n != 6 || err != nil {
		t.Fatalf("Write after timeout returned n=%d err=%v", n, err)
	}
	select {
	case err := <-readResult:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(integrationTimeout):
		t.Fatal("reader did not receive write after timeout")
	}
}

func TestConnWriteContextCancellationKeepsConnectionUsable(t *testing.T) {
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	observed := &integrationObservedWriteConn{Conn: left, entered: make(chan struct{})}
	c := newConn(observed, nil, Config{})
	ctx, cancel := context.WithCancel(context.Background())
	writeResult := make(chan error, 1)
	go func() {
		_, err := c.WriteContext(ctx, []byte("blocked"))
		writeResult <- err
	}()
	select {
	case <-observed.entered:
	case <-time.After(integrationTimeout):
		t.Fatal("Write did not enter net.Pipe")
	}
	cancel()
	select {
	case err := <-writeResult:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled WriteContext returned %v", err)
		}
	case <-time.After(integrationTimeout):
		t.Fatal("canceled WriteContext remained blocked")
	}
	readResult := make(chan error, 1)
	go func() {
		got := make([]byte, 2)
		_, err := io.ReadFull(right, got)
		if err == nil && string(got) != "ok" {
			err = fmt.Errorf("received %q, want ok", got)
		}
		readResult <- err
	}()
	ctx2, cancel2 := integrationContext(t)
	defer cancel2()
	if err := integrationWrite(ctx2, c, []byte("ok")); err != nil {
		t.Fatalf("WriteContext after cancellation: %v", err)
	}
	select {
	case err := <-readResult:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(integrationTimeout):
		t.Fatal("reader did not receive write after cancellation")
	}
}

func TestEndpointAcceptCancellationKeepsListenerUsable(t *testing.T) {
	e := integrationEndpoint(t, Config{Mode: ModeServer, Network: "tcp", Address: "127.0.0.1:0"})
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		close(started)
		_, err := e.Accept(ctx)
		result <- err
	}()
	<-started
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled Accept returned %v", err)
		}
	case <-time.After(integrationTimeout):
		t.Fatal("canceled Accept remained blocked")
	}
	remote := integrationDial(t, e.Addr())
	ctx2, cancel2 := integrationContext(t)
	defer cancel2()
	if _, err := e.Accept(ctx2); err != nil {
		t.Fatalf("Accept after cancellation: %v", err)
	}
	_ = remote.Close()
}

func TestEndpointQueuedAcceptContextCancellation(t *testing.T) {
	e := integrationEndpoint(t, Config{Mode: ModeServer, Network: "tcp", Address: "127.0.0.1:0"})
	e.mu.Lock()
	s := e.session
	e.mu.Unlock()
	firstCtx, cancelFirst := context.WithCancel(context.Background())
	defer cancelFirst()
	firstResult := make(chan error, 1)
	go func() {
		_, err := e.Accept(firstCtx)
		firstResult <- err
	}()
	// The first Accept holds the serialization gate until it returns. Observe
	// that state instead of relying on a delay or scheduler timing.
	deadline := time.NewTimer(integrationTimeout)
	defer deadline.Stop()
	for len(s.acceptGate) != 0 {
		select {
		case <-deadline.C:
			t.Fatal("first Accept never acquired its gate")
		default:
			runtime.Gosched()
		}
	}
	queuedCtx, cancelQueued := context.WithCancel(context.Background())
	secondResult := make(chan error, 1)
	go func() {
		_, err := e.Accept(queuedCtx)
		secondResult <- err
	}()
	cancelQueued()
	select {
	case err := <-secondResult:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("queued Accept returned %v", err)
		}
	case <-time.After(integrationTimeout):
		t.Fatal("queued Accept ignored context cancellation")
	}
	cancelFirst()
	select {
	case err := <-firstResult:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("first canceled Accept returned %v", err)
		}
	case <-time.After(integrationTimeout):
		t.Fatal("first canceled Accept remained blocked")
	}
	remote := integrationDial(t, e.Addr())
	ctx, cancel := integrationContext(t)
	defer cancel()
	if _, err := e.Accept(ctx); err != nil {
		t.Fatalf("Accept after queued cancellation: %v", err)
	}
	_ = remote.Close()
}

func TestConfigValidationAndRejectedReset(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  Config
	}{
		{"missing mode", Config{Address: "127.0.0.1:0"}},
		{"invalid mode", Config{Mode: Mode("other"), Address: "127.0.0.1:0"}},
		{"invalid network", Config{Mode: ModeServer, Network: "udp", Address: "127.0.0.1:0"}},
		{"missing address", Config{Mode: ModeServer, Network: "tcp"}},
		{"server bind in LocalAddress", Config{Mode: ModeServer, Address: "127.0.0.1:0", LocalAddress: "127.0.0.1:0"}},
		{"LocalAddress hostname", Config{Mode: ModeClient, Address: "127.0.0.1:1", LocalAddress: "localhost:0"}},
		{"LocalAddress service port", Config{Mode: ModeClient, Address: "127.0.0.1:1", LocalAddress: "127.0.0.1:http"}},
		{"negative dial timeout", Config{Mode: ModeClient, Address: "127.0.0.1:1", DialTimeout: -time.Second}},
		{"negative read timeout", Config{Mode: ModeServer, Address: "127.0.0.1:0", ReadTimeout: -time.Second}},
		{"negative write timeout", Config{Mode: ModeServer, Address: "127.0.0.1:0", WriteTimeout: -time.Second}},
		{"negative keepalive period", Config{Mode: ModeServer, Address: "127.0.0.1:0", KeepAlivePeriod: -time.Second}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewEndpoint(tc.cfg); err == nil {
				t.Fatalf("NewEndpoint accepted %+v", tc.cfg)
			}
		})
	}
	if _, err := NewEndpoint(Config{Mode: ModeClient, Address: "127.0.0.1:1", LocalAddress: "127.0.0.1:0"}); err != nil {
		t.Fatalf("numeric LocalAddress rejected: %v", err)
	}
	e := integrationEndpoint(t, Config{Mode: ModeServer, Address: "127.0.0.1:0"})
	if got := e.Config(); got.Network != "tcp" {
		t.Fatalf("default network: got %q", got.Network)
	}
	before := e.Config()
	bad := Config{Mode: Mode("other"), Address: "127.0.0.1:0"}
	ctx, cancel := integrationContext(t)
	defer cancel()
	if err := e.Reset(ctx, &bad); err == nil {
		t.Fatal("Reset accepted invalid configuration")
	}
	if got := e.Config(); got != before {
		t.Fatalf("invalid Reset changed config: before=%+v after=%+v", before, got)
	}
	remote := integrationDial(t, e.Addr())
	if _, err := e.Accept(ctx); err != nil {
		t.Fatalf("invalid Reset stopped healthy listener: %v", err)
	}
	_ = remote.Close()
}

func TestEndpointCanceledStartAndResetLeaveStoppedState(t *testing.T) {
	e, err := NewEndpoint(Config{Mode: ModeServer, Address: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := e.Start(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Start with canceled context returned %v", err)
	}
	if e.Addr() != nil {
		t.Fatal("canceled Start left an active listener")
	}
	if err := e.Start(context.Background()); err != nil {
		t.Fatalf("Start after canceled Start: %v", err)
	}
	if err := e.Reset(ctx, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("Reset with canceled context returned %v", err)
	}
	if e.Addr() != nil {
		t.Fatal("canceled Reset left an active listener")
	}
	if err := e.Start(context.Background()); err != nil {
		t.Fatalf("Start after canceled Reset: %v", err)
	}
}
