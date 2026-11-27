package gera

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/dead-game-entrepreneurs/rei-nation/internal/connectivity"
	"github.com/dead-game-entrepreneurs/rei-nation/internal/game"
	"github.com/dead-game-entrepreneurs/rei-nation/internal/games/gera/relay"
	"github.com/dead-game-entrepreneurs/rei-nation/internal/protocol/aries"
	"github.com/dead-game-entrepreneurs/rei-nation/internal/protocol/fesl"
)

// TestTheaterRelayedJoin forces a join through a relay node on loopback and
// checks that each console is given the other's relay address and that
// datagrams flow between them through it.
func TestTheaterRelayedJoin(t *testing.T) {
	joinerIP := net.IPv4(127, 0, 0, 2)
	if c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: joinerIP}); err != nil {
		t.Skip("127.0.0.2 is not usable here")
	} else {
		c.Close()
	}
	dir := t.TempDir()
	token, err := relay.Setup(dir, "test", netip.MustParseAddr("127.0.0.1"))
	if err != nil {
		t.Fatal(err)
	}
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	// A loopback public IP keeps observed loopback endpoints as they are.
	s := Definition().New(game.Options{PublicIP: netip.MustParseAddr("127.0.0.1"), Log: quiet,
		Store: newMemStore(), RelayDir: dir, RelayForce: []string{"*"}}).(*server)
	if s.relays == nil {
		t.Fatal("relay hub not loaded")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	control, _ := net.Listen("tcp4", "127.0.0.1:0")
	go s.relays.Serve(ctx, control)
	cert, _ := os.ReadFile(filepath.Join(dir, relay.CertFile))
	node := &relay.Node{Bind: netip.MustParseAddr("127.0.0.1"), PortLow: 45000, PortHigh: 45999, AllowPrivate: true, Log: quiet}
	go node.Run(ctx, relay.ControlConfig{Server: control.Addr().String(), CertPEM: cert, Token: token, Name: "test"})

	ln, _ := net.Listen("tcp4", "127.0.0.1:0")
	udp, _ := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	go aries.ServeWithOptions(ctx, ln, s.theater, aries.ServeOptions{IdleTimeout: time.Minute})
	go connectivity.ServeUDP(ctx, udp, func(from netip.AddrPort, data []byte) { s.theater.handleEcho(udp, from, data) })
	defer udp.Close()
	theaterUDP := udp.LocalAddr().(*net.UDPAddr).AddrPort()

	deadline := time.Now().Add(5 * time.Second)
	for !s.relays.Available() {
		if time.Now().After(deadline) {
			t.Fatal("relay node did not connect")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// The consoles' game sockets.
	hostUDP, _ := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	defer hostUDP.Close()
	joinUDP, _ := net.ListenUDP("udp4", &net.UDPAddr{IP: joinerIP})
	defer joinUDP.Close()
	echo := func(c *net.UDPConn, fields ...fesl.Field) {
		t.Helper()
		var b bytes.Buffer
		fesl.Packet("ECHO", 0, fields...).Write(&b)
		c.WriteToUDPAddrPort(b.Bytes(), theaterUDP)
		c.SetReadDeadline(time.Now().Add(3 * time.Second))
		buf := make([]byte, 512)
		if _, _, err := c.ReadFromUDPAddrPort(buf); err != nil {
			t.Fatal(err)
		}
	}

	host := dialTheater(t, s, ln.Addr().String(), s.issueKey(identity{accountID: 1, personaID: 10, personaName: "Host"}))
	defer host.conn.Close()
	host.send("CGAM", field("tid", "3"), field("lid", "1"), field("name", "Relay"), field("max-players", "4"), field("port", "18040"))
	cgam := host.read()
	gid, _ := cgam.Get("GID")
	secret, _ := cgam.Get("SECRET")
	echo(hostUDP, field("tid", "1"), field("type", "1"), field("ugid", gid), field("secret", secret))

	d := net.Dialer{LocalAddr: &net.TCPAddr{IP: joinerIP}}
	jc, err := d.Dial("tcp4", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	jc.SetDeadline(time.Now().Add(10 * time.Second))
	joiner := &theaterClient{t: t, conn: jc}
	defer jc.Close()
	joiner.send("CONN", field("tid", "1"))
	joiner.read()
	joiner.send("USER", field("tid", "2"), field("lkey", s.issueKey(identity{accountID: 2, personaID: 20, personaName: "Guest"})))
	joiner.read()
	echo(joinUDP, field("tid", "1"), field("type", "1"))

	host.conn.SetDeadline(time.Now().Add(10 * time.Second))
	joiner.send("EGAM", field("tid", "5"), field("gid", gid), field("lid", "1"), field("port", "18040"))
	if m := joiner.read(); m.Service != "EGAM" || m.Code != msgOK {
		t.Fatalf("EGAM = %s %#x", m.Service, m.Code)
	}
	egrq := host.read()
	if egrq.Service != "EGRQ" {
		t.Fatalf("host got %s, want EGRQ", egrq.Service)
	}
	joinerAt := relayAddr(t, egrq, "IP", "PORT")
	pid, _ := egrq.Get("PID")
	host.send("EGRS", field("tid", "6"), field("pid", pid), field("allowed", "1"))
	host.read()
	egeg := joiner.read()
	if egeg.Service != "EGEG" {
		t.Fatalf("joiner got %s, want EGEG", egeg.Service)
	}
	hostAt := relayAddr(t, egeg, "I", "P")
	if v, _ := egeg.Get("INT-IP"); v != "0.0.0.0" {
		t.Fatalf("EGEG INT-IP = %q, want 0.0.0.0", v)
	}

	// The joiner sends to the host's relay port; the host hears it from the
	// joiner's relay port, the address it was given in EGRQ.
	joinUDP.WriteToUDPAddrPort([]byte("hello host"), hostAt)
	from := receive(t, hostUDP, "hello host")
	if from != joinerAt {
		t.Fatalf("host heard the joiner from %v, want %v", from, joinerAt)
	}
	hostUDP.WriteToUDPAddrPort([]byte("hello guest"), joinerAt)
	if from := receive(t, joinUDP, "hello guest"); from != hostAt {
		t.Fatalf("joiner heard the host from %v, want %v", from, hostAt)
	}
}

func relayAddr(t *testing.T, m fesl.Message, ipKey, portKey string) netip.AddrPort {
	t.Helper()
	ip, _ := m.Get(ipKey)
	port, _ := m.Get(portKey)
	p, err := strconv.Atoi(port)
	if ip != "127.0.0.1" || err != nil || p < 45000 || p > 45999 {
		t.Fatalf("%s %s=%s %s=%s, want a relay address", m.Service, ipKey, ip, portKey, port)
	}
	return netip.AddrPortFrom(netip.MustParseAddr(ip), uint16(p))
}

// receive reads until payload arrives, skipping relay probes.
func receive(t *testing.T, c *net.UDPConn, payload string) netip.AddrPort {
	t.Helper()
	buf := make([]byte, 512)
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		n, from, err := c.ReadFromUDPAddrPort(buf)
		if err != nil {
			t.Fatalf("waiting for %q: %v", payload, err)
		}
		if string(buf[:n]) == payload {
			return from
		}
	}
}
