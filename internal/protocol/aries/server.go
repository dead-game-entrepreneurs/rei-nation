package aries

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"
)

type Handler interface {
	Handle(context.Context, *Client, Frame)
	Disconnected(*Client)
}

type Client struct {
	conn net.Conn
	out  chan outgoing
	done chan struct{}
	once sync.Once
	id   uint64
	// Written before done closes and read only after observing its closure.
	closeReason string
}

var nextClientID atomic.Uint64

type outgoing struct {
	frame   Frame
	written func()
}

func (c *Client) Addr() netip.Addr {
	return c.conn.RemoteAddr().(*net.TCPAddr).AddrPort().Addr().Unmap()
}

func (c *Client) ID() uint64 { return c.id }

// Send queues in order without letting a slow client hold up a title runtime.
func (c *Client) Send(f Frame) {
	c.SendAfter(f, nil)
}

// SendAfter runs written after the entire frame is written to the connection.
func (c *Client) SendAfter(f Frame, written func()) {
	select {
	case <-c.done:
	case c.out <- outgoing{f, written}:
	default:
		c.CloseWithReason("send queue full")
	}
}

func (c *Client) Close() {
	c.CloseWithReason("closed by server")
}

func (c *Client) CloseWithReason(reason string) {
	c.once.Do(func() {
		c.closeReason = reason
		close(c.done)
		c.conn.Close()
	})
}

func (c *Client) CloseReason() string {
	if c.Closed() {
		return c.closeReason
	}
	return ""
}

func (c *Client) Closed() bool {
	select {
	case <-c.done:
		return true
	default:
		return false
	}
}

func Serve(ctx context.Context, ln net.Listener, handler Handler) error {
	return ServeWithHeartbeat(ctx, ln, handler, "~png")
}

// ServeWithHeartbeat uses the keepalive command expected by the connected clients.
func ServeWithHeartbeat(ctx context.Context, ln net.Listener, handler Handler, tag string) error {
	return ServeTransport(ctx, ln, handler, Transport{
		Heartbeat: Reply(tag, nil), HeartbeatInterval: 5 * time.Second, ReadTimeout: 45 * time.Second,
	})
}

type Transport struct {
	Upgrade           func(net.Conn) (net.Conn, error)
	Heartbeat         Frame
	HeartbeatInterval time.Duration
	ReadTimeout       time.Duration
}

func ServeTransport(ctx context.Context, ln net.Listener, handler Handler, transport Transport) error {
	const maxClients = 128
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(ctx, func() { ln.Close() })
	var clients sync.WaitGroup
	defer func() {
		cancel()
		stop()
		ln.Close()
		clients.Wait()
	}()
	slots := make(chan struct{}, maxClients)
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		select {
		case slots <- struct{}{}:
			clients.Add(1)
			go func() {
				defer clients.Done()
				defer func() { <-slots }()
				if transport.Upgrade != nil {
					stop := context.AfterFunc(ctx, func() { conn.Close() })
					upgraded, err := transport.Upgrade(conn)
					stop()
					if err != nil {
						conn.Close()
						return
					}
					serveClient(ctx, upgraded, handler, transport)
					return
				}
				serveClient(ctx, conn, handler, transport)
			}()
		default:
			conn.Close()
		}
	}
}

func serveClient(ctx context.Context, conn net.Conn, h Handler, transport Transport) {
	c := &Client{conn: conn, out: make(chan outgoing, 256), done: make(chan struct{}), id: nextClientID.Add(1)}
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(ctx, func() { c.CloseWithReason("server shutdown") })
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		defer c.Close()
		var heartbeat <-chan time.Time
		if transport.HeartbeatInterval > 0 {
			ping := time.NewTicker(transport.HeartbeatInterval)
			defer ping.Stop()
			heartbeat = ping.C
		}
		for {
			var entry outgoing
			select {
			case <-c.done:
				return
			case entry = <-c.out:
			case <-heartbeat:
				entry.frame = transport.Heartbeat
			}
			if conn.SetWriteDeadline(time.Now().Add(5*time.Second)) != nil || entry.frame.Write(conn) != nil {
				c.CloseWithReason("socket write failed")
				cancel()
				return
			}
			if entry.written != nil {
				entry.written()
			}
		}
	}()
	defer func() {
		c.Close()
		cancel()
		stop()
		h.Disconnected(c)
		<-writerDone
	}()
	for {
		var deadline time.Time
		if transport.ReadTimeout > 0 {
			deadline = time.Now().Add(transport.ReadTimeout)
		}
		if conn.SetReadDeadline(deadline) != nil {
			c.CloseWithReason("could not set read deadline")
			return
		}
		frame, err := Read(conn)
		if err != nil {
			c.CloseWithReason(readCloseReason(err))
			return
		}
		request, cancel := context.WithTimeout(ctx, 5*time.Second)
		h.Handle(request, c, frame)
		cancel()
	}
}

func readCloseReason(err error) string {
	var socketErr net.Error
	switch {
	case errors.Is(err, io.EOF):
		return "peer closed socket"
	case errors.Is(err, io.ErrUnexpectedEOF):
		return "peer closed during frame"
	case errors.As(err, &socketErr):
		if socketErr.Timeout() {
			return "socket read timed out"
		}
		return "socket read failed"
	default:
		return "invalid Aries frame"
	}
}
