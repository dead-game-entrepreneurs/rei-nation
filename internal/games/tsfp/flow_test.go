package tsfp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dead-game-entrepreneurs/rei-nation/internal/game"
	"github.com/dead-game-entrepreneurs/rei-nation/internal/protocol/aries"
	"github.com/dead-game-entrepreneurs/rei-nation/internal/storage"
)

func TestFESLFragments(t *testing.T) {
	in := aries.Frame{Tag: "blob", Code: [4]byte{0xc0, 0, 0, 9}}
	want := strings.Repeat("=percent% ", 500)
	out := feslFrames(in, "TXN", "GetBlobContent", "content", want)
	if len(out) < 2 {
		t.Fatal("large response was not fragmented")
	}
	var joined strings.Builder
	for _, part := range out {
		if binary.BigEndian.Uint32(part.Code[:]) != 0xf0000009 {
			t.Fatalf("unexpected fragment code: %x", part.Code)
		}
		joined.WriteString(fields(part)["DATA"])
	}
	data, err := base64.StdEncoding.DecodeString(joined.String())
	if err != nil {
		t.Fatal(err)
	}
	s := &feslSession{}
	encoded := joined.String()
	total := strconv.Itoa(len(encoded))
	for len(encoded) > 0 {
		n := min(len(encoded), fragmentLimit)
		frame := packet("blob", "size", total, "data", encoded[:n])
		frame.Code = out[0].Code
		encoded = encoded[n:]
		decoded, ready, err := s.receive(frame)
		if err != nil || ready != (encoded == "") {
			t.Fatalf("reassemble: ready=%v err=%v", ready, err)
		}
		if ready && (decoded.Body != strings.TrimSuffix(string(data), "\x00") ||
			decoded.Code != in.Code || fields(decoded)["CONTENT"] != want) {
			t.Fatal("fragment reassembly changed the transaction")
		}
	}
}

func TestRankFragmentBudget(t *testing.T) {
	pairs := []string{"stats.[]", "16"}
	for i := range 16 {
		prefix := "stats." + strconv.Itoa(i) + "."
		pairs = append(pairs, prefix+"key", "S_123456789", prefix+"value", "123456789012345678901234.1234",
			prefix+"rank", strconv.Itoa(i+1), prefix+"text", strings.Repeat("%", 512),
			prefix+"owner", "2147483647", prefix+"name", strings.Repeat("P", 32))
		for j := range 12 {
			detail := prefix + "addStats." + strconv.Itoa(j) + "."
			pairs = append(pairs, detail+"key", "S_123456789", detail+"value", "123456789012345678901234.1234",
				detail+"text", strings.Repeat("%", 512))
		}
	}
	if frames := feslFrames(packet("rank"), pairs...); len(frames) > 240 {
		t.Fatalf("maximum admitted ranking reply needs %d fragments", len(frames))
	}
}

func TestRelayProbeTargetChangeClearsStaleReachability(t *testing.T) {
	theater := newTheater(&loginKeys{keys: make(map[string]sessionKey)}, netip.MustParseAddr("127.0.0.22"), "auto")
	for range cap(theater.probeSlots) {
		theater.probeSlots <- struct{}{}
	}
	old := netip.MustParseAddrPort("198.51.100.10:3000")
	current := netip.MustParseAddrPort("203.0.113.20:3000")
	game := &match{external: current, relayProbeAddr: old, relayProbedAt: time.Now(), relayReachable: true}
	theater.probeRelay(game)
	if game.relayReachable || game.relayProbeAddr != current || !game.relayProbedAt.IsZero() {
		t.Fatalf("stale probe survived target change: reachable=%v target=%s probed=%s",
			game.relayReachable, game.relayProbeAddr, game.relayProbedAt)
	}
}

func TestRelayRouteSelection(t *testing.T) {
	host := netip.MustParseAddr("198.51.100.10")
	guest := netip.MustParseAddr("203.0.113.20")
	for _, test := range []struct {
		name                       string
		mode                       string
		available, reachable, same bool
		want                       relayRoute
	}{
		{"off", "off", true, true, false, direct},
		{"auto unprobed", "auto", true, false, false, direct},
		{"auto verified", "auto", true, true, false, forcedRelay},
		{"always distinct", "always", true, false, false, forcedRelay},
		{"always same address", "always", true, true, true, direct},
		{"unavailable", "always", false, true, false, direct},
	} {
		t.Run(test.name, func(t *testing.T) {
			peer := guest
			if test.same {
				peer = host
			}
			if got := relayRouteFor(test.mode, test.available, test.reachable, host, peer); got != test.want {
				t.Fatalf("route=%v, want %v", got, test.want)
			}
		})
	}
}

func TestMapNameKeyMatchesLegacyCaseFold(t *testing.T) {
	if got := mapNameKey("  Straße   Arena "); got != "strasse arena" {
		t.Fatalf("map key %q", got)
	}
}

func TestLoginKeysAreBoundedPerPersona(t *testing.T) {
	keys := &loginKeys{keys: make(map[string]sessionKey)}
	persona := storage.Persona{ID: 1, Name: "Player"}
	issued := make([]string, maxPersonaLoginKeys+2)
	for i := range issued {
		issued[i] = keys.issue(persona)
	}
	if len(keys.keys) != maxPersonaLoginKeys {
		t.Fatalf("retained %d keys", len(keys.keys))
	}
	if _, ok := keys.lookup(issued[0]); ok {
		t.Fatal("oldest persona key was not retired")
	}
	if _, ok := keys.lookup(issued[len(issued)-1]); !ok {
		t.Fatal("newest persona key was not retained")
	}
}

func TestTheaterAdmission(t *testing.T) {
	ln := loopbackListener(t)
	keys := &loginKeys{keys: make(map[string]sessionKey)}
	theater := newTheater(keys, netip.Addr{}, "off")
	runListener(t, ln, theater)
	host, guest, next := dialWire(t, ln), dialWire(t, ln), dialWire(t, ln)
	for i, c := range []testWire{host, guest, next} {
		name := "Player" + strconv.Itoa(i)
		key := keys.issue(storage.Persona{ID: int64(i + 1), Name: name})
		c.send("CONN", "TID", "1")
		c.expect("CONN", "PROT", "2")
		c.send("USER", "TID", "2", "LKEY", key)
		c.expect("USER", "NAME", name)
	}
	host.send("CGAM", "TID", "3", "LID", "1", "MAX-PLAYERS", "2")
	gid := host.expect("CGAM", "MAX-PLAYERS", "2")["GID"]
	host.send("EGAM", "TID", "4", "LID", "1", "GID", gid)
	host.expect("EGAM", "TID", "4")
	host.expect("EGRQ", "PID", "1")
	host.send("EGRS", "PID", "1", "ALLOWED", "1")
	host.expect("EGEG", "PID", "1")
	host.send("PENT", "PID", "1", "TID", "5")
	host.expect("PENT", "PID", "1")
	guest.send("EGAM", "TID", "3", "LID", "1", "GID", gid)
	guest.expect("EGAM", "TID", "3")
	first := host.expect("EGRQ", "UID", "2")["PID"]
	next.send("EGAM", "TID", "3", "LID", "1", "GID", gid)
	next.expect("EGAMjden", "REASON", "f")
	guest.send("GDAT", "TID", "4", "LID", "1", "GID", gid)
	guest.expect("GDAT", "AP", "1", "JP", "1")
	guest.expect("GDET", "GID", gid)
	guest.expect("PDAT", "PID", "1")
	host.send("EGRS", "PID", first, "ALLOWED", "1")
	guest.expect("EGEG", "PID", first)
	host.send("PENT", "PID", first, "TID", "6")
	host.expect("PENT", "PID", first)
	guest.send("ECNL", "TID", "5", "LID", "1", "GID", gid)
	host.expect("KICK", "PID", first)
	guest.expect("ECNL", "TID", "5")
	next.send("EGAM", "TID", "4", "LID", "1", "GID", gid)
	next.expect("EGAM", "TID", "4")
	host.expect("EGRQ", "PID", "3")
	host.conn.Close()
	next.expect("GREM", "GID", gid)
}

func TestTheaterEchoAndRelayPorts(t *testing.T) {
	keys := &loginKeys{keys: make(map[string]sessionKey)}
	theater := newTheater(keys, netip.Addr{}, "off")
	ln := loopbackListener(t)
	theater.udp = testUDP(t, "127.0.0.1")
	runListener(t, ln, theater)
	for i := range 2 {
		c := dialWire(t, ln)
		key := keys.issue(storage.Persona{ID: int64(i + 1), Name: "Synthetic" + strconv.Itoa(i)})
		c.send("USER", "TID", "1", "LKEY", key)
		c.expect("USER", "TID", "1")
	}
	a, b := testUDP(t, "127.0.0.1"), testUDP(t, "127.0.0.1")
	theaterAddr := theater.udp.LocalAddr().(*net.UDPAddr).AddrPort()
	aAddr, bAddr := a.LocalAddr().(*net.UDPAddr).AddrPort(), b.LocalAddr().(*net.UDPAddr).AddrPort()
	var echo bytes.Buffer
	if err := packet("ECHO", "TID", "42", "TYPE", "1").Write(&echo); err != nil {
		t.Fatal(err)
	}
	theater.echo(aAddr, echo.Bytes())
	reply, source := receiveUDP(t, a)
	frame, err := aries.Read(bytes.NewReader(reply))
	if f := fields(frame); err != nil || source != theaterAddr || frame.Tag != "ECHO" ||
		f["TYPE"] != "1" || f["TID"] != "42" || f["PORT"] != strconv.Itoa(int(aAddr.Port())) {
		t.Fatalf("unexpected echo fields: %v from %s: %v", f, source, err)
	}
	theater.mu.Lock()
	for _, session := range theater.sessions {
		if session.endpoint.IsValid() {
			t.Error("ambiguous ECHO overwrote a Theater endpoint")
		}
	}
	theater.mu.Unlock()
	room := &relayRoom{ip: netip.MustParseAddr("127.0.0.22"), members: make(map[int]*relayMember)}
	t.Cleanup(room.close)
	if err := room.add(1, aAddr); err != nil {
		t.Fatal(err)
	}
	if err := room.add(2, bAddr); err != nil {
		t.Fatal(err)
	}
	if _, err := a.WriteToUDPAddrPort([]byte("synthetic peer packet"), room.address(2)); err != nil {
		t.Fatal(err)
	}
	if data, from := receiveUDP(t, b); string(data) != "synthetic peer packet" || from != room.address(1) {
		t.Fatalf("unexpected datagram %q from %s", data, from)
	}
}

func testUDP(t *testing.T, ip string) *net.UDPConn {
	t.Helper()
	socket, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP(ip)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { socket.Close() })
	return socket
}

func receiveUDP(t *testing.T, socket *net.UDPConn) ([]byte, netip.AddrPort) {
	t.Helper()
	socket.SetReadDeadline(time.Now().Add(2 * time.Second))
	var data [512]byte
	n, from, err := socket.ReadFromUDPAddrPort(data[:])
	if err != nil {
		t.Fatal(err)
	}
	return data[:n], from
}

func TestPostgresPS2Services(t *testing.T) {
	dsn := os.Getenv("REI_NATION_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set REI_NATION_TEST_DATABASE_URL for the isolated stack")
	}
	ctx := t.Context()
	db, err := storage.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(db.Close)
	messengerListener := loopbackListener(t)
	handler := Definition("", "", "").New(game.Options{Store: db, PublicIP: netip.MustParseAddr("127.0.0.1")}).(*fesl)
	runListener(t, messengerListener, handler.messenger)
	feslListener := loopbackListener(t)
	runListener(t, feslListener, handler)
	client := dialWire(t, feslListener)
	client.send("fsys", "TXN", "Hello")
	client.expect("fsys", "TXN", "Hello", "theaterPort", "18071")
	client.expect("fsys", "TXN", "MemCheck")
	login := func(c testWire, prefix string) (name, id, key string) {
		name = prefix + token(5)
		c.send("acct", "TXN", "AddAccount", "NAME", name, "PASSWORD", "synthetic-only")
		c.expect("acct", "TXN", "AddAccount", "ERRORCODE", "")
		c.send("acct", "TXN", "Login", "NAME", name, "PASSWORD", "synthetic-only")
		accountKey := c.expect("acct", "TXN", "Login", "ERRORCODE", "")["LKEY"]
		if _, authorized := handler.keys.lookup(accountKey); authorized {
			t.Fatal("account-level key can authenticate a persona")
		}
		c.send("acct", "TXN", "AddSubAccount", "NAME", name)
		c.expect("acct", "TXN", "AddSubAccount", "ERRORCODE", "")
		c.send("acct", "TXN", "LoginSubAccount", "NAME", name)
		got := c.expect("acct", "TXN", "LoginSubAccount", "ERRORCODE", "")
		return name, got["USERID"], got["LKEY"]
	}
	alice, aliceID, aliceKey := login(client, "A")
	bob, _, bobKey := login(dialWire(t, feslListener), "B")
	sender := dialWire(t, messengerListener)
	sender.send("AUTH", "ID", "1", "LKEY", aliceKey)
	sender.expect("AUTH", "USER", alice)
	sender.send("SEND", "ID", "2", "USER", strings.ToLower(bob)+"@messaging.ea.com", "SUBJ", "Test", "BODY", "first offline")
	sender.expect("SEND", "ERRS", "0")
	recipient := dialWire(t, messengerListener)
	recipient.send("AUTH", "ID", "1", "LKEY", bobKey)
	recipient.expect("AUTH", "USER", bob)
	recipient.send("PSET", "ID", "2", "SHOW", "CHAT")
	recipient.expect("PSET", "ID", "2")
	recipient.expect("RECV", "USER", alice, "BODY", "first offline", "SUBJ", "Test")
	sender.send("RADM", "ID", "3", "USER", bob)
	sender.expect("RNOT", "ATTR", "T")
	sender.expect("RADM", "ID", "3")
	recipient.expect("RNOT", "ATTR", "R")
	recipient.send("RRSP", "ID", "3", "USER", alice, "ANSW", "Y")
	recipient.expect("RNOT", "CHNG", "D")
	recipient.expect("RNOT", "ATTR", "S")
	recipient.expect("PGET", "USER", alice)
	recipient.expect("RRSP", "ID", "3")
	raw := []byte("synthetic-" + token(12))
	content := base64.StdEncoding.EncodeToString(raw)
	client.send("blob", "TXN", "AddBlob", "NAME", "Map "+alice, "OWNERID", aliceID, "OWNERTYPE", "1",
		"TYPE", "2", "SIZE", strconv.Itoa(len(raw)), "CONTENT", content)
	id := client.expect("blob", "TXN", "AddBlob", "ERRORCODE", "")["BLOBID"]
	client.send("blob", "TXN", "GetBlobContent", "BLOBID", id)
	client.expect("blob", "CONTENT", content, "SIZE", strconv.Itoa(len(content)))
	client.send("blob", "TXN", "UpdateBlobRating", "BLOBID", id, "RATING", "4")
	client.expect("blob", "ERRORCODE", "")
	client.send("blob", "TXN", "GetBlobInfo", "BLOBID", id)
	client.expect("blob", "RATING", "4.0000", "DOWNLOADCOUNT", "1")
	client.send("blob", "TXN", "ListBlobInfo", "NAME", "Map "+alice, "MAXRECORDS", "1")
	client.expect("blob", "BLOBS.0.BLOBID", id, "BLOBS.[]", "1")
	client.send("fdbk", "TXN", "SendFeedback", "ORIGINATORUSERID", aliceID, "TARGETTYPE", "3",
		"TARGETIDS.[]", "1", "TARGETIDS.0", id, "FEEDBACKTYPE", "1")
	client.expect("fdbk", "ERRORCODE", "")
	statID := aliceID
	if len(statID) > 9 {
		statID = statID[len(statID)-9:]
	}
	statKey := "S_" + statID
	for range 2 {
		client.send("rank", "TXN", "UpdateStats", "U.[]", "1", "U.0.OT", "1", "U.0.O", aliceID, "U.0.S.[]", "1",
			"U.0.S.0.K", statKey, "U.0.S.0.UT", "3", "U.0.S.0.V", "1")
		client.expect("rank", "ERRORCODE", "")
	}
	client.send("rank", "TXN", "GetTopN", "KEY", statKey)
	client.expect("rank", "STATS.0.NAME", alice, "STATS.0.VALUE", "1.0000", "STATS.0.RANK", "1")
	client.send("rank", "TXN", "GetStats", "KEYS.[]", "1", "KEYS.0", statKey)
	client.expect("rank", "STATS.0.NAME", alice, "STATS.0.VALUE", "1.0000", "STATS.0.RANK", "1")
}

type testWire struct {
	t    *testing.T
	conn net.Conn
}

func loopbackListener(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	return ln
}

func runListener(t *testing.T, ln net.Listener, h aries.Handler) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- aries.ServeTransport(ctx, ln, h, aries.Transport{}) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
}

func dialWire(t *testing.T, ln net.Listener) testWire {
	t.Helper()
	conn, err := net.DialTimeout("tcp4", ln.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return testWire{t, conn}
}

func (c testWire) send(tag string, pairs ...string) {
	c.t.Helper()
	c.conn.SetWriteDeadline(time.Now().Add(3 * time.Second))
	if err := packet(tag, pairs...).Write(c.conn); err != nil {
		c.t.Fatal(err)
	}
}

func (c testWire) expect(tag string, pairs ...string) map[string]string {
	c.t.Helper()
	c.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	f, err := aries.Read(c.conn)
	expected := aries.Reply(tag, nil)
	if err != nil || f.Tag != expected.Tag || !bytes.Equal(f.Code[:], expected.Code[:]) {
		c.t.Fatalf("want %s, got %s%q: %v", tag, f.Tag, f.Code, err)
	}
	got := fields(f)
	for i := 0; i < len(pairs); i += 2 {
		if got[strings.ToUpper(pairs[i])] != pairs[i+1] {
			c.t.Fatalf("%s: unexpected field %s = %q, want %q", tag, pairs[i], got[strings.ToUpper(pairs[i])], pairs[i+1])
		}
	}
	return got
}
