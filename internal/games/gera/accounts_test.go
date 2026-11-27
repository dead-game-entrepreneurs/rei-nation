package gera

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dead-game-entrepreneurs/rei-nation/internal/game"
	"github.com/dead-game-entrepreneurs/rei-nation/internal/protocol/aries"
	"github.com/dead-game-entrepreneurs/rei-nation/internal/protocol/fesl"
	"github.com/dead-game-entrepreneurs/rei-nation/internal/storage"
)

// memStore is an in-memory IdentityStore for exercising the account flow.
type memStore struct {
	mu       sync.Mutex
	accounts map[string]storage.Account
	personas map[int64][]storage.Persona
	nextAcct int64
	nextPers int64
}

func newMemStore() *memStore {
	return &memStore{accounts: map[string]storage.Account{}, personas: map[int64][]storage.Persona{}}
}

func (s *memStore) CreateAccount(_ context.Context, a storage.Account) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.accounts[strings.ToLower(a.Name)]; ok {
		return storage.ErrConflict
	}
	s.nextAcct++
	a.ID = s.nextAcct
	s.accounts[strings.ToLower(a.Name)] = a
	return nil
}

func (s *memStore) Account(_ context.Context, name string) (storage.Account, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.accounts[strings.ToLower(name)]
	return a, ok, nil
}

func (s *memStore) CreatePersona(_ context.Context, account int64, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, list := range s.personas {
		for _, p := range list {
			if strings.EqualFold(p.Name, name) {
				return storage.ErrConflict
			}
		}
	}
	s.nextPers++
	s.personas[account] = append(s.personas[account], storage.Persona{ID: s.nextPers, Name: name})
	return nil
}

func (s *memStore) Personas(_ context.Context, account int64) ([]storage.Persona, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]storage.Persona(nil), s.personas[account]...), nil
}

// acctConn drives the server's account handler directly, without SSL or a
// socket, by pairing the handler with a lightweight in-memory client.
func newTestServer(t *testing.T) *server {
	t.Helper()
	def := Definition(false)
	opts := game.Options{PublicIP: netip.MustParseAddr("192.0.2.10"),
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Store: newMemStore()}
	return def.New(opts).(*server)
}

// pipeClient connects the aries server to the test over a real socket so the
// exchange runs end to end through the frame codec, minus SSL.
func dialHandler(t *testing.T, s *server) (*aries.Frame, net.Conn, func()) {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- aries.ServeWithOptions(ctx, ln, s, aries.ServeOptions{IdleTimeout: idleTimeout}) }()
	conn, err := net.Dial("tcp4", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	// One deadline covers the whole test; bcrypt under -race needs several seconds.
	conn.SetDeadline(time.Now().Add(30 * time.Second))
	return nil, conn, func() {
		conn.Close()
		cancel()
		<-done
		ln.Close()
	}
}

func TestAccountFlow(t *testing.T) {
	s := newTestServer(t)
	_, conn, cleanup := dialHandler(t, s)
	defer cleanup()

	seq := uint32(0xc0000000)
	call := func(service, txn string, fields ...fesl.Field) fesl.Message {
		t.Helper()
		seq++
		if err := fesl.Encode(service, seq, txn, fields...).Write(conn); err != nil {
			t.Fatal(err)
		}
		f, err := aries.Read(conn)
		if err != nil {
			t.Fatal(err)
		}
		m := fesl.Decode(f)
		if m.Code != seq {
			t.Fatalf("%s %s: reply code %#x, want %#x", service, txn, m.Code, seq)
		}
		return m
	}
	noError := func(m fesl.Message) {
		t.Helper()
		if code, ok := m.Get("errorCode"); ok {
			t.Fatalf("%s returned errorCode %s", m.TXN, code)
		}
	}

	// Terms screen: must come back with a non-empty, decodable body.
	tos := call("acct", "GetTos")
	noError(tos)
	if v, _ := tos.Get("tos"); !strings.Contains(v, "preservation") {
		t.Fatalf("GetTos tos = %q", v)
	}

	countries := call("acct", "GetCountryList")
	if v, _ := countries.Get("countryList.0.ISOCode"); v != "US" {
		t.Fatalf("country ISOCode = %q", v)
	}

	noError(call("acct", "AddAccount",
		fesl.Field{Key: "name", Value: "tester"}, fesl.Field{Key: "password", Value: "pw-123"},
		fesl.Field{Key: "email", Value: "t@example.com"}, fesl.Field{Key: "countryCode", Value: "US"}))

	// Duplicate registration is rejected.
	dup := call("acct", "AddAccount",
		fesl.Field{Key: "name", Value: "tester"}, fesl.Field{Key: "password", Value: "pw-123"})
	if code, _ := dup.Get("errorCode"); code != strconv.Itoa(errDuplicate) {
		t.Fatalf("duplicate account errorCode = %q, want %d", code, errDuplicate)
	}

	// Wrong password is rejected, correct one logs in.
	bad := call("acct", "Login",
		fesl.Field{Key: "name", Value: "tester"}, fesl.Field{Key: "password", Value: "wrong"})
	if code, _ := bad.Get("errorCode"); code != strconv.Itoa(errBadPassword) {
		t.Fatalf("bad password errorCode = %q, want %d", code, errBadPassword)
	}
	login := call("acct", "Login",
		fesl.Field{Key: "name", Value: "tester"}, fesl.Field{Key: "password", Value: "pw-123"})
	noError(login)
	if v, _ := login.Get("lkey"); v == "" {
		t.Fatal("login did not return an lkey")
	}

	// Persona creation and selection.
	noError(call("acct", "AddSubAccount", fesl.Field{Key: "name", Value: "Agent47"}))
	subs := call("acct", "GetSubAccounts")
	if v, _ := subs.Get("subAccounts.[]"); v != "1" {
		t.Fatalf("subAccounts count = %q", v)
	}
	if v, _ := subs.Get("subAccounts.0"); v != "Agent47" {
		t.Fatalf("persona name = %q", v)
	}
	persona := call("acct", "LoginSubAccount", fesl.Field{Key: "name", Value: "Agent47"})
	noError(persona)
	personaKey, _ := persona.Get("lkey")
	if personaKey == "" {
		t.Fatal("persona login did not return an lkey")
	}
	if _, ok := s.lookupKey(personaKey); !ok {
		t.Fatal("persona key not accepted while logged in")
	}

	// Logout revokes every key the connection was issued.
	noError(call("acct", "Logout"))
	loginKey, _ := login.Get("lkey")
	for _, key := range []string{loginKey, personaKey} {
		if _, ok := s.lookupKey(key); ok {
			t.Fatal("key still accepted after logout")
		}
	}
}

func TestSessionKeyExpiresAfterDisconnect(t *testing.T) {
	s := newTestServer(t)
	c := &aries.Client{}
	key := s.issueSessionKey(c, identity{accountID: 1, personaID: 2, personaName: "Agent47"})
	s.revokeKeys(c, time.Minute)
	if _, ok := s.lookupKey(key); !ok {
		t.Fatal("key revoked before its reconnect grace ended")
	}
	s.keysMu.Lock()
	e := s.keys[key]
	e.expires = time.Now().Add(-time.Second)
	s.keys[key] = e
	s.keysMu.Unlock()
	if _, ok := s.lookupKey(key); ok {
		t.Fatal("key accepted after its grace ended")
	}
}

func TestSubAccountsBeforeLogin(t *testing.T) {
	s := newTestServer(t)
	_, conn, cleanup := dialHandler(t, s)
	defer cleanup()
	if err := fesl.Encode("acct", 0xc0000001, "GetSubAccounts").Write(conn); err != nil {
		t.Fatal(err)
	}
	f, err := aries.Read(conn)
	if err != nil {
		t.Fatal(err)
	}
	m := fesl.Decode(f)
	if code, _ := m.Get("errorCode"); code != strconv.Itoa(errNoSuchUser) {
		t.Fatalf("errorCode = %q, want %d before login", code, errNoSuchUser)
	}
}

func TestRankService(t *testing.T) {
	s := newTestServer(t)
	_, conn, cleanup := dialHandler(t, s)
	defer cleanup()
	seq := uint32(0xc0000000)
	call := func(service, txn string, fields ...fesl.Field) fesl.Message {
		t.Helper()
		seq++
		if err := fesl.Encode(service, seq, txn, fields...).Write(conn); err != nil {
			t.Fatal(err)
		}
		f, err := aries.Read(conn)
		if err != nil {
			t.Fatal(err)
		}
		m := fesl.Decode(f)
		if m.Code != seq || m.TXN != txn {
			t.Fatalf("%s reply = %q %#x", txn, m.TXN, m.Code)
		}
		return m
	}

	// Stats reads need a persona.
	if code, _ := call("rank", "GetRankedStats").Get("errorCode"); code != strconv.Itoa(errNoSuchUser) {
		t.Fatalf("GetRankedStats before login errorCode = %q", code)
	}
	call("acct", "AddAccount", fesl.Field{Key: "name", Value: "ranker"}, fesl.Field{Key: "password", Value: "pw"})
	call("acct", "Login", fesl.Field{Key: "name", Value: "ranker"}, fesl.Field{Key: "password", Value: "pw"})
	call("acct", "AddSubAccount", fesl.Field{Key: "name", Value: "R1"})
	call("acct", "LoginSubAccount", fesl.Field{Key: "name", Value: "R1"})

	if v, _ := call("rank", "GetRankedStats").Get("stats.[]"); v != "0" {
		t.Fatalf("GetRankedStats stats.[] = %q, want 0", v)
	}
	sid := call("rank", "GetSessionId")
	if v, _ := sid.Get("sessionId"); v == "" || v == "0" {
		t.Fatalf("GetSessionId sessionId = %q", v)
	}
	for _, txn := range []string{"StartReport", "UpdateStats", "EndReport"} {
		if code, ok := call("rank", txn).Get("errorCode"); ok {
			t.Fatalf("%s errorCode = %q", txn, code)
		}
	}
	if _, ok := call("rank", "GetDateRange").Get("errorCode"); !ok {
		t.Fatal("unknown rank transaction should get an error reply, not silence")
	}
}
