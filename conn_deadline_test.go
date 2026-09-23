package teasipper

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

func TestConnManualReadDeadlineSpansReads(t *testing.T) {
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	var conn net.Conn = newConn(left, nil, Config{})
	if err := conn.SetReadDeadline(time.Now().Add(80 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	writeDone := make(chan error, 1)
	go func() { _, err := right.Write([]byte("a")); writeDone <- err }()
	var first [1]byte
	if _, err := io.ReadFull(conn, first[:]); err != nil || first[0] != 'a' {
		t.Fatalf("first read = %q, %v", first, err)
	}
	if err := <-writeDone; err != nil {
		t.Fatal(err)
	}
	var second [1]byte
	if _, err := io.ReadFull(conn, second[:]); !integrationIsTimeout(err) {
		t.Fatalf("second read should obey original frame deadline, got %v", err)
	}
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}
	go func() { _, err := right.Write([]byte("b")); writeDone <- err }()
	if _, err := io.ReadFull(conn, second[:]); err != nil || second[0] != 'b' {
		t.Fatalf("read after clearing deadline = %q, %v", second, err)
	}
	if err := <-writeDone; err != nil {
		t.Fatal(err)
	}
}

func TestConnSetReadDeadlineInterruptsBlockedRead(t *testing.T) {
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	observed := &integrationObservedReadConn{Conn: left, entered: make(chan struct{})}
	conn := newConn(observed, nil, Config{})
	result := make(chan error, 1)
	go func() { _, err := conn.Read(make([]byte, 1)); result <- err }()
	select {
	case <-observed.entered:
	case <-time.After(integrationTimeout):
		t.Fatal("read did not enter net.Pipe")
	}
	if err := conn.SetReadDeadline(time.Now()); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if !integrationIsTimeout(err) {
			t.Fatalf("blocked read returned %v, want timeout", err)
		}
	case <-time.After(integrationTimeout):
		t.Fatal("SetReadDeadline did not interrupt blocked read")
	}
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}
}

func TestConnManualDeadlineCombinedWithConfiguredTimeout(t *testing.T) {
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	conn := newConn(left, nil, Config{ReadTimeout: time.Second})
	if err := conn.SetReadDeadline(time.Now().Add(80 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Read(make([]byte, 1)); !integrationIsTimeout(err) {
		t.Fatalf("manual deadline did not override configured timeout: %v", err)
	}
	// Completion of the timed operation must restore the caller's deadline,
	// including its already-expired state, rather than clearing it.
	if _, err := conn.Read(make([]byte, 1)); !integrationIsTimeout(err) {
		t.Fatalf("manual deadline was cleared after timeout: %v", err)
	}
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}
	writeDone := make(chan error, 1)
	go func() { _, err := right.Write([]byte("x")); writeDone <- err }()
	var got [1]byte
	if _, err := conn.Read(got[:]); err != nil || got[0] != 'x' {
		t.Fatalf("read after clearing manual deadline = %q, %v", got, err)
	}
	if err := <-writeDone; err != nil {
		t.Fatal(err)
	}
}

func TestConnSetWriteDeadlineInterruptsBlockedWrite(t *testing.T) {
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	observed := &integrationObservedWriteConn{Conn: left, entered: make(chan struct{})}
	conn := newConn(observed, nil, Config{})
	result := make(chan error, 1)
	go func() { _, err := conn.Write([]byte("blocked")); result <- err }()
	select {
	case <-observed.entered:
	case <-time.After(integrationTimeout):
		t.Fatal("write did not enter net.Pipe")
	}
	if err := conn.SetWriteDeadline(time.Now()); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if !integrationIsTimeout(err) {
			t.Fatalf("blocked write returned %v, want timeout", err)
		}
	case <-time.After(integrationTimeout):
		t.Fatal("SetWriteDeadline did not interrupt blocked write")
	}
	if err := conn.SetWriteDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}
}

func TestConnSetDeadlineAppliesToBothDirections(t *testing.T) {
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	var conn net.Conn = newConn(left, nil, Config{})
	if err := conn.SetDeadline(time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Read(make([]byte, 1)); !integrationIsTimeout(err) {
		t.Fatalf("read with expired combined deadline returned %v", err)
	}
	if _, err := conn.Write([]byte("x")); !integrationIsTimeout(err) {
		t.Fatalf("write with expired combined deadline returned %v", err)
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}
}

func TestConnContextCancellationPreservesManualDeadline(t *testing.T) {
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	observed := &integrationObservedReadConn{Conn: left, entered: make(chan struct{})}
	conn := newConn(observed, nil, Config{})
	if err := conn.SetReadDeadline(time.Now().Add(150 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { _, err := conn.ReadContext(ctx, make([]byte, 1)); result <- err }()
	select {
	case <-observed.entered:
	case <-time.After(integrationTimeout):
		t.Fatal("read did not enter net.Pipe")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled read returned %v", err)
		}
	case <-time.After(integrationTimeout):
		t.Fatal("canceled read did not finish")
	}
	if _, err := conn.Read(make([]byte, 1)); !integrationIsTimeout(err) {
		t.Fatalf("manual deadline was lost after cancellation: %v", err)
	}
}
