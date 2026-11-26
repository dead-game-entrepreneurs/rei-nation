package gera

import (
	"bytes"
	"context"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/dead-game-entrepreneurs/rei-nation/internal/connectivity"
	"github.com/dead-game-entrepreneurs/rei-nation/internal/protocol/aries"
	"github.com/dead-game-entrepreneurs/rei-nation/internal/protocol/fesl"
)

// theaterClient is a theater TCP connection used in tests.
type theaterClient struct {
	t    *testing.T
	conn net.Conn
}

func (tc *theaterClient) send(kind string, fields ...fesl.Field) {
	tc.t.Helper()
	if err := fesl.Packet(kind, 0, fields...).Write(tc.conn); err != nil {
		tc.t.Fatal(err)
	}
}

func (tc *theaterClient) read() fesl.Message {
	tc.t.Helper()
	for {
		f, err := aries.Read(tc.conn)
		if err != nil {
			tc.t.Fatal(err)
		}
		if f.Tag == "PING" { // ignore server keepalives
			continue
		}
		return fesl.Decode(f)
	}
}

func dialTheater(t *testing.T, s *server, addr, key string) *theaterClient {
	t.Helper()
	conn, err := net.Dial("tcp4", addr)
	if err != nil {
		t.Fatal(err)
	}
	conn.SetDeadline(time.Now().Add(5 * time.Second))
	tc := &theaterClient{t: t, conn: conn}
	tc.send("CONN", field("tid", "1"))
	if m := tc.read(); m.Service != "CONN" {
		t.Fatalf("CONN reply = %s", m.Service)
	}
	tc.send("USER", field("tid", "2"), field("lkey", key))
	if m := tc.read(); m.Service != "USER" || m.Code != msgOK {
		t.Fatalf("USER reply = %s %#x", m.Service, m.Code)
	}
	return tc
}

// echoProbe sends a theater UDP ECHO and returns the observed endpoint the
// server reports back.
func echoProbe(t *testing.T, serverAddr netip.AddrPort, fields ...fesl.Field) {
	t.Helper()
	conn, err := net.DialUDP("udp4", nil, net.UDPAddrFromAddrPort(serverAddr))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var buf bytes.Buffer
	if err := fesl.Packet("ECHO", 0, fields...).Write(&buf); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write(buf.Bytes()); err != nil {
		t.Fatal(err)
	}
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	reply := make([]byte, 1024)
	n, err := conn.Read(reply)
	if err != nil {
		t.Fatal(err)
	}
	f, err := aries.Read(bytes.NewReader(reply[:n]))
	if err != nil || f.Tag != "ECHO" {
		t.Fatalf("ECHO reply: %v %q", err, f.Tag)
	}
}

func TestTheaterBrowserEntry(t *testing.T) {
	s := newTestServer(t)
	key := s.issueKey(identity{accountID: 1, personaID: 7, personaName: "Agent47"})

	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- aries.ServeWithOptions(ctx, ln, s.theater, aries.ServeOptions{IdleTimeout: time.Minute})
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

	send := func(kind string, fields ...fesl.Field) {
		t.Helper()
		if err := fesl.Packet(kind, 0, fields...).Write(conn); err != nil {
			t.Fatal(err)
		}
	}
	read := func() fesl.Message {
		t.Helper()
		f, err := aries.Read(conn)
		if err != nil {
			t.Fatal(err)
		}
		return fesl.Decode(f)
	}

	send("CONN", fesl.Field{Key: "tid", Value: "1"})
	if m := read(); m.Service != "CONN" || m.Code != msgOK {
		t.Fatalf("CONN reply = %s %#x", m.Service, m.Code)
	}

	// USER without a valid key is rejected.
	send("USER", fesl.Field{Key: "tid", Value: "2"}, fesl.Field{Key: "lkey", Value: "nope"})
	if m := read(); m.Service != "USER" || m.Code != msgAuth {
		t.Fatalf("bad USER reply = %s %#x, want USER auth", m.Service, m.Code)
	}

	send("USER", fesl.Field{Key: "tid", Value: "3"}, fesl.Field{Key: "lkey", Value: key})
	authed := read()
	if authed.Service != "USER" || authed.Code != msgOK {
		t.Fatalf("USER reply = %s %#x", authed.Service, authed.Code)
	}
	if v, _ := authed.Get("NAME"); v != "Agent47" {
		t.Fatalf("USER NAME = %q", v)
	}

	// Lobby list: an LLST header followed by one LDAT.
	send("LLST", fesl.Field{Key: "tid", Value: "4"})
	if m := read(); m.Service != "LLST" {
		t.Fatalf("expected LLST, got %s", m.Service)
	}
	ldat := read()
	if ldat.Service != "LDAT" {
		t.Fatalf("expected LDAT, got %s", ldat.Service)
	}
	if v, _ := ldat.Get("NAME"); v != "Local" {
		t.Fatalf("LDAT NAME = %q", v)
	}

	// Game list is empty for now.
	send("GLST", fesl.Field{Key: "tid", Value: "5"}, fesl.Field{Key: "lid", Value: "1"})
	glst := read()
	if glst.Service != "GLST" {
		t.Fatalf("expected GLST, got %s", glst.Service)
	}
	if v, _ := glst.Get("NUM-GAMES"); v != "0" {
		t.Fatalf("GLST NUM-GAMES = %q, want 0", v)
	}
}

func TestTheaterHostAndJoin(t *testing.T) {
	s := newTestServer(t)
	hostKey := s.issueKey(identity{accountID: 1, personaID: 10, personaName: "Host"})
	joinKey := s.issueKey(identity{accountID: 2, personaID: 20, personaName: "Joiner"})

	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	udp, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	tcpDone := make(chan error, 1)
	udpDone := make(chan error, 1)
	go func() {
		tcpDone <- aries.ServeWithOptions(ctx, ln, s.theater, aries.ServeOptions{IdleTimeout: time.Minute})
	}()
	go func() {
		udpDone <- connectivity.ServeUDP(ctx, udp, func(from netip.AddrPort, data []byte) {
			s.theater.handleEcho(udp, from, data)
		})
	}()
	defer func() {
		cancel()
		ln.Close()
		udp.Close()
		<-tcpDone
		<-udpDone
	}()
	udpAddr := netip.MustParseAddrPort(udp.LocalAddr().String())

	host := dialTheater(t, s, ln.Addr().String(), hostKey)
	defer host.conn.Close()

	// Host creates a game.
	host.send("CGAM", field("tid", "3"), field("lid", "1"), field("name", "My Game"),
		field("max-players", "8"), field("port", "3000"), field("int-ip", pcsx2SocketIP), field("int-port", "3000"))
	cgam := host.read()
	if cgam.Service != "CGAM" || cgam.Code != msgOK {
		t.Fatalf("CGAM reply = %s %#x", cgam.Service, cgam.Code)
	}
	gid, _ := cgam.Get("GID")
	secret, _ := cgam.Get("SECRET")
	if gid == "" || secret == "" {
		t.Fatalf("CGAM missing GID/SECRET: %q %q", gid, secret)
	}

	// Host advertises its public UDP endpoint via ECHO (host-secret binding).
	echoProbe(t, udpAddr, field("tid", "1"), field("type", "1"), field("ugid", gid), field("secret", secret))

	// Joiner lists games and sees the hosted one with the observed endpoint.
	joiner := dialTheater(t, s, ln.Addr().String(), joinKey)
	defer joiner.conn.Close()
	joiner.send("GLST", field("tid", "4"), field("lid", "1"))
	glst := joiner.read()
	if v, _ := glst.Get("NUM-GAMES"); v != "1" {
		t.Fatalf("GLST NUM-GAMES = %q, want 1", v)
	}
	gdat := joiner.read()
	if gdat.Service != "GDAT" {
		t.Fatalf("expected GDAT, got %s", gdat.Service)
	}
	if v, _ := gdat.Get("N"); v != "My Game" {
		t.Fatalf("GDAT name = %q", v)
	}
	if v, _ := gdat.Get("I"); v != "127.0.0.1" {
		t.Fatalf("GDAT host IP = %q, want the observed 127.0.0.1", v)
	}

	// Joiner requests to join; the host receives an EGRQ.
	joiner.send("EGAM", field("tid", "5"), field("gid", gid), field("lid", "1"), field("port", "3000"))
	egam := joiner.read()
	if egam.Service != "EGAM" || egam.Code != msgOK {
		t.Fatalf("EGAM reply = %s %#x", egam.Service, egam.Code)
	}
	egrq := host.read()
	if egrq.Service != "EGRQ" {
		t.Fatalf("host expected EGRQ, got %s", egrq.Service)
	}
	pid, _ := egrq.Get("PID")
	if v, _ := egrq.Get("NAME"); v != "Joiner" {
		t.Fatalf("EGRQ NAME = %q", v)
	}

	// Host approves; the joiner receives an EGEG with the host endpoint.
	host.send("EGRS", field("tid", "6"), field("pid", pid), field("allowed", "1"))
	if m := host.read(); m.Service != "EGRS" {
		t.Fatalf("host EGRS reply = %s", m.Service)
	}
	egeg := joiner.read()
	if egeg.Service != "EGEG" {
		t.Fatalf("joiner expected EGEG, got %s", egeg.Service)
	}
	if v, _ := egeg.Get("I"); v != "127.0.0.1" {
		t.Fatalf("EGEG host I = %q", v)
	}
	if v, _ := egeg.Get("INT-IP"); v != "0.0.0.0" {
		t.Fatalf("EGEG INT-IP = %q, want suppressed 0.0.0.0", v)
	}
}
