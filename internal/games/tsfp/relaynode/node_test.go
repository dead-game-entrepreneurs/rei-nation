package relaynode

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/netip"
	"testing"
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
}
