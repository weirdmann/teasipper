# Teasipper

Teasipper is a small TCP client and server library for Go. It exposes synchronous byte streams with explicit configuration, cancellation, and reset. This is an educational project; evaluate it for your own requirements before using it in production.

## Install

```sh
go get github.com/weirdmann/teasipper
```

## Example

This program starts a server on an operating-system-selected port, connects a client, and transfers four bytes:

```go
package main

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/weirdmann/teasipper"
)

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	server, err := teasipper.NewEndpoint(teasipper.Config{
		Mode:         teasipper.ModeServer,
		Address:      "127.0.0.1:0",
		ReadTimeout:  time.Second,
		WriteTimeout: time.Second,
	})
	if err != nil { panic(err) }
	defer server.Close()
	if err := server.Start(ctx); err != nil { panic(err) }

	client, err := teasipper.NewEndpoint(teasipper.Config{
		Mode:        teasipper.ModeClient,
		Address:     server.Addr().String(),
		DialTimeout: time.Second,
	})
	if err != nil { panic(err) }
	defer client.Close()
	if err := client.Start(ctx); err != nil { panic(err) }

	incoming, err := server.Accept(ctx)
	if err != nil { panic(err) }
	defer incoming.Close()

	outgoing, err := client.Conn()
	if err != nil { panic(err) }
	if _, err := outgoing.WriteContext(ctx, []byte("ping")); err != nil {
		panic(err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(incoming, buf); err != nil { panic(err) }
	fmt.Println(string(buf)) // ping
}
```

`Accept` belongs to server endpoints. `Conn()` returns the active connection of a client endpoint after a successful `Start` or `Reset`. Both methods return an error if the endpoint is stopped or used in the wrong mode. Close accepted connections when finished; endpoint reset or close also closes all managed connections.

## Configuration

Pass a `Config` to `NewEndpoint`. `Config()` returns a value copy; changes to that copy take effect only when passed to `Reset`.

| Field | Meaning |
| --- | --- |
| `Mode` | `ModeClient` dials; `ModeServer` listens. |
| `Network` | `tcp` (default), `tcp4`, or `tcp6`. |
| `Address` | Client remote address or server listen address, including port. Use `net.JoinHostPort` when assembling an address. |
| `LocalAddress` | Optional client local bind address with a numeric IP (or empty host) and numeric port, for example `127.0.0.1:0` or `:0`. Hostnames are rejected so setup cancellation never waits on DNS for the local address. It is invalid in server mode. |
| `DialTimeout` | Limit for client dialing; zero adds no timeout. The startup context may impose a shorter limit. |
| `ReadTimeout` | Limit for the active I/O part of each read, starting after its turn for reading begins; zero disables the configured limit. |
| `WriteTimeout` | Limit for the active I/O part of each write, starting after its turn for writing begins; zero disables the configured limit. |
| `NoDelay` | If true, explicitly enable `TCP_NODELAY` on dialed and accepted TCP connections. False leaves Go's TCP default unchanged (currently enabled); it does not enable Nagle's algorithm. |
| `KeepAlivePeriod` | If positive, enable TCP keepalive with this period on dialed and accepted connections. Zero leaves Go's default unchanged. |

`NewEndpoint` rejects an invalid mode or network, an empty address, a negative timeout or keepalive period, and `LocalAddress` in server mode. All fields are fixed during an active session. Change mode, network, addresses, timeouts, or TCP options with `Reset`. TCP option setup errors fail client startup or the individual server `Accept`; a rejected accepted connection is closed. The contexts passed to `Start` and `Reset` limit setup only; canceling them after a successful return does not end the session. `Addr()` reports the active local address, including the actual port for a server bound to `:0`; it returns `nil` while stopped.

## I/O and ownership

`Conn` implements `net.Conn` and also provides `ReadContext` and `WriteContext`. The caller owns each buffer. A read may fill only part of the supplied slice; synchronous writes use the supplied slice until they return. Always inspect both the byte count and error. One read and one write can run concurrently; concurrent operations in the same direction are serialized.

`SetDeadline`, `SetReadDeadline`, and `SetWriteDeadline` set absolute deadlines that persist across calls until changed or cleared with a zero `time.Time`. This lets a parser impose one deadline on a whole frame spanning multiple `Read` calls. When combined with a configured per-call timeout or a context deadline, the earliest deadline wins for that call. The configured timeout is measured from the start of each active call, while a manually set deadline stays absolute across calls. Canceling a `ReadContext` or `WriteContext` call temporarily interrupts that operation; afterward the manual deadline remains in force. Use a zero configured timeout when the application exclusively manages frame deadlines itself.

`ReadContext` and `WriteContext` interrupt a blocked operation when their context is canceled or reaches its deadline, without closing the connection. Their contexts cover the entire call, including time spent waiting behind another operation in the same direction. Configured read and write timeouts start only after that wait, when the operation obtains its turn to use the connection; they do not limit time in the queue. During active I/O, the earliest applicable context deadline or configured timeout wins. `Reset` and `Close` close old connections and interrupt their blocked I/O.

TCP is a byte stream: one `Write` may be split across reads, and multiple writes may be combined. Use a length prefix, delimiter, or a fixed length with `io.ReadFull` when your application needs message boundaries. The server does not broadcast automatically; applications choose which accepted connections to write to.

Teasipper does not reconnect automatically after the remote side closes or restarts. An I/O operation reports EOF or another connection error; call `Reset(ctx, nil)` to establish a new client connection. Until reset or close, `Conn()` still returns the old client connection. After a successful reset, fetch `Conn()` again to obtain the replacement connection.

## Reset and close

`Reset(ctx, nil)` restarts with the current configuration. Supply a new `Config` to change mode or connection parameters:

```go
next := endpoint.Config()
next.Mode = teasipper.ModeClient
next.Address = "127.0.0.1:9000"
if err := endpoint.Reset(ctx, &next); err != nil {
	return err
}
```

Reset closes the old listener and managed connections, interrupts their blocked operations, and starts a new session. It does not retain or replay pending data. An old `Conn` remains closed and cannot become a connection to the replacement session. An operation that completes concurrently with reset may still return bytes read before closure, and bytes written before closure may have reached the peer.

A server reset succeeds when the new listener has bound. A client reset succeeds when the new TCP connection has been established, so total reset time depends on the network and on the dial timeout or context. If restart fails, the endpoint is stopped with the selected configuration and can be started or reset again. An invalid replacement configuration is rejected before the old session is stopped.

Concurrent `Start` and `Reset` calls are serialized; a new `Reset` cancels connection setup already in progress. `Close` interrupts startup or reset, closes the listener and managed connections, and is final. It is safe to call repeatedly. A closed endpoint cannot start again. Blocked `Accept` on the old session is interrupted by reset or close. Canceling an individual `Accept(ctx)` interrupts that call without closing the listener.

## Migration from the channel API

| Old API | New API |
| --- | --- |
| `NewEndpoint(logger)` | `NewEndpoint(Config{...})`. |
| `Listen(ctx, addr, &sendChan)` | Configure `ModeServer` and `Address`, then `Start(ctx)` and `Accept(ctx)`. |
| `Dial(ctx, addr, &sendChan)` | Configure `ModeClient` and `Address`, then `Start(ctx)` and `Conn()`. |
| Send and receive through `*chan []byte` | Synchronous `Conn.Write`/`WriteContext` and `Conn.Read`/`ReadContext` with caller-owned buffers. |
| Close the send channel to stop | `Reset` to reconnect or rebind; `Close` to dispose of the endpoint. |
| `Peer`, `UpdateTimeout` | `Conn`, operation contexts, and `Config` timeouts. |

The former server API broadcast outbound data to every peer. Keep track of accepted connections and write to each selected peer explicitly. The library adds neither framing nor an outgoing queue.
