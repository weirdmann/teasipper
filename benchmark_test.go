package teasipper

import (
	"bytes"
	"context"
	"testing"
	"time"
)

// These benchmarks measure the same one-way TCP byte transfer as the
// pre-change harness. Setup, connection establishment, and cleanup are outside
// the timer. The stream may deliver a write in multiple reads.
func BenchmarkEndpointOneWay64(b *testing.B) {
	benchmarkEndpointOneWay(b, 64, 0)
}

func BenchmarkEndpointOneWay4096(b *testing.B) {
	benchmarkEndpointOneWay(b, 4096, 0)
}

func BenchmarkEndpointOneWay64Timeout1s(b *testing.B) {
	benchmarkEndpointOneWay(b, 64, time.Second)
}

func benchmarkEndpointOneWay(b *testing.B, size int, timeout time.Duration) {
	server, err := NewEndpoint(Config{
		Mode: ModeServer, Address: "127.0.0.1:0",
		ReadTimeout: timeout, WriteTimeout: timeout,
	})
	if err != nil {
		b.Fatal(err)
	}
	if err := server.Start(context.Background()); err != nil {
		b.Fatal(err)
	}
	defer server.Close()

	client, err := NewEndpoint(Config{
		Mode: ModeClient, Address: server.Addr().String(),
		ReadTimeout: timeout, WriteTimeout: timeout,
	})
	if err != nil {
		b.Fatal(err)
	}
	if err := client.Start(context.Background()); err != nil {
		b.Fatal(err)
	}
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	serverConn, err := server.Accept(ctx)
	if err != nil {
		b.Fatal(err)
	}
	clientConn, err := client.Conn()
	if err != nil {
		b.Fatal(err)
	}

	payload := make([]byte, size)
	for i := range payload {
		payload[i] = byte(i)
	}
	received := make([]byte, size)
	transfer := func() {
		n, err := clientConn.Write(payload)
		if err != nil || n != size {
			b.Fatalf("write %d bytes: n=%d err=%v", size, n, err)
		}
		for offset := 0; offset < size; {
			n, err := serverConn.Read(received[offset:])
			if err != nil || n == 0 {
				b.Fatalf("read %d bytes: offset=%d n=%d err=%v", size, offset, n, err)
			}
			offset += n
		}
	}
	transfer()
	if !bytes.Equal(received, payload) {
		b.Fatal("warm-up data mismatch")
	}
	b.ReportAllocs()
	b.SetBytes(int64(size))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		transfer()
	}
	b.StopTimer()
}

// This measures the local session teardown and loopback listener rebind. It
// does not include waiting for a remote client to reconnect.
func BenchmarkEndpointServerResetLocal(b *testing.B) {
	server, err := NewEndpoint(Config{Mode: ModeServer, Address: "127.0.0.1:0"})
	if err != nil {
		b.Fatal(err)
	}
	if err := server.Start(context.Background()); err != nil {
		b.Fatal(err)
	}
	defer server.Close()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := server.Reset(context.Background(), nil); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
}

// This measures client teardown, a fresh loopback TCP dial, server Accept,
// and the first byte received on the new connection. It reports readiness
// separately from the server's local listener reset above.
func BenchmarkEndpointClientResetReady(b *testing.B) {
	server, err := NewEndpoint(Config{Mode: ModeServer, Address: "127.0.0.1:0"})
	if err != nil {
		b.Fatal(err)
	}
	if err := server.Start(context.Background()); err != nil {
		b.Fatal(err)
	}
	defer server.Close()

	client, err := NewEndpoint(Config{Mode: ModeClient, Address: server.Addr().String()})
	if err != nil {
		b.Fatal(err)
	}
	if err := client.Start(context.Background()); err != nil {
		b.Fatal(err)
	}
	defer client.Close()

	previous, err := server.Accept(context.Background())
	if err != nil {
		b.Fatal(err)
	}
	defer previous.Close()
	payload := []byte{0x7f}
	var received [1]byte
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := client.Reset(context.Background(), nil); err != nil {
			b.Fatal(err)
		}
		accepted, err := server.Accept(context.Background())
		if err != nil {
			b.Fatal(err)
		}
		conn, err := client.Conn()
		if err != nil {
			b.Fatal(err)
		}
		if n, err := conn.Write(payload); err != nil || n != len(payload) {
			b.Fatalf("write first byte: n=%d err=%v", n, err)
		}
		if n, err := accepted.Read(received[:]); err != nil || n != 1 || received[0] != payload[0] {
			b.Fatalf("read first byte: n=%d err=%v data=%x", n, err, received[0])
		}
		_ = previous.Close()
		previous = accepted
	}
	b.StopTimer()
}
