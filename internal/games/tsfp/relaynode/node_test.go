package relaynode

import (
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"strconv"
	"testing"
	"time"
)

func TestMembersAcceptCurrentAndLegacyRemoveShapes(t *testing.T) {
	for _, test := range []struct {
		wire string
		keys []string
	}{
		{`{"members":{"1":["203.0.114.1",3000]}}`, []string{"1"}},
		{`{"members":["1","2"]}`, []string{"1", "2"}},
	} {
		var request message
		if err := json.Unmarshal([]byte(test.wire), &request); err != nil {
			t.Fatal(err)
		}
		for _, key := range test.keys {
			if _, ok := request.Members[key]; !ok {
				t.Fatalf("%s did not contain member %q", test.wire, key)
			}
		}
	}
}

func TestAllowedIPv4(t *testing.T) {
	for _, address := range []string{"10.0.0.1", "100.64.0.1", "127.0.0.1", "192.0.2.1", "198.51.100.1"} {
		if allowedIPv4(netip.MustParseAddr(address), false) {
			t.Fatalf("accepted non-public address %s", address)
		}
	}
	if !allowedIPv4(netip.MustParseAddr("8.8.8.8"), false) {
		t.Fatal("rejected public IPv4 address")
	}
}

func TestFailedRoomAddReleasesPorts(t *testing.T) {
	node, err := New(Config{Bind: netip.MustParseAddr("127.0.0.31"), FirstPort: 23000, LastPort: 23000,
		AllowPrivate: true, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	_, err = node.add("room", members{
		"1": {"127.0.0.2", float64(3000)}, "2": {"127.0.0.3", float64(3000)},
	})
	if err == nil {
		t.Fatal("two members unexpectedly fit on one relay port")
	}
	if len(node.rooms) != 0 {
		t.Fatal("failed room was retained")
	}
	ports, err := node.add("fresh", members{"1": {"127.0.0.2", float64(3000)}})
	if err != nil {
		t.Fatalf("released port could not be reused: %v", err)
	}
	defer node.close("fresh")
	if ports["1"] != 23000 {
		t.Fatalf("reused port = %d, want 23000", ports["1"])
	}
}

func TestRoomLimitAllowsExistingMemberUpdate(t *testing.T) {
	node := testNode(t, 23100, 23116)
	initial := make(members, maxMembers)
	for i := range maxMembers {
		initial[strconv.Itoa(i)] = []any{"127.0.0.2", float64(3000 + i)}
	}
	if _, err := node.add("room", initial); err != nil {
		t.Fatal(err)
	}
	defer node.close("room")
	if _, err := node.add("room", members{"0": {"127.0.0.2", float64(4000)}}); err != nil {
		t.Fatalf("existing member update at capacity failed: %v", err)
	}
	if _, err := node.add("room", members{"extra": {"127.0.0.3", float64(4001)}}); err == nil {
		t.Fatal("seventeenth member was accepted")
	}
}

func TestFailedBatchPreservesExistingEndpoint(t *testing.T) {
	node := testNode(t, 23200, 23200)
	original := netip.MustParseAddrPort("127.0.0.2:3000")
	if _, err := node.add("room", members{"1": {original.Addr().String(), float64(original.Port())}}); err != nil {
		t.Fatal(err)
	}
	defer node.close("room")
	_, err := node.add("room", members{
		"1": {"127.0.0.2", float64(4000)}, "2": {"127.0.0.3", float64(3000)},
	})
	if err == nil {
		t.Fatal("batch unexpectedly fit on one relay port")
	}
	if got := node.rooms["room"].members["1"].endpoint; got != original {
		t.Fatalf("existing endpoint changed to %s after rollback", got)
	}
}

func TestForwardRejectsUnregisteredSourcePort(t *testing.T) {
	node := testNode(t, 23300, 23301)
	host := testSocket(t, "127.0.0.2")
	attacker := testSocket(t, "127.0.0.2")
	guest := testSocket(t, "127.0.0.3")
	defer host.Close()
	defer attacker.Close()
	defer guest.Close()
	ports, err := node.add("room", members{
		"host": socketTarget(host), "guest": socketTarget(guest),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer node.close("room")
	target := netip.AddrPortFrom(node.bind, uint16(ports["guest"]))
	if _, err := attacker.WriteToUDPAddrPort([]byte("unregistered"), target); err != nil {
		t.Fatal(err)
	}
	if err := guest.SetReadDeadline(time.Now().Add(150 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	var data [32]byte
	if _, _, err := guest.ReadFromUDPAddrPort(data[:]); err == nil {
		t.Fatal("forwarded a datagram from an unregistered source port")
	}
	if _, err := host.WriteToUDPAddrPort([]byte("registered"), target); err != nil {
		t.Fatal(err)
	}
	if err := guest.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	size, _, err := guest.ReadFromUDPAddrPort(data[:])
	if err != nil || string(data[:size]) != "registered" {
		t.Fatalf("registered datagram = %q, %v", data[:size], err)
	}
}

func testNode(t *testing.T, first, last uint16) *Node {
	t.Helper()
	node, err := New(Config{Bind: netip.MustParseAddr("127.0.0.31"), FirstPort: first, LastPort: last,
		AllowPrivate: true, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	return node
}

func testSocket(t *testing.T, ip string) *net.UDPConn {
	t.Helper()
	socket, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP(ip), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	return socket
}

func socketTarget(socket *net.UDPConn) []any {
	address := socket.LocalAddr().(*net.UDPAddr).AddrPort()
	return []any{address.Addr().String(), float64(address.Port())}
}
