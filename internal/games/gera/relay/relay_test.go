package relay

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// startRelay sets up a relay directory, a hub on loopback and one connected
// node allowed to relay loopback players.
func startRelay(t *testing.T) (*Hub, context.Context) {
	t.Helper()
	dir := t.TempDir()
	token, err := Setup(dir, "test", netip.MustParseAddr("127.0.0.1"))
	if err != nil {
		t.Fatal(err)
	}
	hub, err := LoadHub(dir, quiet())
	if err != nil {
		t.Fatal(err)
	}
	cert, err := os.ReadFile(filepath.Join(dir, CertFile))
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx := runHubAndNode(t, hub, ln, &Node{Bind: netip.MustParseAddr("127.0.0.1"), PortLow: 45000, PortHigh: 45999,
		AllowPrivate: true, Log: quiet()}, ControlConfig{Server: ln.Addr().String(), CertPEM: cert, Token: token, Name: "test"})
	deadline := time.Now().Add(5 * time.Second)
	for !hub.Available() {
		if time.Now().After(deadline) {
			t.Fatal("relay node did not connect")
		}
		time.Sleep(20 * time.Millisecond)
	}
	return hub, ctx
}

// runHubAndNode serves hub and runs node until the test ends, then waits for both to stop.
func runHubAndNode(t *testing.T, hub *Hub, ln net.Listener, node *Node, cfg ControlConfig) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	hubDone, nodeDone := make(chan struct{}), make(chan struct{})
	go func() { defer close(hubDone); hub.Serve(ctx, ln) }()
	go func() { defer close(nodeDone); node.Run(ctx, cfg) }()
	t.Cleanup(func() {
		cancel()
		<-hubDone
		<-nodeDone
	})
	return ctx
}

func udp(t *testing.T) *net.UDPConn {
	t.Helper()
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func endpoint(c *net.UDPConn) netip.AddrPort { return c.LocalAddr().(*net.UDPAddr).AddrPort() }

func TestRelayForwardsBetweenPlayers(t *testing.T) {
	hub, ctx := startRelay(t)
	host, guest := udp(t), udp(t)
	room := hub.OpenRoom(ctx, map[string]netip.AddrPort{"1": endpoint(host), "2": endpoint(guest)}, nil, "test")
	if room == nil {
		t.Fatal("no room")
	}
	hostPort, ok1 := hub.Port(room, "1")
	guestPort, ok2 := hub.Port(room, "2")
	if !ok1 || !ok2 || hostPort == guestPort {
		t.Fatalf("ports %v %v", hostPort, guestPort)
	}
	// The guest sends to the port standing for the host; the host hears it
	// from the port standing for the guest, and answers there.
	if _, err := guest.WriteToUDPAddrPort([]byte("hello host"), hostPort); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	host.SetReadDeadline(time.Now().Add(3 * time.Second))
	n, from, err := host.ReadFromUDPAddrPort(buf)
	if err != nil || string(buf[:n]) != "hello host" || from != guestPort {
		t.Fatalf("host got %q from %v (%v), want from %v", buf[:n], from, err, guestPort)
	}
	host.WriteToUDPAddrPort([]byte("hello guest"), from)
	guest.SetReadDeadline(time.Now().Add(3 * time.Second))
	n, from, err = guest.ReadFromUDPAddrPort(buf)
	if err != nil || string(buf[:n]) != "hello guest" || from != hostPort {
		t.Fatalf("guest got %q from %v (%v)", buf[:n], from, err)
	}

	// A stranger's datagram is not forwarded.
	stranger, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 2)})
	if err == nil {
		defer stranger.Close()
		stranger.WriteToUDPAddrPort([]byte("spoof"), hostPort)
		host.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
		if n, _, err := host.ReadFromUDPAddrPort(buf); err == nil {
			t.Fatalf("stranger's datagram forwarded: %q", buf[:n])
		}
	}

	hub.CloseRoom(room)
	time.Sleep(100 * time.Millisecond)
	if !hub.Closed(room) {
		t.Fatal("room not closed")
	}
}

func TestRelayProbe(t *testing.T) {
	hub, ctx := startRelay(t)
	// A console answers a PROB request with code 0 and the same TID.
	console := udp(t)
	go func() {
		buf := make([]byte, 256)
		for {
			n, from, err := console.ReadFromUDPAddrPort(buf)
			if err != nil {
				return
			}
			data := buf[2:n]
			if !bytes.HasPrefix(data, []byte("PROB")) || binary.BigEndian.Uint32(data[4:8]) != probeRequest {
				continue
			}
			body := data[12:]
			var reply bytes.Buffer
			reply.Write([]byte{0xff, 0xff})
			reply.WriteString("PROB")
			binary.Write(&reply, binary.BigEndian, uint32(0))
			binary.Write(&reply, binary.BigEndian, uint32(12+len(body)))
			reply.Write(body)
			console.WriteToUDPAddrPort(reply.Bytes(), from)
		}
	}()
	got := hub.ProbeAll(ctx, endpoint(console))
	if !got["test"] {
		t.Fatalf("probe result %v, want reachable", got)
	}
	silent := udp(t)
	if got := hub.ProbeAll(ctx, endpoint(silent)); func() bool { v, ok := got["test"]; return !ok || v }() {
		t.Fatalf("silent console: probe result %v, want an answer of unreachable", got)
	}
}

func TestRelayRefusesBadToken(t *testing.T) {
	dir := t.TempDir()
	if _, err := Setup(dir, "test", netip.MustParseAddr("127.0.0.1")); err != nil {
		t.Fatal(err)
	}
	hub, err := LoadHub(dir, quiet())
	if err != nil {
		t.Fatal(err)
	}
	cert, err := os.ReadFile(filepath.Join(dir, CertFile))
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	runHubAndNode(t, hub, ln, &Node{Bind: netip.MustParseAddr("127.0.0.1"), PortLow: 46000, PortHigh: 46099, Log: quiet()},
		ControlConfig{Server: ln.Addr().String(), CertPEM: cert, Token: "wrong", Name: "test"})
	// Wait until the hub has recorded the refused hello, then check it stayed out.
	deadline := time.Now().Add(5 * time.Second)
	for !hub.sawFailedAuth() {
		if time.Now().After(deadline) {
			t.Fatal("hub never saw the node's hello")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if hub.Available() {
		t.Fatal("node with a bad token was accepted")
	}
}

func TestValidNodeName(t *testing.T) {
	for name, want := range map[string]bool{"relay1": true, "eu-west.2_a": true, "": false, "..": false,
		"../x": false, "a b": false, "a\nb": false, "a\u00a0b": false, "a#b": false} {
		if got := ValidNodeName(name); got != want {
			t.Errorf("ValidNodeName(%q) = %v, want %v", name, got, want)
		}
	}
}

// sawFailedAuth reports whether the hub has refused any control hello.
func (h *Hub) sawFailedAuth() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, attempts := range h.failures {
		if len(attempts) > 0 {
			return true
		}
	}
	return false
}

func TestRoomIDsDifferAcrossHubs(t *testing.T) {
	a, b := newHub(nil, nil, quiet()), newHub(nil, nil, quiet())
	if a.generation == "" || a.generation == b.generation {
		t.Fatalf("hub generations %q and %q", a.generation, b.generation)
	}
}
