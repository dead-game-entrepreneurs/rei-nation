package connectivity

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"
)

func TestDemanglerAdmissionAndObservedEndpoints(t *testing.T) {
	server := netip.MustParseAddr("192.0.2.10")
	host := netip.MustParseAddr("198.51.100.1")
	guest := netip.MustParseAddr("198.51.100.2")
	d := NewDemangler(server, 3658)
	cancel := d.Admit("host", host, guest, time.Now().Add(time.Minute))
	lookup := func(ip netip.Addr, name string) string {
		r := httptest.NewRequest("GET", "/getPeerAddress?myIP=10.0.0.5&myPort=3658&version=1.0", nil)
		r.RemoteAddr = netip.AddrPortFrom(ip, 4242).String()
		r.AddCookie(&http.Cookie{Name: "sessionID", Value: name})
		w := httptest.NewRecorder()
		d.ServeHTTP(w, r)
		return w.Body.String()
	}
	if result := lookup(netip.MustParseAddr("198.51.100.3"), "host"); result != "status=failure\n" {
		t.Fatalf("unadmitted address got %q", result)
	}
	probe := func(ip netip.Addr) string {
		out := lookup(ip, "host")
		_, tag, ok := strings.Cut(out, "tag-0=")
		if !ok || !strings.Contains(out, "targetIP-0=192.0.2.10\ntargetPort-0=3658") {
			t.Fatalf("invalid probe response: %q", out)
		}
		tag, _, _ = strings.Cut(tag, "\n")
		return "sourceIP=10.0.0.5\r\nsourcePort=3658\r\ntag=" + tag + "\r\n"
	}
	hostProbe, guestProbe := probe(host), probe(guest)
	hostUDP := netip.AddrPortFrom(host, 41234)
	guestUDP := netip.AddrPortFrom(guest, 42345)
	if !d.Probe(hostUDP, []byte(hostProbe)) || !d.Probe(hostUDP, []byte(guestProbe)) {
		t.Fatal("valid/mismatched tags both need to be consumed")
	}
	if got := lookup(host, "host"); !strings.HasPrefix(got, "status=probe\n") {
		t.Fatal("accepted a probe from the wrong address", got)
	}
	d.Probe(guestUDP, []byte(guestProbe))
	d.Probe(netip.AddrPortFrom(guest, 54321), []byte(guestProbe))
	if got := lookup(host, "host"); got != "status=success\npeerIP=198.51.100.2\npeerPort=42345\n" {
		t.Fatalf("host observed result: %q", got)
	}
	if got := lookup(guest, "host"); got != "status=success\npeerIP=198.51.100.1\npeerPort=41234\n" {
		t.Fatalf("guest observed result: %q", got)
	}
	cancel()
	if result := lookup(host, "host"); result != "status=failure\n" {
		t.Fatal("removed session remained visible", result)
	}
	if d.Probe(hostUDP, []byte{0, 1, 2}) {
		t.Fatal("opaque game datagram was consumed")
	}
}
