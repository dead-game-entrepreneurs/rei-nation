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
	return ServeWithOptions(ctx, ln, handler, ServeOptions{Heartbeat: tag})
}

// ServeOptions sets per-protocol connection behavior.
type ServeOptions struct {
	// Heartbeat is the command sent every five seconds. Empty sends none, for
	// clients that run their own keepalive transaction.
	Heartbeat string
	// IdleTimeout closes a client that sends nothing for this long. Zero uses
	// the 45-second Aries default.
	IdleTimeout time.Duration
}

func ServeWithOptions(ctx context.Context, ln net.Listener, handler Handler, opts ServeOptions) error {
	const maxClients = 128
	var heartbeat *Frame
	if opts.Heartbeat != "" {
		f := Reply(opts.Heartbeat, nil)
		heartbeat = &f
	}
	idle := opts.IdleTimeout
	if idle == 0 {
		idle = 45 * time.Second
	}
	// A negative idle means no read deadline: the client may stay silent (for
	// example while loading a level) and TCP keepalive detects a dead peer.
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
				serveClient(ctx, conn, handler, heartbeat, idle)
			}()
		default:
			conn.Close()
		}
	}
}

func serveClient(ctx context.Context, conn net.Conn, h Handler, heartbeat *Frame, idle time.Duration) {
	// Detect peers that vanish without a FIN, since a long idle window would
	// otherwise leave the connection open. The underlying socket is a TCP conn
	// for both plaintext and SSL-wrapped listeners.
	if ka, ok := conn.(interface {
		SetKeepAlive(bool) error
		SetKeepAlivePeriod(time.Duration) error
	}); ok {
		ka.SetKeepAlive(true)
		ka.SetKeepAlivePeriod(30 * time.Second)
	}
	c := &Client{conn: conn, out: make(chan outgoing, 256), done: make(chan struct{}), id: nextClientID.Add(1)}
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(ctx, func() { c.CloseWithReason("server shutdown") })
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		defer c.Close()
		var pings <-chan time.Time
		if heartbeat != nil {
			ping := time.NewTicker(5 * time.Second)
			defer ping.Stop()
			pings = ping.C
		}
		for {
			var entry outgoing
			select {
			case <-c.done:
				return
			case entry = <-c.out:
			case <-pings:
				entry.frame = *heartbeat
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
		deadline := time.Time{}
		if idle > 0 {
			deadline = time.Now().Add(idle)
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
