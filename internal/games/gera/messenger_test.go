package gera

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/dead-game-entrepreneurs/rei-nation/internal/protocol/aries"
	"github.com/dead-game-entrepreneurs/rei-nation/internal/protocol/fesl"
)

func TestMessengerAuthAndPresence(t *testing.T) {
	s := newTestServer(t)
	key := s.issueKey(identity{accountID: 1, personaID: 7, personaName: "Agent47"})

	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- aries.ServeWithOptions(ctx, ln, s.messenger, aries.ServeOptions{IdleTimeout: time.Minute})
	}()
	defer func() {
		cancel()
		<-done
		ln.Close()
	}()

	conn, err := net.Dial("tcp4", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(5 * time.Second))

	call := func(kind string, fields ...fesl.Field) fesl.Message {
		t.Helper()
		if err := fesl.Packet(kind, 0, fields...).Write(conn); err != nil {
			t.Fatal(err)
		}
		f, err := aries.Read(conn)
		if err != nil {
			t.Fatal(err)
		}
		return fesl.Decode(f)
	}

	// A request before AUTH is rejected with the "auth" status.
	if m := call("RGET", fesl.Field{Key: "id", Value: "1"}); m.Code != msgAuth {
		t.Fatalf("pre-auth RGET code = %#x, want %#x", m.Code, msgAuth)
	}

	// A bad key is rejected.
	if m := call("AUTH", fesl.Field{Key: "id", Value: "2"}, fesl.Field{Key: "lkey", Value: "deadbeef"}); m.Code != msgAuth {
		t.Fatalf("bad-key AUTH code = %#x, want %#x", m.Code, msgAuth)
	}

	// The real key authenticates and returns the persona name.
	authed := call("AUTH", fesl.Field{Key: "id", Value: "3"}, fesl.Field{Key: "lkey", Value: key})
	if authed.Service != "AUTH" || authed.Code != msgOK {
		t.Fatalf("AUTH reply = %s %#x, want AUTH 0", authed.Service, authed.Code)
	}
	if v, _ := authed.Get("USER"); v != "Agent47" {
		t.Fatalf("AUTH USER = %q, want Agent47", v)
	}
	if v, _ := authed.Get("ID"); v != "3" {
		t.Fatalf("AUTH ID = %q, want 3", v)
	}

	if m := call("PSET", fesl.Field{Key: "id", Value: "4"}, fesl.Field{Key: "show", Value: "CHAT"}); m.Code != msgOK {
		t.Fatalf("PSET code = %#x", m.Code)
	}
	if m := call("PING", fesl.Field{Key: "id", Value: "5"}); m.Service != "PING" || m.Code != msgOK {
		t.Fatalf("PING reply = %s %#x", m.Service, m.Code)
	}
	roster := call("RGET", fesl.Field{Key: "id", Value: "6"}, fesl.Field{Key: "list", Value: "B"})
	if v, _ := roster.Get("SIZE"); v != "0" {
		t.Fatalf("RGET SIZE = %q, want 0", v)
	}
}
