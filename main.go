package teasipper

/*

TODO: There is some error with the Dialer locking the graceful shutdown
when it has some leftover values in the send channel but
it didn't establish the connection...
Look into it.

*/
import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"os"
	"sync"
	"time"
	"github.com/beevik/guid"
)

var wg sync.WaitGroup
/*
func main() {
	logger := log.Default()
	logger.Println("Starting...")
	main_context, cancel := context.WithCancel(context.Background())
	go gs.GracefulShutdown(logger, cancel)

	tcp := NewEndpoint("0.0.0.0", 2137, &main_context, logger)
	tcp_d := NewEndpoint("127.0.0.1", 2139, &main_context, logger)

	send_to_listener := make(chan []byte)
	send_to_dialer := make(chan []byte)
	recv_from_listener, _ := tcp.Listen(main_context, "0.0.0.0:2137", &send_to_listener)
	recv_from_dialer, _ := tcp_d.Dial(main_context, "0.0.0.0:2138", &send_to_dialer)

	handle := func(ctx context.Context, recv_chan *chan []byte, send_chan *chan []byte) {

		defer wg.Done()
		if recv_chan == nil || send_chan == nil {
			return
		}
		stop := false
		for !stop {
			select {
			case r, ok := <-*recv_chan:
				if !ok {
					log.Println("Endpoint closed the recv_chan, exiting...")
					stop = true
					break
				}
				//log.Printf("[INFO] Received: %s", r)
				str := string(r[:])
				str = strings.TrimSpace(str)
				*send_chan <- r
			case <-ctx.Done():
				//time.Sleep(time.Second)
				stop = true
				break
			}
		}
		close(*send_chan)
	}
	wg.Add(2)
	go handle(main_context, recv_from_listener, &send_to_listener)
	go handle(main_context, recv_from_dialer, &send_to_dialer)
	wg.Wait()
}
*/
// ---- Tcp Endpoint ---- //

type Endpoint struct {
	Guid      guid.Guid
	listener  net.Listener
	dialer    net.Dialer
	logger    *log.Logger
	peers     map[string]*Peer
	recv_chan chan []byte

	// the endpoint creates the send channel and returns it to the caller
	// the caller then sends to this channel the data to be fanned-out to the
	// connected clients
	// when the channel is closed by the caller, the listener closes
	// all the connections, stops accepting new ones and then closes the recv channel
	send_chan *chan []byte
}

func NewEndpoint(ipAddr string, port uint16, ctx *context.Context, logger *log.Logger) *Endpoint {
	return &Endpoint{
		Guid:   *guid.New(),
		logger: logger,
		peers:  make(map[string]*Peer),
	}
}

func (this *Endpoint) log(s string, a ...any) {
	a = append([]any{this.Guid.String()[:4]}, a)
	this.logger.Printf("%s | "+s, a...)
}

func (this *Endpoint) ListenerStop(ctx context.Context) {
	select {
	case <-(ctx).Done():
		this.log("Stopping TCP Listener %s", this.listener.Addr())
		for _, tcpPeer := range this.peers {
			tcpPeer.Close()
		}
		this.listener.Close()
	}
}

func (this *Endpoint) DialerStop(ctx context.Context) {
	select {
	case <-(ctx).Done():
		this.logger.Printf("Stopping TCP Dialer: %s", this.dialer.LocalAddr)
		for _, tcpPeer := range this.peers {
			tcpPeer.Close()
		}
		//	close(*this.recv_chan)
	}
}

// do it the "GO" way:
// Listen should not be started as a goroutine
// but start its own goroutine and return a channel that
// will receive the packets
func (this *Endpoint) Listen(ctx context.Context, addr string, chan_for_data_to_send *chan []byte) (*chan []byte, error) {
	this.recv_chan = make(chan []byte, 64)
	this.send_chan = chan_for_data_to_send

	var err error

	this.listener, err = net.Listen("tcp4", addr)
	if err != nil {
		this.logger.Println(err)
		close(this.recv_chan)
		return &this.recv_chan, err
	}

	connectionContext, connectionCancel := context.WithCancel(ctx)
	go this.ListenerStop(connectionContext)
	go this.accept(connectionContext)

	// Sender coroutine - spread the sent data between all peers
	go func() {
		for to_send := range *this.send_chan {
			for _, peer := range this.peers {
				peer.send_chan <- to_send
			}
		}
		this.logger.Printf("%s x-  | Send channel closed, stopping TCP Listener", this.listener.Addr())
		// cancel the stop handler coroutine
		connectionCancel()
	}()
	return &this.recv_chan, nil
}

func (this *Endpoint) accept(ctx context.Context) {
	for {
		// Accept incoming connections
		conn, err := this.listener.Accept()
		if errors.Is(err, net.ErrClosed) {
			this.log("%s > Goroutine this.accept: Connection closed", this.listener.Addr().String())
			return
		}
		if err != nil {
			this.logger.Printf("%s > Goroutine this.accept: Error: %q", this.listener.Addr().String(), err)
			return
		}

		tcpPeer, _ := NewPeer(conn, this.logger)
		this.peers[tcpPeer.Id.String()] = tcpPeer
		// Handle client connection in a goroutine
		this.logger.Printf("%s <- %s | New TCP Peer connected", this.listener.Addr().String(), tcpPeer.Conn.RemoteAddr().String())
		tcpPeer.Receive(func() { delete(this.peers, tcpPeer.Id.String()) })

		go func(ctx context.Context, tcpPeer *Peer) {
			for {
				select {
				case received, ok := <-tcpPeer.recv_chan:
					if !ok {
						this.logger.Printf("%s <- %s | Recv_chan closed, closing the fan-in func", this.listener.Addr().String(), tcpPeer.Conn.RemoteAddr().String())
						close(tcpPeer.send_chan)
						return
					}
					this.recv_chan <- received
				case <-ctx.Done():
					this.logger.Printf("%s <- %s | Context cancelled, closing the fan-in func", this.listener.Addr().String(), tcpPeer.Conn.RemoteAddr().String())
					return
				}
			}
		}(ctx, tcpPeer)
	}
}

func (this *Endpoint) Dial(ctx context.Context, addr string, chan_for_data_to_send *chan []byte) (*chan []byte, error) {
	this.recv_chan = make(chan []byte, 64)
	this.send_chan = chan_for_data_to_send

	connectionContext, connectionCancel := context.WithCancel(ctx)
	go this.DialerStop(connectionContext)

	var conn net.Conn
	this.dialer.Deadline.Add(5 * time.Second)
	conn, err := this.dialer.DialContext(ctx, "tcp4", addr)

	if err != nil {
		this.logger.Printf(" >x %s | TCP Client connection attempt failed for: %q", addr, err)
		// cancel the stop handler coroutine so it doesn't try to close a closed recv_chan
		connectionCancel()
		close(this.recv_chan)
		return &this.recv_chan, err
	}

	this.logger.Printf("%s -> %s | TCP Dialer connected", conn.LocalAddr().String(), conn.RemoteAddr().String())

	tcpPeer, _ := NewPeer(conn, this.logger)
	this.peers[tcpPeer.Id.String()] = tcpPeer
	tcpPeer.Receive(func() { delete(this.peers, tcpPeer.Id.String()) })
	go func(ctx context.Context, tcpPeer *Peer) {
		for {
			select {
			case received, ok := <-tcpPeer.recv_chan:
				if !ok {
					this.logger.Printf(" -> %s | Recv_chan closed, closing the fan-in func", tcpPeer.Conn.RemoteAddr().String())
					close(tcpPeer.send_chan)
					return
				}
				this.recv_chan <- received
			case <-ctx.Done():
				this.logger.Printf(" -> %s | Context cancelled, closing the fan-in func", tcpPeer.Conn.RemoteAddr().String())
				return
			}
		}
	}(connectionContext, tcpPeer)
	// Sender coroutine - spread the sent data between all peers
	go func() {
		for to_send := range *this.send_chan {
			for _, peer := range this.peers {
				peer.send_chan <- to_send
			}
		}
		this.logger.Printf(" x> %s | Send channel closed, stopping TCP Dialer", addr)
		// cancel the stop handler coroutine so it doesn't try to close a closed recv_chan
		connectionCancel()
	}()
	return &this.recv_chan, nil
}

// --------------------- //

// ---- Tcp Peer ---- //
// Peer
type Peer struct {
	Id        guid.Guid
	Conn      net.Conn
	recv_chan chan []byte // TcpPeer receives data and sends it to the recv_chan
	send_chan chan []byte // TcpPeer reads the send_chan and sends the data to its partner
	logger    *log.Logger
}

func NewPeer(conn net.Conn, logger *log.Logger) (*Peer, error) {
	n := &Peer{
		Id:        *guid.New(),
		Conn:      conn,
		recv_chan: make(chan []byte, 64),
		send_chan: make(chan []byte, 64),
		logger:    logger,
	}

	return n, nil
}

func (this *Peer) Close() {
	this.Conn.Close()
	close(this.recv_chan)
}

func (this *Peer) GetConn() net.Conn {
	return this.Conn
}

func (this *Peer) Send() {
	for to_send := range this.send_chan {
		this.Conn.Write(to_send)
	}
}

func (this *Peer) Receive(on_disconnect func()) *chan []byte {
	buffer := make([]byte, 1024)
	go func() {
		for {
			// Read data from the client
			n, err := this.Conn.Read(buffer)

			if errors.Is(err, os.ErrDeadlineExceeded) {
				continue
			}
			if errors.Is(err, io.EOF) {
				this.logger.Printf("%s <x %s | Client disconnected", this.Conn.LocalAddr(), this.Conn.RemoteAddr())
				this.Close()
				on_disconnect()
				return
			}
			if errors.Is(err, net.ErrClosed) {
				on_disconnect()
				return
			}
			if err != nil {
				this.logger.Printf("%s ?? %s | [ERRR] Error: %s", this.Conn.LocalAddr(), this.Conn.RemoteAddr(), err)
				return
			}
			this.recv_chan <- buffer[:n]
		}
	}()
	go this.Send()
	return &this.recv_chan
}

// ----------------- //

// function to update the timeout of blahblah
func UpdateTimeout[C net.Conn](conn C, seconds int) {
	conn.SetDeadline(time.Now().Add(time.Second * time.Duration(seconds)))
}
