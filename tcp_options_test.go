//go:build windows || linux

package teasipper

import (
	"net"
	"syscall"
	"testing"
	"time"
)

func TestEndpointTCPOptionsOnDialAndAccept(t *testing.T) {
	cfg := Config{NoDelay: true, KeepAlivePeriod: 30 * time.Second}
	server := integrationEndpoint(t, Config{
		Mode: ModeServer, Address: "127.0.0.1:0",
		NoDelay: cfg.NoDelay, KeepAlivePeriod: cfg.KeepAlivePeriod,
	})
	client := integrationEndpoint(t, Config{
		Mode: ModeClient, Address: server.Addr().String(),
		NoDelay: cfg.NoDelay, KeepAlivePeriod: cfg.KeepAlivePeriod,
	})
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
	for name, conn := range map[string]*Conn{"dialed": clientConn, "accepted": serverConn} {
		if got, err := tcpSocketOption(conn, syscall.IPPROTO_TCP, syscall.TCP_NODELAY); err != nil || got != 1 {
			t.Fatalf("%s TCP_NODELAY = %d, %v; want 1", name, got, err)
		}
		if got, err := tcpSocketOption(conn, syscall.SOL_SOCKET, syscall.SO_KEEPALIVE); err != nil || got != 1 {
			t.Fatalf("%s SO_KEEPALIVE = %d, %v; want 1", name, got, err)
		}
	}

	// Explicit NoDelay must be applied rather than merely relying on Go's
	// current TCP default. The zero value must not change an existing setting.
	raw := serverConn.raw
	if err := raw.(*net.TCPConn).SetNoDelay(false); err != nil {
		t.Fatal(err)
	}
	if err := configureTCP(raw.(*net.TCPConn), Config{}); err != nil {
		t.Fatal(err)
	}
	if got, err := tcpSocketOption(serverConn, syscall.IPPROTO_TCP, syscall.TCP_NODELAY); err != nil || got != 0 {
		t.Fatalf("zero NoDelay changed socket option: %d, %v", got, err)
	}
	if err := configureTCP(raw.(*net.TCPConn), cfg); err != nil {
		t.Fatal(err)
	}
	if got, err := tcpSocketOption(serverConn, syscall.IPPROTO_TCP, syscall.TCP_NODELAY); err != nil || got != 1 {
		t.Fatalf("explicit NoDelay did not enable TCP_NODELAY: %d, %v", got, err)
	}
}
