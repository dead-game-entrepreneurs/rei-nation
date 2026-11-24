package nascar04

import (
	"net"
	"net/netip"
	"testing"
	"time"
)

func TestRaceRelayPortsAndOwnership(t *testing.T) {
	open := func(ip string) *net.UDPConn {
		s, err := net.ListenUDP("udp4", net.UDPAddrFromAddrPort(netip.MustParseAddrPort(ip+":0")))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.Close() })
		return s
	}
	addr := func(s *net.UDPConn) netip.AddrPort {
		return s.LocalAddr().(*net.UDPAddr).AddrPort()
	}
	sockets := map[uint16]*net.UDPConn{
		guestPort: open("127.0.0.1"), hostPort: open("127.0.0.1"), peerPort: open("127.0.0.1"),
	}
	host, guest := open("127.0.0.2"), open("127.0.0.3")
	r := &raceRelay{mode: "always", sockets: sockets, pairs: make(map[netip.Addr]*relayPair)}
	drop := r.admit(addr(host).Addr(), addr(guest).Addr())
	if drop == nil || r.admit(addr(host).Addr(), netip.MustParseAddr("127.0.0.4")) != nil {
		t.Fatal("incorrect admission for overlapping relay pairs")
	}
	forward := func(port uint16, from *net.UDPConn, data string) {
		r.forward(port, addr(from), []byte(data))
	}
	receive := func(to *net.UDPConn, port uint16, want string) {
		to.SetReadDeadline(time.Now().Add(time.Second))
		buf := make([]byte, 16)
		n, from, err := to.ReadFromUDPAddrPort(buf)
		if err != nil || from != addr(sockets[port]) || string(buf[:n]) != want {
			t.Fatalf("forwarded %q from %s, error %v, expected %q from port %d", buf[:n], from, err, want, port)
		}
	}
	forward(hostPort, host, "host open")
	forward(guestPort, guest, "guest open")
	receive(host, hostPort, "guest open")
	forward(guestPort, host, "wrong role")
	forward(hostPort, open("127.0.0.2"), "wrong endpoint")
	forward(hostPort, host, "race")
	receive(guest, guestPort, "race")
	forward(peerPort, guest, "guest open")
	forward(peerPort, host, "voice setup")
	receive(guest, peerPort, "voice setup")
	forward(peerPort, guest, "reply")
	receive(host, peerPort, "reply")
	drop()
	if r.pairs[addr(host).Addr()] != nil || r.pairs[addr(guest).Addr()] != nil {
		t.Fatal("pair remained after release")
	}
}
