package aries

import (
	"context"
	"net"
	"testing"
	"time"
)

type deadlineHandler struct {
	clear        bool
	handled      chan struct{}
	disconnected chan struct{}
}

func (h *deadlineHandler) Handle(_ context.Context, c *Client, _ Frame) {
	if h.clear {
		c.ClearInitialReadDeadline()
	}
	h.handled <- struct{}{}
}

func (h *deadlineHandler) Disconnected(*Client) { close(h.disconnected) }

func TestInitialReadDeadlineClearsAfterAuthentication(t *testing.T) {
	server, client := net.Pipe()
	t.Cleanup(func() { client.Close() })
	ctx, cancel := context.WithCancel(t.Context())
	h := &deadlineHandler{clear: true, handled: make(chan struct{}, 2), disconnected: make(chan struct{})}
	go serveClient(ctx, server, h, Transport{InitialReadTimeout: 40 * time.Millisecond})
	frame := Frame{Tag: "AUTH", Terminate: true}
	if err := frame.Write(client); err != nil {
		t.Fatal(err)
	}
	<-h.handled
	time.Sleep(80 * time.Millisecond)
	if err := client.SetWriteDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := frame.Write(client); err != nil {
		t.Fatalf("authenticated connection retained initial deadline: %v", err)
	}
	<-h.handled
	cancel()
	select {
	case <-h.disconnected:
	case <-time.After(time.Second):
		t.Fatal("connection did not stop")
	}
}

func TestInitialReadDeadlineClosesIdleClient(t *testing.T) {
	server, client := net.Pipe()
	t.Cleanup(func() { client.Close() })
	h := &deadlineHandler{handled: make(chan struct{}, 1), disconnected: make(chan struct{})}
	go serveClient(t.Context(), server, h, Transport{InitialReadTimeout: 20 * time.Millisecond})
	select {
	case <-h.disconnected:
	case <-time.After(time.Second):
		t.Fatal("idle connection outlived initial deadline")
	}
}
