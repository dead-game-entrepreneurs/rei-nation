package gera

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/dead-game-entrepreneurs/rei-nation/internal/game"
	"github.com/dead-game-entrepreneurs/rei-nation/internal/protocol/aries"
	"github.com/dead-game-entrepreneurs/rei-nation/internal/protocol/fesl"
	"github.com/dead-game-entrepreneurs/rei-nation/internal/protocol/ssl2/ssl2test"
)

func TestHelloPingGoodbye(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	def := Definition()
	h := def.New(game.Options{PublicIP: netip.MustParseAddr("192.0.2.10"), Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	// Exercise the FESL transaction logic over a plain listener; the SSL
	// transport is covered in package ssl2 and in TestServeOverSSL.
	go func() { done <- aries.ServeWithOptions(ctx, ln, h, aries.ServeOptions{IdleTimeout: idleTimeout}) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()

	conn, err := net.Dial("tcp4", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	send := func(code uint32, txn string, fields ...fesl.Field) {
		t.Helper()
		if err := fesl.Encode("fsys", code, txn, fields...).Write(conn); err != nil {
			t.Fatal(err)
		}
	}
	expect := func(code uint32, txn string) fesl.Message {
		t.Helper()
		f, err := aries.Read(conn)
		if err != nil {
			t.Fatal(err)
		}
		m := fesl.Decode(f)
		if m.Service != "fsys" || m.Code != code || m.TXN != txn {
			t.Fatalf("got %s %#x %q, want fsys %#x %q", m.Service, m.Code, m.TXN, code, txn)
		}
		return m
	}

	send(0xc0000001, "Hello", fesl.Field{Key: "clientString", Value: "gera"},
		fesl.Field{Key: "clientPlatform", Value: "PS2"}, fesl.Field{Key: "fragmentSize", Value: "2048"})
	hello := expect(0xc0000001, "Hello")
	for key, want := range map[string]string{"theaterIp": "192.0.2.10", "theaterPort": "18041",
		"messengerIp": "192.0.2.10", "messengerPort": "18042"} {
		if got, _ := hello.Get(key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}

	// Aries listeners send a heartbeat every five seconds. FESL must not.
	conn.SetReadDeadline(time.Now().Add(5500 * time.Millisecond))
	if f, err := aries.Read(conn); err == nil {
		t.Fatalf("unexpected server frame %q", f.Tag)
	} else if ne, ok := err.(net.Error); !ok || !ne.Timeout() {
		t.Fatal(err)
	}
	conn.SetDeadline(time.Now().Add(5 * time.Second))

	send(0xc0000002, "Ping")
	expect(0xc0000002, "Ping")

	send(0xc0000003, "Goodbye", fesl.Field{Key: "reason", Value: "1"})
	if _, err := aries.Read(conn); err != io.EOF {
		t.Fatalf("after Goodbye: %v, want EOF", err)
	}
}

// TestServeOverSSL drives the whole path a retail client takes: Definition.Serve
// wraps the listener in SSL, the client negotiates SSL 2.0, then sends Hello and
// reads the reply over the encrypted stream.
func TestServeOverSSL(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	def := Definition()
	h := def.New(game.Options{PublicIP: netip.MustParseAddr("192.0.2.10"), Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- def.Serve(ctx, ln, h) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()

	raw, err := net.Dial("tcp4", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	raw.SetDeadline(time.Now().Add(5 * time.Second))
	secure, err := ssl2test.Dial(raw)
	if err != nil {
		t.Fatal(err)
	}

	if err := fesl.Encode("fsys", 0xc0000001, "Hello",
		fesl.Field{Key: "clientString", Value: "gera"}).Write(secure); err != nil {
		t.Fatal(err)
	}
	f, err := aries.Read(secure)
	if err != nil {
		t.Fatal(err)
	}
	m := fesl.Decode(f)
	if m.Service != "fsys" || m.TXN != "Hello" {
		t.Fatalf("got %s %q, want fsys Hello", m.Service, m.TXN)
	}
	if got, _ := m.Get("theaterPort"); got != "18041" {
		t.Fatalf("theaterPort = %q, want 18041", got)
	}
}
