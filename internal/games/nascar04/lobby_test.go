package nascar04

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dead-game-entrepreneurs/rei-nation/internal/game"
	"github.com/dead-game-entrepreneurs/rei-nation/internal/protocol/aries"
	"github.com/dead-game-entrepreneurs/rei-nation/internal/storage"
)

type testStore struct {
	mu       sync.Mutex
	accounts map[string]storage.Account
	personas map[string]testPersona
}

type testPersona struct {
	owner int64
	storage.Persona
}

func (s *testStore) CreateAccount(_ context.Context, a storage.Account) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := strings.ToLower(a.Name)
	if _, exists := s.accounts[key]; exists {
		return storage.ErrConflict
	}
	a.ID = int64(len(s.accounts)) + 1
	s.accounts[key] = a
	return nil
}

func (s *testStore) Account(_ context.Context, name string) (storage.Account, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.accounts[strings.ToLower(name)]
	return a, ok, nil
}

func (s *testStore) CreatePersona(_ context.Context, account int64, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.personas[name]; exists {
		return storage.ErrConflict
	}
	s.personas[name] = testPersona{owner: account, Persona: storage.Persona{ID: int64(len(s.personas)) + 1, Name: name}}
	return nil
}

func (s *testStore) Personas(_ context.Context, account int64) ([]storage.Persona, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []storage.Persona
	for _, persona := range s.personas {
		if persona.owner == account {
			out = append(out, persona.Persona)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

type testClient struct {
	t *testing.T
	c net.Conn
}

// Synthetic SSC2 encoding of "pass" from the upstream codec test.
const testPassword = "~Kbx&4f|xq>a[zLz0Z%7F%25Rs9[He@ydyVb"

func testPersonaSuffix() string {
	suffix := strconv.FormatInt(time.Now().UnixNano(), 36)
	return suffix[len(suffix)-10:]
}

func dial(t *testing.T, addr string) testClient {
	return dialFrom(t, addr, "")
}

func dialFrom(t *testing.T, addr, source string) testClient {
	d := net.Dialer{Timeout: 3 * time.Second}
	if source != "" {
		d.LocalAddr = &net.TCPAddr{IP: net.ParseIP(source)}
	}
	c, err := d.Dial("tcp4", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return testClient{t, c}
}

func (c testClient) send(tag string, f map[string]string) {
	c.t.Helper()
	c.sendFrame(aries.Reply(tag, f))
}

func (c testClient) sendFrame(f aries.Frame) {
	c.t.Helper()
	c.c.SetWriteDeadline(time.Now().Add(3 * time.Second))
	if err := f.Write(c.c); err != nil {
		c.t.Fatal(err)
	}
}

func (c testClient) expect(tag string, fields map[string]string) aries.Frame {
	c.t.Helper()
	c.c.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		f, err := aries.Read(c.c)
		if err != nil {
			c.t.Fatalf("waiting for %s %v: %v", tag, fields, err)
		}
		name := f.Tag
		if f.Code != [4]byte{} {
			name += string(f.Code[:])
		}
		if name != tag {
			continue
		}
		got := f.Fields()
		matches := true
		for k, v := range fields {
			if actual, ok := got[k]; !ok || actual != v {
				matches = false
			}
		}
		if matches {
			return f
		}
	}
}

func (c testClient) expectRosterUntil(tag, self string, peers ...string) {
	c.t.Helper()
	seen := make(map[string]bool)
	c.c.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		f, err := aries.Read(c.c)
		if err != nil {
			c.t.Fatalf("waiting for %s: %v", tag, err)
		}
		switch f.Tag {
		case "+who":
			if f.Fields()["N"] != self {
				c.t.Fatalf("received another driver's identity before %s", tag)
			}
		case "+usr":
			name := f.Fields()["N"]
			if name == self {
				c.t.Fatalf("received own selectable roster row before %s", tag)
			}
			seen[name] = true
		}
		if f.Tag == tag {
			for _, name := range peers {
				if !seen[name] {
					c.t.Fatalf("missing roster row for %s before %s", name, tag)
				}
			}
			return
		}
	}
}

func TestLobbyFlow(t *testing.T) {
	addr := os.Getenv("REI_NATION_TEST_ADDR")
	public := os.Getenv("REI_NATION_TEST_PUBLIC_IP")
	if public == "" {
		public = "127.0.0.1"
	}
	if addr == "" {
		ln, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		t.Cleanup(func() {
			cancel()
			if err := <-done; err != nil {
				t.Error(err)
			}
		})
		store := &testStore{accounts: map[string]storage.Account{}, personas: map[string]testPersona{}}
		h := Definition().New(game.Options{
			PublicIP: netip.MustParseAddr(public), TCPPort: ln.Addr().(*net.TCPAddr).AddrPort().Port(),
			Store: store, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		})
		addr = ln.Addr().String()
		go func() { done <- aries.Serve(ctx, ln, h) }()
	}
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	const pass = testPassword
	suffix := testPersonaSuffix()
	alice, bob := "A"+suffix, "B"+suffix
	buddyAddr := os.Getenv("REI_NATION_TEST_BUDDY_ADDR")
	relayAddr := os.Getenv("REI_NATION_TEST_RELAY_ADDR")
	var sources [2]string
	if relayAddr != "" {
		sources = [2]string{"127.0.0.2", "127.0.0.3"}
	}
	host, guest := dialFrom(t, addr, sources[0]), dialFrom(t, addr, sources[1])
	host.send("@dir", map[string]string{"SLUS": "BASLUS-20824"})
	host.expect("@dir", map[string]string{"ADDR": public, "PORT": port})
	host.send("skey", nil)
	host.expect("skey", map[string]string{"SKEY": "$" + aries.SSC2Key})
	host.send("news", map[string]string{"NAME": "0"})
	bootstrap := host.expect("newsnew0", nil).Fields()
	if buddyAddr != "" {
		if bootstrap["BUDDY_URL"] != public || bootstrap["BUDDY_PORT"] != "5000" {
			t.Fatal("startup News document is missing the Messenger endpoint")
		}
	}
	host.send("news", map[string]string{"NAME": "1"})
	news := host.expect("newsnew1", map[string]string{
		"NEWS": "Welcome back to NASCAR Thunder 2004!", "TIP": "Create or join a room to race with a friend.",
	}).Fields()
	if len(news) != 2 {
		t.Fatalf("Online News includes unexpected fields: %v", news)
	}
	host.send("auth", map[string]string{"NAME": alice, "PASS": pass})
	host.expect("authimst", nil)
	var keys, rosterIDs [2]string
	for i, c := range []testClient{host, guest} {
		name := []string{alice, bob}[i]
		c.send("acct", map[string]string{"NAME": name, "PASS": pass})
		c.expect("acct", nil)
		c.send("auth", map[string]string{"NAME": name, "PASS": pass})
		c.expect("auth", map[string]string{"NAME": name})
		c.send("cper", map[string]string{"PERS": name})
		c.expect("cper", nil)
		if i == 1 {
			c.send("pers", map[string]string{"PERS": alice})
			c.expect("persmaut", nil)
		}
		c.send("pers", map[string]string{"PERS": name})
		keys[i] = c.expect("pers", map[string]string{"PERS": name}).Fields()["LKEY"]
		rosterIDs[i] = c.expect("+who", map[string]string{"N": name, "F": ""}).Fields()["I"]
		c.sendFrame(aries.Frame{Tag: "sele", Body: "ROOMS=1 USERS=1 RANKS=1 MESGS=1"})
		c.expect("sele", nil)
		for _, division := range []string{"B.Rookie Race", "E.Open Race", "W.Open Race"} {
			c.expect("+rom", map[string]string{"N": division, "T": "0"})
		}
	}
	checkPublicChat(t, host, guest, alice, "\"Lobby chat is ready\"")
	host.send("move", map[string]string{"NAME": "\"E.Open Race\""})
	host.expect("move", map[string]string{"IDENT": "2", "COUNT": "1"})
	guest.expect("+rom", map[string]string{"N": "E.Open Race", "T": "1"})
	peek := func(name, id, count string) {
		guest.send("peek", map[string]string{"NAME": "\"" + name + "\""})
		guest.expect("+who", map[string]string{"N": bob, "RI": id, "R": name, "RM": name, "RT": count})
		if count == "1" {
			guest.expect("+usr", map[string]string{"N": alice, "RM": name, "T": count})
		}
		guest.expect("peek", map[string]string{"NAME": name, "IDENT": id, "COUNT": count})
	}
	peek("E.Open Race", "2", "1")
	peek("W.Open Race", "3", "0")
	peek("E.Open Race", "2", "1")
	host.send("move", map[string]string{"IDENT": "0"})
	host.expect("move", nil)
	guest.expect("+usr", map[string]string{"I": rosterIDs[0]})
	guest.expect("+rom", map[string]string{"N": "E.Open Race", "T": "0"})
	guest.send("peek", map[string]string{"NAME": "\"E.Open Race\""})
	guest.expect("peek", map[string]string{"NAME": "E.Open Race", "COUNT": "0"})

	guest.send("move", map[string]string{"NAME": "LVL.1"})
	guest.expect("move", map[string]string{"IDENT": "1"})
	guest.expect("+who", map[string]string{"N": bob, "RI": "1"})
	peek("E.Open Race", "2", "0")
	guest.send("move", map[string]string{"NAME": "LVL.1"})
	guest.expect("move", map[string]string{"IDENT": "1"})
	guest.expect("+who", map[string]string{"N": bob, "RI": "1", "R": "LVL.1"})
	guest.send("move", map[string]string{"IDENT": "0"})
	guest.expect("move", nil)

	room := "C." + suffix
	host.send("room", map[string]string{"NAME": room, "MAX": "4", "DESC": "Test"})
	info := host.expect("room", map[string]string{"COUNT": "1", "FLAGS": "A"}).Fields()
	host.send("qdef", nil)
	host.expectRosterUntil("qdef", alice)
	guest.expect("+rom", map[string]string{"N": room, "I": info["IDENT"]})
	guest.sendFrame(aries.Frame{Tag: "sele", Body: "ROOMS=1 USERS=1 RANKS=1 MESGS=1"})
	guest.expect("sele", nil)
	guest.expect("+rom", map[string]string{"N": room, "I": info["IDENT"], "T": "1"})
	guest.send("peek", map[string]string{"NAME": room})
	guest.expectRosterUntil("peek", bob, alice)
	host.send("mesg", map[string]string{"TEXT": "\"Room members only\""})
	host.expect("mesg", nil)
	host.expect("+msg", nil)
	guest.send("move", map[string]string{"NAME": room})
	guest.c.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		frame, err := aries.Read(guest.c)
		if err != nil || frame.Tag == "+msg" {
			t.Fatalf("expected room join without prior room chat, got %s (%v)", frame.Tag, err)
		}
		if frame.Tag == "move" {
			fields := frame.Fields()
			if fields["IDENT"] != info["IDENT"] || fields["COUNT"] != "2" || fields["NAME"] != room {
				t.Fatalf("unexpected room join %v", fields)
			}
			break
		}
	}
	guest.expect("+who", map[string]string{"N": bob, "RI": info["IDENT"]})
	host.send("qdef", nil)
	host.expectRosterUntil("qdef", alice, bob)
	guest.send("qdef", nil)
	guest.expectRosterUntil("qdef", bob, alice)
	guest.send("peek", map[string]string{"NAME": room})
	guest.expectRosterUntil("peek", bob, alice)
	checkPublicChat(t, guest, host, bob, "\"Room chat is 100%25 ready\"")

	host.send("auxi", map[string]string{"TEXT": "race=proposal"})
	host.expect("auxi", map[string]string{"TEXT": "race=proposal"})
	guest.expect("+usr", map[string]string{"N": alice, "X": "race=proposal"})
	guest.send("qdef", nil)
	guest.expectRosterUntil("qdef", bob)
	expectHostChallenge := func(text string) {
		t.Helper()
		want := "F=3P\tN=" + alice + "\tT=" + text + "\tU="
		if got := guest.expect("+msg", nil).Body; got != want {
			t.Fatalf("challenge message = %q, want %q", got, want)
		}
	}
	host.send("mesg", map[string]string{"TEXT": "1eu9zm_a0dxw_28", "PRIV": bob, "ATTR": "3"})
	host.expect("mesg", nil)
	expectHostChallenge("1eu9zm_a0dxw_28")
	host.send("chal", map[string]string{"PERS": "*"})
	host.expect("chal", nil)
	expectHostChallenge("DECL")

	host.send("mesg", map[string]string{"TEXT": "1eu9zm_a0dxw_28", "PRIV": bob, "ATTR": "3"})
	host.expect("mesg", nil)
	if frame := guest.expect("+msg", nil); frame.Body != "F=3P\tN="+alice+"\tT=1eu9zm_a0dxw_28\tU=" {
		t.Fatalf("unexpected race proposal %q", frame.Body)
	}
	guest.send("mesg", map[string]string{"TEXT": "ACPT", "PRIV": alice, "ATTR": "3"})
	guest.expect("mesg", nil)
	if got := host.expect("+msg", nil).Body; !strings.Contains(got, "T=ACPT") || !strings.Contains(got, "\t") {
		t.Fatalf("unexpected private message %q", got)
	}
	host.send("chal", map[string]string{"PERS": bob, "HOST": "1"})
	host.expect("chal", nil)
	guest.send("chal", map[string]string{"PERS": alice})
	guest.expect("chal", nil)
	h := host.expect("+ses", map[string]string{"SELF": alice, "OPPO": bob, "HOST": alice}).Fields()
	guest.expect("+ses", map[string]string{"SELF": bob, "OPPO": alice, "HOST": alice, "SEED": h["SEED"]})
	if relayAddr != "" {
		if h["ADDR"] != public {
			t.Fatalf("relay address=%q, expected %s", h["ADDR"], public)
		}
		testPeerServices(t, relayAddr, alice, sources)
	}
	if buddyAddr != "" {
		testMessenger(t, buddyAddr, host, alice, bob, keys, sources)
		testRankReports(t, host, guest, alice, bob, h["WHEN"])
		testReportsAndDeletion(t, host, guest, alice, bob)
	}

	host.send("mesg", map[string]string{"TEXT": "1eu9zm_a0dxw_28", "PRIV": bob, "ATTR": "3"})
	host.expect("mesg", nil)
	expectHostChallenge("1eu9zm_a0dxw_28")
	host.c.Close()
	expectHostChallenge("DECL")
	guest.expect("+rom", map[string]string{"N": room, "T": "1"})
	guest.send("move", map[string]string{"IDENT": "0"})
	if f := guest.expect("+rom", map[string]string{"I": info["IDENT"]}).Fields(); len(f) != 1 {
		t.Fatalf("room removal must carry only its ID, got %v", f)
	}
	guest.expect("move", nil)
	guest.send("move", map[string]string{"IDENT": info["IDENT"]})
	guest.expect("moveurom", nil)

	reconnect := dial(t, addr)
	reconnect.send("auth", map[string]string{"NAME": strings.ToLower(alice), "PASS": pass})
	reconnect.expect("auth", map[string]string{"PERSONAS": alice})
	reconnect.send("pers", map[string]string{"PERS": fmt.Sprintf("\"%s\"", alice)})
	reconnect.expect("pers", map[string]string{"PERS": alice})
	reconnect.expect("+who", map[string]string{"N": alice, "I": rosterIDs[0]})
	testSharedAccountRoster(t, reconnect, addr, alice, pass, suffix, rosterIDs[0])
}

func testSharedAccountRoster(t *testing.T, host testClient, addr, account, password, suffix, hostID string) {
	t.Helper()
	room, alt := "C.Shared"+suffix, "Y"+suffix
	host.send("room", map[string]string{"NAME": room})
	host.expect("room", map[string]string{"NAME": room})
	second := dial(t, addr)
	second.send("auth", map[string]string{"NAME": account, "PASS": password})
	second.expect("auth", map[string]string{"NAME": account})
	second.send("cper", map[string]string{"PERS": alt})
	second.expect("cper", nil)
	second.send("pers", map[string]string{"PERS": alt})
	second.expect("pers", map[string]string{"PERS": alt})
	altID := second.expect("+who", map[string]string{"N": alt}).Fields()["I"]
	if hostID == "" || altID == "" || hostID == altID {
		t.Fatalf("personas on the same account have conflicting room IDs %q and %q", hostID, altID)
	}
	second.send("move", map[string]string{"NAME": room})
	second.expect("move", map[string]string{"COUNT": "2"})
	second.expect("+usr", map[string]string{"N": account, "I": hostID})
	host.expect("+usr", map[string]string{"N": alt, "I": altID})
	second.c.Close()
	if fields := host.expect("+usr", map[string]string{"I": altID}).Fields(); len(fields) != 1 {
		t.Fatalf("expected persona removal, got %v", fields)
	}
}

func checkPublicChat(t *testing.T, sender, recipient testClient, name, text string) {
	t.Helper()
	sender.send("mesg", map[string]string{"TEXT": text})
	sender.expect("mesg", nil)
	for _, c := range []testClient{sender, recipient} {
		want := aries.TabReply("+msg", map[string]string{"F": "Z", "N": name, "T": text, "U": ""})
		if got := c.expect("+msg", nil); got != want {
			t.Fatalf("chat payload = %q, want %q", got.Body, want.Body)
		}
	}
}

func testRankReports(t *testing.T, host, guest testClient, alice, bob, when string) {
	t.Helper()
	result := map[string]string{
		"SET_RANKED": "1", "WHEN": when, "RACETIME": "2660", "NAME0": alice,
		"NAME1": alice, "FINISPOS0": "1", "FINISPOS1": "42", "STARTPOS0": "1",
		"LAPSCOMP0": "15", "LAPSLED0": "4", "SET_TRACKID": "13", "SET_TRACK": "TALLADEGA%b1",
	}
	guest.send("rank", result)
	guest.expect("rank", nil)
	guest.send("snap", map[string]string{"CHAN": "4"})
	guest.expect("snap", map[string]string{"RANGE": "0"})
	result["NAME1"] = bob
	host.send("rank", result)
	host.expect("rank", nil)
	host.expect("+who", map[string]string{"N": alice, "S": ",1,0,,0,,,,,"})
	host.send("qdef", nil)
	host.expectRosterUntil("qdef", alice, bob)
	guest.expect("+who", map[string]string{"N": bob, "S": ",0,1,,0,,,,,"})
	guest.send("qdef", nil)
	guest.expectRosterUntil("qdef", bob)
	result["RACETIME"] = "2661" // even a changed report cannot count the same race twice
	guest.send("rank", result)
	guest.expect("rank", nil)
	guest.send("snap", map[string]string{"CHAN": "5", "RANGE": "100"})
	guest.expect("snap", map[string]string{"CC": "24"})
	fields := guest.expect("+snp", map[string]string{"N": alice, "P": "10"}).Fields()
	columns := strings.Split(fields["S"], ",")
	if len(columns) != 24 || columns[2] != "a" || columns[4] != "1" || columns[8] != "f" || columns[9] != "4" ||
		columns[10] != "1" || columns[12] != "64" || columns[14] != "1" {
		t.Fatalf("unexpected winner row %v", columns)
	}
	guest.send("snap", map[string]string{"CHAN": "4"})
	guest.expect("snap", map[string]string{"RANGE": "1"})
	fields = guest.expect("+snp", map[string]string{"N": bob, "P": "0"}).Fields()
	columns = strings.Split(fields["S"], ",")
	if len(columns) != 24 || columns[10] != "1" || columns[15] != "1" {
		t.Fatalf("unexpected runner-up row %v", columns)
	}
	guest.send("snap", map[string]string{"CHAN": "4", "INDEX": "0", "FIND": strings.ToLower(alice)})
	guest.expect("snap", map[string]string{"RANGE": "1"})
	find := guest.expect("+snp", map[string]string{"N": alice}).Fields()
	guest.send("snap", map[string]string{"CHAN": "7", "FIND": alice})
	guest.expect("snap", map[string]string{"CHAN": "7", "RANGE": "1"})
	guest.expect("+snp", map[string]string{"N": alice, "R": find["R"]})
	guest.send("snap", map[string]string{"CHAN": "6", "INDEX": "23", "FIND": alice})
	guest.expect("snap", map[string]string{"CHAN": "6", "INDEX": "23", "RANGE": "1"})
	guest.expect("+snp", map[string]string{"N": alice})
	guest.send("snap", map[string]string{"CHAN": "5", "INDEX": "33"})
	guest.expect("snap", map[string]string{"INDEX": "33", "CC": "1"})
	guest.expect("+snp", map[string]string{"N": "13"})

	host.send("chal", map[string]string{"PERS": bob, "HOST": "1"})
	host.expect("chal", nil)
	guest.send("chal", map[string]string{"PERS": alice})
	guest.expect("chal", nil)
	rematch := host.expect("+ses", nil).Fields()
	guest.expect("+ses", nil)
	host.send("rank", map[string]string{
		"SET_RANKED": "1", "WHEN": rematch["WHEN"], "NAME0": alice, "NAME1": bob,
		"STARTPOS0": "2", "STARTPOS1": "1", "DISCTIME0": "0", "DISCTIME1": "123",
	})
	host.expect("rank", nil)
	host.expect("+who", map[string]string{"N": alice, "S": ",2,0,,0,,,,,"})
	guest.expect("+who", map[string]string{"N": bob, "S": ",0,2,,32,,,,,"})
	guest.send("snap", map[string]string{"CHAN": "4", "FIND": bob})
	guest.expect("snap", map[string]string{"RANGE": "1"})
	columns = strings.Split(guest.expect("+snp", map[string]string{"N": bob}).Fields()["S"], ",")
	if columns[10] != "1" || columns[15] != "2" || columns[16] != "-2" || columns[17] != "1" {
		t.Fatalf("unexpected abort row %v", columns)
	}
	host.send("snap", map[string]string{"CHAN": "6", "INDEX": "2", "FIND": alice})
	host.expect("snap", map[string]string{"RANGE": "1"})
	columns = strings.Split(host.expect("+snp", map[string]string{"N": alice}).Fields()["S"], ",")
	if columns[14] != "1" || columns[15] != "0" || columns[13] == "0" {
		t.Fatalf("unexpected top 100 record %v", columns)
	}
}

func testReportsAndDeletion(t *testing.T, host, guest testClient, alice, bob string) {
	t.Helper()
	host.send("rept", map[string]string{"PERS": strings.ToLower(bob)})
	host.expect("+usr", map[string]string{"N": bob, "RP": "4"})
	host.expect("rept", nil)
	guest.expect("+who", map[string]string{"N": bob, "RP": "4"})
	guest.send("qdef", nil)
	guest.expectRosterUntil("qdef", bob)
	host.send("rept", map[string]string{"PERS": bob})
	host.expect("rept", nil)
	guest.send("llvl", nil)
	guest.expect("llvl", nil)
	guest.expect("+who", map[string]string{"N": bob, "RP": "4"})

	extra := "X" + alice[1:]
	host.send("cper", map[string]string{"PERS": extra})
	host.expect("cper", nil)
	guest.send("dper", map[string]string{"PERS": extra})
	guest.expect("dperiper", nil)
	host.send("dper", map[string]string{"PERS": extra})
	host.expect("dper", map[string]string{"PERS": extra})
	host.send("pers", map[string]string{"PERS": extra})
	host.expect("persmaut", nil)
}

func (c testClient) syncMessengerRoster() {
	c.t.Helper()
	c.send("RGET", map[string]string{"ID": "1", "LIST": "B"})
	c.expect("RGET", map[string]string{"ID": "1"})
	c.send("RGET", map[string]string{"ID": "2", "LIST": "I"})
	c.expect("RGET", map[string]string{"ID": "2"})
}

func (c testClient) expectNoMessengerMail() {
	c.t.Helper()
	c.send("EPGT", map[string]string{"ID": "1"})
	c.c.SetReadDeadline(time.Now().Add(3 * time.Second))
	for {
		frame, err := aries.Read(c.c)
		if err != nil || frame.Tag == "RECV" {
			c.t.Fatalf("unexpected Messenger frame %q (%v)", frame.Tag, err)
		}
		if frame.Tag == "EPGT" {
			return
		}
	}
}

func testMessenger(t *testing.T, addr string, lobby testClient, alice, bob string, keys, sources [2]string) {
	t.Helper()
	lobby.send("user", map[string]string{"PERS": strings.ToLower(bob)})
	lobby.expect("user", map[string]string{"PERS": bob, "NAME": bob})
	host, guest := dialFrom(t, addr, sources[0]), dialFrom(t, addr, sources[1])
	host.send("AUTH", map[string]string{"LKEY": strings.Repeat("0", 51), "NAME": alice})
	host.expect("AUTHfail", nil)
	for i, c := range []testClient{host, guest} {
		name := []string{alice, bob}[i]
		// The retail client first formats "persona:$LKEY" in a 64-byte buffer.
		keys[i] = keys[i][:min(len(keys[i]), 61-len(name))]
		c.send("AUTH", map[string]string{"LKEY": keys[i], "PROD": "TEST", "PRES": "CHAT", "VERS": "1.0"})
		c.expect("AUTH", map[string]string{"TITL": "TEST"})
		c.syncMessengerRoster()
	}
	lobby.send("pers", map[string]string{"PERS": alice})
	refreshed := lobby.expect("pers", map[string]string{"PERS": alice}).Fields()
	if !strings.HasPrefix(refreshed["LKEY"], keys[0]) {
		t.Fatal("repeat persona selection changed the active Messenger key")
	}
	host.send("SEND", map[string]string{"USER": bob, "TYPE": "C", "BODY": "\"Hello racer\"", "SECS": "259200"})
	host.expect("SEND", nil)
	original := guest.expect("RECV", map[string]string{"USER": alice, "TYPE": "C", "BODY": "\"Hello racer\""}).Fields()

	host.send("USCH", map[string]string{"ID": "search", "USER": bob})
	host.expect("USCH", map[string]string{"ID": "search", "SIZE": "1"})
	host.expect("ROST", map[string]string{"ID": "search", "USER": bob, "GROUP": ""})
	host.c.SetReadDeadline(time.Now().Add(7 * time.Second))
	if ping, err := aries.Read(host.c); err != nil || ping.Tag != "PING" {
		t.Fatalf("expected Messenger keepalive, got %q (%v)", ping.Tag, err)
	}
	host.send("PING", nil)
	host.send("RADD", map[string]string{"ID": "2", "USER": bob, "LIST": "B", "PRES": "Y"})
	host.expect("RADD", map[string]string{"FUSR": bob, "ID": "2"})
	guest.expect("ROST", map[string]string{"USER": alice, "ATTR": "T", "ID": "1"})
	host.expect("PGET", map[string]string{"USER": bob, "SHOW": "CHAT", "PROD": "TEST", "TITL": "1.0"})
	guest.expect("PGET", map[string]string{"USER": alice, "SHOW": "CHAT", "PROD": "TEST"})
	guest.expect("RECV", map[string]string{"USER": alice, "TYPE": "C", "BODY": buddyNotice})
	host.send("RADD", map[string]string{"ID": "3", "USER": bob, "LIST": "B", "PRES": "Y"})
	host.expect("RADD", map[string]string{"FUSR": bob, "ID": "3"})
	guest.expectNoMessengerMail()
	guest.send("PSET", map[string]string{"SHOW": "AWAY", "PROD": "LIVE"})
	guest.expect("PSET", nil)
	host.expect("PGET", map[string]string{"USER": bob, "SHOW": "AWAY", "PROD": "LIVE"})
	host.send("RGET", map[string]string{"ID": "4", "LIST": "B"})
	host.expect("RGET", map[string]string{"ID": "4", "SIZE": "1"})
	host.expect("ROST", map[string]string{"USER": bob, "ATTR": "T", "ID": "4"})
	host.send("SEND", map[string]string{"USER": bob, "BODY": "race=ready"})
	host.expect("SEND", nil)
	guest.expect("RECV", map[string]string{"USER": alice, "BODY": "race=ready", "TYPE": "C", "SIZE": "10"})
	guest.send("PSET", map[string]string{"SHOW": "PASS"})
	guest.expect("PSET", nil)
	host.expect("PGET", map[string]string{"USER": bob, "SHOW": "PASS"})
	host.send("SEND", map[string]string{"USER": bob, "BODY": "during race"})
	host.expect("SEND", nil)
	guest.expectNoMessengerMail()
	guest.send("PSET", map[string]string{"SHOW": "CHAT"})
	guest.expect("PSET", nil)
	guest.expectNoMessengerMail()
	guest.syncMessengerRoster()
	for _, body := range []string{`"Hello racer"`, "race=ready", "during race"} {
		guest.expect("RECV", map[string]string{"USER": alice, "BODY": body, "TYPE": "C"})
	}
	guest.expectNoMessengerMail()
	guest.send("RDEL", map[string]string{"ID": "21", "USER": alice, "LIST": "B"})
	guest.expect("RDEL", map[string]string{"FUSR": alice, "ID": "21"})
	for _, client := range []testClient{host, guest} {
		client.send("RGET", map[string]string{"ID": "1", "LIST": "B"})
		client.expect("RGET", map[string]string{"ID": "1", "SIZE": "0"})
	}
	guest.send("DISC", nil)
	guest.c.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.Copy(io.Discard, guest.c); err != nil {
		t.Fatal(err)
	}
	guest.c.Close()
	host.send("RADD", map[string]string{"ID": "23", "USER": bob, "LIST": "B", "PRES": "Y"})
	host.expect("RADD", map[string]string{"FUSR": bob, "ID": "23"})
	host.expect("PGET", map[string]string{"USER": bob, "SHOW": "CHAT", "PROD": lobbyPresenceProduct})
	host.send("SEND", map[string]string{"USER": bob, "BODY": "offline"})
	host.expect("SEND", nil)
	host.send("RDEL", map[string]string{"ID": "24", "USER": bob, "LIST": "B"})
	host.expect("RDEL", map[string]string{"FUSR": bob, "ID": "24"})
	host.send("RADD", map[string]string{"ID": "25", "USER": bob, "LIST": "B", "PRES": "Y"})
	host.expect("RADD", map[string]string{"FUSR": bob, "ID": "25"})
	guest = dialFrom(t, addr, sources[1])
	guest.send("AUTH", map[string]string{"LKEY": keys[1]})
	guest.expect("AUTH", nil)
	guest.expectNoMessengerMail()
	guest.syncMessengerRoster()
	for _, body := range []string{`"Hello racer"`, "race=ready", "during race", "offline", buddyNotice} {
		got := guest.expect("RECV", nil).Fields()
		if got["BODY"] != body || got["TYPE"] != "C" || got["USER"] != alice {
			t.Fatalf("expected %q from %s on fresh Messenger sign-in, got %v", body, alice, got)
		}
		if body == original["BODY"] && got["TIME"] != original["TIME"] {
			t.Fatal("replayed Messenger message changed its original timestamp")
		}
	}
	guest.send("AUTH", map[string]string{"LKEY": keys[1]})
	guest.expect("AUTH", nil)
	guest.expectNoMessengerMail()
	guest.send("RGET", map[string]string{"ID": "1", "LIST": "B"})
	guest.expect("RGET", map[string]string{"ID": "1", "SIZE": "1"})
	guest.expect("ROST", map[string]string{"USER": alice, "ATTR": "T", "ID": "1"})
	guest.send("RADD", map[string]string{"ID": "5", "USER": alice, "LIST": "I"})
	guest.expect("RADD", map[string]string{"FUSR": alice})
	host.send("RADD", map[string]string{"ID": "26", "USER": bob, "LIST": "B", "PRES": "Y"})
	host.expect("RADDfail", map[string]string{"ID": "26"})
	host.send("RGET", map[string]string{"ID": "1", "LIST": "B"})
	host.expect("RGET", map[string]string{"ID": "1", "SIZE": "0"})
	host.send("SEND", map[string]string{"USER": bob, "BODY": "blocked"})
	host.expect("SENDfail", nil)
}
