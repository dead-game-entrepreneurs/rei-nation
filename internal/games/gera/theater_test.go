package gera

import (
	"bytes"
	"context"
	"net"
	"net/netip"
	"strconv"
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
	t.Cleanup(func() { conn.Close() })
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

// echoProbe sends a theater UDP ECHO from a fresh socket and returns that
// socket's source port, which the server should record.
func echoProbe(t *testing.T, serverAddr netip.AddrPort, fields ...fesl.Field) int {
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
	return conn.LocalAddr().(*net.UDPAddr).Port
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

	// Lobby list: an LLST header followed by one LDAT per lobby.
	send("LLST", fesl.Field{Key: "tid", Value: "4"})
	if m := read(); m.Service != "LLST" {
		t.Fatalf("expected LLST, got %s", m.Service)
	}
	ldat := read()
	if ldat.Service != "LDAT" {
		t.Fatalf("expected LDAT, got %s", ldat.Service)
	}
	if v, _ := ldat.Get("FAVORITE-PLAYERS"); v == "0" || v == "" {
		t.Fatalf("LDAT FAVORITE-PLAYERS = %q; the client hosts only in a lobby where it is non-zero", v)
	}
	if v, _ := ldat.Get("NAME"); v != lobbyName {
		t.Fatalf("LDAT NAME = %q", v)
	}
	// Remaining lobbies follow, one LDAT each.
	for i := 1; i < len(lobbies); i++ {
		if m := read(); m.Service != "LDAT" {
			t.Fatalf("expected LDAT %d, got %s", i+1, m.Service)
		}
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
	t.Cleanup(func() { ln.Close() })
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

	// Host advertises its public UDP endpoint via ECHO (host-secret binding);
	// the observed port differs from the advertised 3000.
	observedPort := strconv.Itoa(echoProbe(t, udpAddr, field("tid", "1"), field("type", "1"), field("ugid", gid), field("secret", secret)))

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
	if v, _ := gdat.Get("P"); v != observedPort {
		t.Fatalf("GDAT host port = %q, want the observed %s", v, observedPort)
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
	if v, _ := egeg.Get("P"); v != observedPort {
		t.Fatalf("EGEG host P = %q, want the observed %s", v, observedPort)
	}
	if v, _ := egeg.Get("INT-IP"); v != "0.0.0.0" {
		t.Fatalf("EGEG INT-IP = %q, want suppressed 0.0.0.0", v)
	}
}

// A host and joiner on one network can pick different lobbies, so every game
// must be listed and joinable from every lobby.
func TestTheaterGamesVisibleAcrossLobbies(t *testing.T) {
	s := newTestServer(t)
	hostKey := s.issueKey(identity{accountID: 1, personaID: 10, personaName: "Host"})
	joinKey := s.issueKey(identity{accountID: 2, personaID: 20, personaName: "Joiner"})
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
		ln.Close()
		<-done
	}()

	host := dialTheater(t, s, ln.Addr().String(), hostKey)
	defer host.conn.Close()
	host.send("CGAM", field("tid", "3"), field("lid", "2"), field("name", "Asia Game"), field("port", "18040"))
	cgam := host.read()
	gid, _ := cgam.Get("GID")

	joiner := dialTheater(t, s, ln.Addr().String(), joinKey)
	defer joiner.conn.Close()
	joiner.send("GLST", field("tid", "4"), field("lid", "1"))
	if v, _ := joiner.read().Get("NUM-GAMES"); v != "1" {
		t.Fatalf("lobby 1 NUM-GAMES = %q, want the lobby 2 game listed", v)
	}
	gdat := joiner.read()
	if v, _ := gdat.Get("LID"); v != "1" {
		t.Fatalf("GDAT LID = %q, want the viewer's lobby 1", v)
	}
	joiner.send("EGAM", field("tid", "5"), field("gid", gid), field("lid", "1"), field("port", "18040"))
	if m := joiner.read(); m.Service != "EGAM" || m.Code != msgOK {
		t.Fatalf("cross-lobby EGAM = %s %#x, want accepted", m.Service, m.Code)
	}
	egrq := host.read()
	if egrq.Service != "EGRQ" {
		t.Fatalf("host expected EGRQ, got %s", egrq.Service)
	}
	pid, _ := egrq.Get("PID")
	host.send("EGRS", field("tid", "6"), field("pid", pid), field("allowed", "1"))
	if m := host.read(); m.Service != "EGRS" {
		t.Fatalf("host EGRS reply = %s", m.Service)
	}
	if m := joiner.read(); m.Service != "EGEG" {
		t.Fatalf("joiner expected EGEG, got %s", m.Service)
	}

	// When the host removes the game at the end of a match, the joiner must
	// be told, in the lobby it joined from, or it reports an error.
	host.send("RGAM", field("tid", "7"))
	if m := host.read(); m.Service != "RGAM" {
		t.Fatalf("host RGAM reply = %s", m.Service)
	}
	grem := joiner.read()
	if grem.Service != "GREM" {
		t.Fatalf("joiner expected GREM, got %s", grem.Service)
	}
	if v, _ := grem.Get("GID"); v != gid {
		t.Fatalf("GREM GID = %q, want %q", v, gid)
	}
	if v, _ := grem.Get("LID"); v != "1" {
		t.Fatalf("GREM LID = %q, want the joiner's lobby 1", v)
	}
}

// joinApproved runs a guest through EGAM, the host's approval and EGEG, and
// returns the guest's PID.
func joinApproved(t *testing.T, host, guest *theaterClient, gid string) string {
	t.Helper()
	guest.send("EGAM", field("tid", "20"), field("gid", gid), field("lid", "1"), field("port", "18040"))
	if m := guest.read(); m.Service != "EGAM" || m.Code != msgOK {
		t.Fatalf("EGAM = %s %#x", m.Service, m.Code)
	}
	egrq := host.read()
	if egrq.Service != "EGRQ" {
		t.Fatalf("host expected EGRQ, got %s", egrq.Service)
	}
	pid, _ := egrq.Get("PID")
	host.send("EGRS", field("tid", "21"), field("pid", pid), field("allowed", "1"))
	if m := host.read(); m.Service != "EGRS" {
		t.Fatalf("host EGRS reply = %s", m.Service)
	}
	if m := guest.read(); m.Service != "EGEG" {
		t.Fatalf("guest expected EGEG, got %s", m.Service)
	}
	return pid
}

// An approved guest that cancels or drops must be reported to the host with
// KICK, or the host keeps its seat reserved and the game fills up for good.
func TestTheaterKicksHostWhenApprovedGuestLeaves(t *testing.T) {
	s := newTestServer(t)
	hostKey := s.issueKey(identity{accountID: 1, personaID: 10, personaName: "Host"})
	aKey := s.issueKey(identity{accountID: 2, personaID: 20, personaName: "Canceller"})
	bKey := s.issueKey(identity{accountID: 3, personaID: 30, personaName: "Dropper"})
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
		ln.Close()
		<-done
	}()

	host := dialTheater(t, s, ln.Addr().String(), hostKey)
	defer host.conn.Close()
	host.send("CGAM", field("tid", "3"), field("lid", "1"), field("name", "Kick Test"), field("port", "18040"))
	gid, _ := host.read().Get("GID")

	// Cancel path.
	a := dialTheater(t, s, ln.Addr().String(), aKey)
	defer a.conn.Close()
	pidA := joinApproved(t, host, a, gid)
	a.send("ECNL", field("tid", "22"), field("gid", gid), field("lid", "1"))
	if m := a.read(); m.Service != "ECNL" {
		t.Fatalf("ECNL reply = %s", m.Service)
	}
	k := host.read()
	if k.Service != "KICK" {
		t.Fatalf("host expected KICK after cancel, got %s", k.Service)
	}
	if v, _ := k.Get("PID"); v != pidA {
		t.Fatalf("KICK PID = %q, want %q", v, pidA)
	}

	// Disconnect path.
	b := dialTheater(t, s, ln.Addr().String(), bKey)
	pidB := joinApproved(t, host, b, gid)
	b.conn.Close()
	k = host.read()
	if k.Service != "KICK" {
		t.Fatalf("host expected KICK after disconnect, got %s", k.Service)
	}
	if v, _ := k.Get("PID"); v != pidB {
		t.Fatalf("KICK PID = %q, want %q", v, pidB)
	}
}

// On a shared public IP an anonymous ECHO goes only to the connection already
// seen on that port; a new port may be a seen console whose NAT moved.
func TestTheaterEchoAttributionSharedIP(t *testing.T) {
	s := newTestServer(t)
	h := s.theater
	c1, c2 := &aries.Client{}, &aries.Client{}
	anon := fesl.Message{}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.connIP[c1] = "203.0.113.5"
	h.connIP[c2] = "203.0.113.5"

	if got := h.attributeEcho(anon, "203.0.113.5", 40001); got != nil {
		t.Fatal("two unseen sessions: attribution should be refused")
	}
	h.observed[c1] = addr{"203.0.113.5", 40001}
	if got := h.attributeEcho(anon, "203.0.113.5", 40001); got != c1 {
		t.Fatal("known port should refresh its own session")
	}
	if got := h.attributeEcho(anon, "203.0.113.5", 40002); got != nil {
		t.Fatal("new port on a shared IP should not be guessed")
	}
	h.observed[c2] = addr{"203.0.113.5", 40002}
	if got := h.attributeEcho(anon, "203.0.113.5", 40003); got != nil {
		t.Fatal("unknown port with every session seen should be refused")
	}
	if got := h.attributeEcho(anon, "198.51.100.9", 40001); got != nil {
		t.Fatal("an IP with no session should not be attributed")
	}
}

// Remote viewers get the public address for a console on the server's LAN;
// viewers on that LAN keep its private address.
func TestTheaterReachableEndpoint(t *testing.T) {
	s := newTestServer(t) // REI_NATION_PUBLIC_IP 192.0.2.10
	h := s.theater
	lanHost := addr{"192.168.88.175", 18040}
	cases := []struct {
		name   string
		in     addr
		viewer string
		want   addr
	}{
		{"remote viewer of LAN host", lanHost, "198.51.100.7", addr{"192.0.2.10", 18040}},
		{"LAN viewer of LAN host", lanHost, "192.168.88.125", lanHost},
		{"remote host", addr{"203.0.113.9", 40001}, "198.51.100.7", addr{"203.0.113.9", 40001}},
		{"LAN viewer of remote host", addr{"203.0.113.9", 40001}, "192.168.88.125", addr{"203.0.113.9", 40001}},
		{"suppressed alternate", addr{"0.0.0.0", 0}, "198.51.100.7", addr{"0.0.0.0", 0}},
	}
	for _, tc := range cases {
		if got := h.reachable(tc.in, tc.viewer); got != tc.want {
			t.Errorf("%s: reachable(%v, %s) = %v, want %v", tc.name, tc.in, tc.viewer, got, tc.want)
		}
	}

	// With a private REI_NATION_PUBLIC_IP (a LAN-only server) nothing changes.
	s.opts.PublicIP = netip.MustParseAddr("192.168.88.175")
	if got := h.reachable(lanHost, "198.51.100.7"); got != lanHost {
		t.Errorf("LAN-only server rewrote %v to %v", lanHost, got)
	}

	// A remote joiner of a LAN host gets the public address, and the host's
	// internal address is never offered as an unreachable alternate.
	s.opts.PublicIP = netip.MustParseAddr("192.0.2.10")
	host := &aries.Client{}
	h.mu.Lock()
	h.observed[host] = lanHost
	g := &hostedGame{owner: host, hostIP: "192.168.88.175", port: 18040,
		internalIP: "192.168.88.175", internalPort: 18040}
	primary, alternate := h.hostAddresses(g, "198.51.100.7", false)
	h.mu.Unlock()
	if primary != (addr{"192.0.2.10", 18040}) || alternate != (addr{"0.0.0.0", 0}) {
		t.Errorf("remote joiner got primary %v alternate %v", primary, alternate)
	}
}

// A host approval that arrives after the guest cancelled gets a KICK, so the
// host releases the seat it reserved.
func TestTheaterKicksLateApprovalAfterCancel(t *testing.T) {
	s := newTestServer(t)
	hostKey := s.issueKey(identity{accountID: 1, personaID: 10, personaName: "Host"})
	guestKey := s.issueKey(identity{accountID: 2, personaID: 20, personaName: "Guest"})
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- aries.ServeWithOptions(ctx, ln, s.theater, aries.ServeOptions{IdleTimeout: time.Minute})
	}()
	t.Cleanup(func() { cancel(); <-done })

	host := dialTheater(t, s, ln.Addr().String(), hostKey)
	t.Cleanup(func() { host.conn.Close() })
	host.send("CGAM", field("tid", "3"), field("lid", "1"), field("name", "Late"), field("port", "18040"))
	gid, _ := host.read().Get("GID")
	guest := dialTheater(t, s, ln.Addr().String(), guestKey)
	t.Cleanup(func() { guest.conn.Close() })
	guest.send("EGAM", field("tid", "20"), field("lid", "1"), field("gid", gid), field("port", "3000"))
	if m := guest.read(); m.Service != "EGAM" {
		t.Fatalf("guest EGAM reply = %s", m.Service)
	}
	pid, _ := host.read().Get("PID")
	guest.send("ECNL", field("tid", "22"), field("gid", gid), field("lid", "1"))
	guest.read()

	host.send("EGRS", field("tid", "21"), field("pid", pid), field("allowed", "1"))
	if m := host.read(); m.Service != "EGRS" {
		t.Fatalf("host EGRS reply = %s", m.Service)
	}
	if k := host.read(); k.Service != "KICK" {
		t.Fatalf("host expected KICK for the cancelled join, got %s", k.Service)
	} else if v, _ := k.Get("PID"); v != pid {
		t.Fatalf("KICK PID = %q, want %q", v, pid)
	}
}

// Operator force rules never fall back to a direct join, even on a shared IP.
func TestForcedRelayWithoutRelayRefuses(t *testing.T) {
	s := newTestServer(t)
	s.relayPolicy = newRelayPolicy([]string{"*"})
	g := &hostedGame{owner: &aries.Client{}, hostIP: "198.51.100.1", port: 3000}
	s.theater.mu.Lock()
	defer s.theater.mu.Unlock()
	for _, ip := range []string{"203.0.113.9", "198.51.100.1"} {
		if mode, reason := s.theater.relayDecision(g, identity{personaID: 2}, ip); mode != "refuse" {
			t.Fatalf("joiner %s: mode %q (%s), want refuse", ip, mode, reason)
		}
	}
}
