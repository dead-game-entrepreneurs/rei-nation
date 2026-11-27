package gera

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"sort"
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

// memSocial adds in-memory friends, mail and stats to memStore. It mirrors the
// PostgreSQL rules the adapter depends on; the SQL itself is covered by the
// storage package's database test.
type memSocial struct {
	*memStore
	smu     sync.Mutex
	friends map[[2]string]*memFriend
	mail    []storage.Mail
	nextID  int64
	stats   map[int64]map[string]storage.StatValue
}

type memFriend struct {
	inviter  string
	accepted bool
}

func newMemSocial() *memSocial {
	return &memSocial{memStore: newMemStore(), friends: map[[2]string]*memFriend{},
		stats: map[int64]map[string]storage.StatValue{}}
}

func pair(a, b string) [2]string { return [2]string{min(a, b), max(a, b)} }

func (s *memSocial) all() []storage.Persona {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []storage.Persona
	for _, list := range s.personas {
		out = append(out, list...)
	}
	return out
}

func (s *memSocial) PersonaByName(_ context.Context, name string) (storage.Persona, bool, error) {
	for _, p := range s.all() {
		if strings.EqualFold(p.Name, name) {
			return p, true, nil
		}
	}
	return storage.Persona{}, false, nil
}

func (s *memSocial) SearchPersonas(_ context.Context, prefix string, limit int) ([]string, error) {
	var out []string
	for _, p := range s.all() {
		if strings.HasPrefix(strings.ToLower(p.Name), strings.ToLower(prefix)) {
			out = append(out, p.Name)
		}
	}
	sort.Strings(out)
	return out[:min(len(out), limit)], nil
}

func (s *memSocial) Friends(ctx context.Context, _ string, name string) ([]storage.Friend, error) {
	s.smu.Lock()
	defer s.smu.Unlock()
	var out []storage.Friend
	for k, f := range s.friends {
		other := k[0]
		if other == name {
			other = k[1]
		} else if k[1] != name {
			continue
		}
		p, _, _ := s.PersonaByName(ctx, other)
		state := storage.FriendIncoming
		switch {
		case f.accepted:
			state = storage.FriendAccepted
		case f.inviter == name:
			state = storage.FriendOutgoing
		}
		out = append(out, storage.Friend{ID: p.ID, Name: p.Name, State: state})
	}
	return out, nil
}

func (s *memSocial) RequestFriend(_ context.Context, _, self, other string) (storage.FriendState, error) {
	s.smu.Lock()
	defer s.smu.Unlock()
	f := s.friends[pair(self, other)]
	switch {
	case f == nil:
		s.friends[pair(self, other)] = &memFriend{inviter: self}
		return storage.FriendOutgoing, nil
	case f.accepted:
		return storage.FriendAccepted, nil
	case f.inviter == self:
		return storage.FriendOutgoing, nil
	}
	f.accepted = true
	return storage.FriendAccepted, nil
}

func (s *memSocial) AnswerFriend(_ context.Context, _, self, requester string, accept bool) (bool, error) {
	s.smu.Lock()
	defer s.smu.Unlock()
	k := pair(self, requester)
	f := s.friends[k]
	if f == nil || f.accepted || f.inviter != requester {
		return false, nil
	}
	if accept {
		f.accepted = true
	} else {
		delete(s.friends, k)
	}
	return true, nil
}

func (s *memSocial) ChangeBuddy(_ context.Context, _, self, other, op string) (bool, error) {
	s.smu.Lock()
	defer s.smu.Unlock()
	k := pair(self, other)
	_, ok := s.friends[k]
	delete(s.friends, k)
	return ok && op != "unblock", nil
}

func (s *memSocial) QueueMail(_ context.Context, _ string, m storage.Mail) (storage.Mail, bool, error) {
	s.smu.Lock()
	defer s.smu.Unlock()
	s.nextID++
	m.ID, m.Sent = s.nextID, time.Unix(1700000000, 0)
	s.mail = append(s.mail, m)
	return m, true, nil
}

func (s *memSocial) Mailbox(_ context.Context, _, name string) ([]storage.Mail, error) {
	s.smu.Lock()
	defer s.smu.Unlock()
	var out []storage.Mail
	for _, m := range s.mail {
		if m.To == name {
			out = append(out, m)
		}
	}
	return out, nil
}

func (s *memSocial) AckMail(_ context.Context, _, _ string, id int64) error {
	s.smu.Lock()
	defer s.smu.Unlock()
	for i, m := range s.mail {
		if m.ID == id {
			s.mail = append(s.mail[:i], s.mail[i+1:]...)
			break
		}
	}
	return nil
}

func (s *memSocial) UpdateStats(_ context.Context, _ string, persona int64, updates []storage.StatUpdate) error {
	s.smu.Lock()
	defer s.smu.Unlock()
	if s.stats[persona] == nil {
		s.stats[persona] = map[string]storage.StatValue{}
	}
	for _, u := range updates {
		v := s.stats[persona][u.Key]
		old, _ := strconv.ParseFloat(v.Value, 64)
		n, _ := strconv.ParseFloat(u.Value, 64)
		if !u.Add {
			old = 0
		}
		v.Key, v.Value = u.Key, strconv.FormatFloat(old+n, 'f', 4, 64)
		if u.Text != nil {
			v.Text = *u.Text
		}
		s.stats[persona][u.Key] = v
	}
	return nil
}

// board ranks every persona holding key, highest first.
func (s *memSocial) board(key string) []storage.StatRow {
	var rows []storage.StatRow
	for _, p := range s.all() {
		if v, ok := s.stats[p.ID][key]; ok {
			rows = append(rows, storage.StatRow{PersonaID: p.ID, Name: p.Name, StatValue: v})
		}
	}
	val := func(r storage.StatRow) float64 { f, _ := strconv.ParseFloat(r.Value, 64); return f }
	sort.Slice(rows, func(i, j int) bool {
		if val(rows[i]) != val(rows[j]) {
			return val(rows[i]) > val(rows[j])
		}
		return rows[i].PersonaID < rows[j].PersonaID
	})
	for i := range rows {
		rows[i].Rank = 1
		for _, o := range rows {
			if val(o) > val(rows[i]) {
				rows[i].Rank++
			}
		}
	}
	return rows
}

func (s *memSocial) PersonaStats(_ context.Context, _ string, persona int64, keys []string) (string, []storage.StatValue, error) {
	s.smu.Lock()
	defer s.smu.Unlock()
	name := ""
	for _, p := range s.all() {
		if p.ID == persona {
			name = p.Name
		}
	}
	if name == "" {
		return "", nil, nil
	}
	var out []storage.StatValue
	for _, k := range keys {
		v := storage.StatValue{Key: k, Value: "0.0000"}
		for _, r := range s.board(k) {
			if r.PersonaID == persona {
				v = r.StatValue
			}
		}
		out = append(out, v)
	}
	return name, out, nil
}

func (s *memSocial) TopStats(_ context.Context, _, key string, first, count int, extra []string) ([]storage.StatRow, error) {
	s.smu.Lock()
	defer s.smu.Unlock()
	rows := s.board(key)
	if first-1 >= len(rows) {
		return nil, nil
	}
	rows = rows[first-1 : min(len(rows), first-1+count)]
	for i := range rows {
		for _, k := range extra {
			v, ok := s.stats[rows[i].PersonaID][k]
			if !ok {
				v = storage.StatValue{Key: k, Value: "0.0000"}
			}
			rows[i].Extra = append(rows[i].Extra, v)
		}
	}
	return rows, nil
}

func newSocialServer(t *testing.T) (*server, *memSocial) {
	t.Helper()
	store := newMemSocial()
	opts := game.Options{PublicIP: netip.MustParseAddr("192.0.2.10"),
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Store: store}
	return Definition().New(opts).(*server), store
}

// msgClient is a messenger connection that collects every frame it receives.
type msgClient struct {
	t    *testing.T
	conn net.Conn
	id   int
	in   chan fesl.Message
}

func dialMessenger(t *testing.T, addr, key string) *msgClient {
	t.Helper()
	conn, err := net.Dial("tcp4", addr)
	if err != nil {
		t.Fatal(err)
	}
	c := &msgClient{t: t, conn: conn, in: make(chan fesl.Message, 64)}
	go func() {
		for {
			f, err := aries.Read(conn)
			if err != nil {
				close(c.in)
				return
			}
			c.in <- fesl.Decode(f)
		}
	}()
	t.Cleanup(func() { conn.Close() })
	if m := c.call("AUTH", fesl.Field{Key: "lkey", Value: key}); m.Code != msgOK {
		t.Fatalf("AUTH code %#x", m.Code)
	}
	return c
}

func (c *msgClient) send(kind string, fields ...fesl.Field) {
	c.t.Helper()
	c.id++
	fields = append([]fesl.Field{{Key: "ID", Value: strconv.Itoa(c.id)}}, fields...)
	if err := fesl.Packet(kind, 0, fields...).Write(c.conn); err != nil {
		c.t.Fatal(err)
	}
}

// next returns the next frame of the given kind, skipping nothing: frames
// arrive in order, so a different kind is a test failure.
func (c *msgClient) next(kind string) fesl.Message {
	c.t.Helper()
	select {
	case m, ok := <-c.in:
		if !ok {
			c.t.Fatalf("connection closed waiting for %s", kind)
		}
		if m.Service != kind {
			c.t.Fatalf("got %s %v, want %s", m.Service, m.Fields(), kind)
		}
		return m
	case <-time.After(3 * time.Second):
		c.t.Fatalf("timed out waiting for %s", kind)
	}
	return fesl.Message{}
}

func (c *msgClient) call(kind string, fields ...fesl.Field) fesl.Message {
	c.t.Helper()
	c.send(kind, fields...)
	return c.next(kind)
}

func (c *msgClient) quiet() {
	c.t.Helper()
	select {
	case m := <-c.in:
		c.t.Fatalf("unexpected %s %v", m.Service, m.Fields())
	case <-time.After(100 * time.Millisecond):
	}
}

func expectField(t *testing.T, m fesl.Message, key, want string) {
	t.Helper()
	if got, _ := m.Get(key); got != want {
		t.Fatalf("%s %s = %q, want %q", m.Service, key, got, want)
	}
}

func TestMessengerFriends(t *testing.T) {
	s, store := newSocialServer(t)
	ctx := context.Background()
	for _, name := range []string{"Bond", "Goldfinger"} {
		if err := store.CreatePersona(ctx, 1, name); err != nil {
			t.Fatal(err)
		}
	}
	bond, _, _ := store.PersonaByName(ctx, "Bond")
	gold, _, _ := store.PersonaByName(ctx, "Goldfinger")

	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	sctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- aries.ServeWithOptions(sctx, ln, s.messenger, aries.ServeOptions{IdleTimeout: noIdle}) }()
	defer func() { cancel(); <-done }()

	a := dialMessenger(t, ln.Addr().String(), s.issueKey(identity{accountID: 1, personaID: bond.ID, personaName: bond.Name}))
	b := dialMessenger(t, ln.Addr().String(), s.issueKey(identity{accountID: 1, personaID: gold.ID, personaName: gold.Name}))
	a.call("PSET", fesl.Field{Key: "SHOW", Value: "CHAT"}, fesl.Field{Key: "STAT", Value: "Online"})
	b.call("PSET", fesl.Field{Key: "SHOW", Value: "CHAT"})

	// Search by prefix.
	found := a.call("USCH", fesl.Field{Key: "USER", Value: "gold"}, fesl.Field{Key: "MAXR", Value: "5"})
	expectField(t, found, "SIZE", "1")
	// Results carry the search's ID; the client drops entries without it.
	result := a.next("USER")
	expectField(t, result, "USER", "Goldfinger")
	expectField(t, result, "ID", found.Fields()["ID"])

	// A request shows as outgoing to the sender and incoming to the target.
	// Roster notifications go out before the acknowledgement.
	a.send("RADM", fesl.Field{Key: "USER", Value: "Goldfinger@messaging.ea.com"})
	expectField(t, a.next("RNOT"), "ATTR", "T")
	a.next("RADM")
	rnot := b.next("RNOT")
	expectField(t, rnot, "USER", "Bond")
	expectField(t, rnot, "UID", strconv.FormatInt(bond.ID, 10))
	expectField(t, rnot, "CHNG", "A")
	expectField(t, rnot, "ATTR", "R")

	// Pending entries are listed only when asked for.
	expectField(t, b.call("RGET", fesl.Field{Key: "LIST", Value: "B"}), "SIZE", "0")
	expectField(t, b.call("RGET", fesl.Field{Key: "LIST", Value: "B"}, fesl.Field{Key: "PEND", Value: "Y"}), "SIZE", "1")
	rost := b.next("ROST")
	expectField(t, rost, "ATTR", "R")
	expectField(t, rost, "ID", strconv.Itoa(b.id))

	// Accepting adds S on both sides with no D first: the client replaces a
	// T entry itself, and a D would delete the requester's entry.
	b.send("RRSP", fesl.Field{Key: "USER", Value: "Bond"}, fesl.Field{Key: "ANSW", Value: "Y"}, fesl.Field{Key: "PRES", Value: "Y"})
	accepted := b.next("RNOT")
	expectField(t, accepted, "CHNG", "A")
	expectField(t, accepted, "ATTR", "S")
	accepted = a.next("RNOT")
	expectField(t, accepted, "CHNG", "A")
	expectField(t, accepted, "ATTR", "S")
	expectField(t, a.next("PGET"), "USER", "Goldfinger")
	bondPresence := b.next("PGET")
	expectField(t, bondPresence, "USER", "Bond")
	expectField(t, bondPresence, "STAT", "Online")
	b.next("RRSP")

	// The accepted roster carries presence when asked.
	expectField(t, a.call("RGET", fesl.Field{Key: "PRES", Value: "Y"}), "SIZE", "1")
	expectField(t, a.next("ROST"), "ATTR", "S")
	expectField(t, a.next("PGET"), "SHOW", "CHAT")

	// A presence change reaches the friend.
	b.call("PSET", fesl.Field{Key: "SHOW", Value: "AWAY"})
	expectField(t, a.next("PGET"), "SHOW", "AWAY")

	// A message is delivered to an online friend and leaves the queue.
	a.send("SEND", fesl.Field{Key: "USER", Value: "Goldfinger"}, fesl.Field{Key: "TYPE", Value: "C"},
		fesl.Field{Key: "SUBJ", Value: "Hi"}, fesl.Field{Key: "BODY", Value: "Meet at the dam"})
	expectField(t, a.next("SEND"), "ERRS", "0")
	recv := b.next("RECV")
	expectField(t, recv, "USER", "Bond")
	expectField(t, recv, "BODY", "Meet at the dam")
	expectField(t, recv, "SUBJ", "Hi")
	if mail, _ := store.Mailbox(ctx, gameKey, "Goldfinger"); len(mail) != 0 {
		t.Fatalf("delivered mail still queued: %d", len(mail))
	}

	// Removing the friend drops it from both rosters.
	b.send("RDEL", fesl.Field{Key: "USER", Value: "Bond"}, fesl.Field{Key: "LIST", Value: "B"})
	expectField(t, b.next("RNOT"), "CHNG", "D")
	expectField(t, a.next("RNOT"), "CHNG", "D")
	b.next("RDEL")
	expectField(t, a.call("RGET", fesl.Field{Key: "PEND", Value: "Y"}), "SIZE", "0")

	// Unknown users are refused.
	if m := a.call("RADM", fesl.Field{Key: "USER", Value: "Nobody"}); m.Code != msgUser {
		t.Fatalf("RADM unknown code %#x", m.Code)
	}
	a.quiet()
	b.quiet()
}

func TestMessengerOfflineMessageAndPresence(t *testing.T) {
	s, store := newSocialServer(t)
	ctx := context.Background()
	for _, name := range []string{"Bond", "Oddjob"} {
		if err := store.CreatePersona(ctx, 1, name); err != nil {
			t.Fatal(err)
		}
	}
	store.RequestFriend(ctx, gameKey, "Bond", "Oddjob")
	store.RequestFriend(ctx, gameKey, "Oddjob", "Bond")
	bond, _, _ := store.PersonaByName(ctx, "Bond")
	odd, _, _ := store.PersonaByName(ctx, "Oddjob")

	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	sctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- aries.ServeWithOptions(sctx, ln, s.messenger, aries.ServeOptions{IdleTimeout: noIdle}) }()
	defer func() { cancel(); <-done }()

	a := dialMessenger(t, ln.Addr().String(), s.issueKey(identity{accountID: 1, personaID: bond.ID, personaName: bond.Name}))
	a.call("PSET", fesl.Field{Key: "SHOW", Value: "CHAT"})
	expectField(t, a.call("SEND", fesl.Field{Key: "USER", Value: "Oddjob"}, fesl.Field{Key: "BODY", Value: "later"}), "ERRS", "0")
	if mail, _ := store.Mailbox(ctx, gameKey, "Oddjob"); len(mail) != 1 {
		t.Fatal("offline message not queued")
	}

	// The message waits for the recipient's first PSET.
	b := dialMessenger(t, ln.Addr().String(), s.issueKey(identity{accountID: 1, personaID: odd.ID, personaName: odd.Name}))
	b.quiet()
	b.send("PSET", fesl.Field{Key: "SHOW", Value: "CHAT"})
	b.next("PSET")
	expectField(t, b.next("RECV"), "BODY", "later")
	expectField(t, a.next("PGET"), "USER", "Oddjob")

	// Signing off tells friends.
	b.conn.Close()
	offline := a.next("PGET")
	expectField(t, offline, "USER", "Oddjob")
	expectField(t, offline, "SHOW", "DISC")
}

func TestRankStats(t *testing.T) {
	s, store := newSocialServer(t)
	ctx := context.Background()
	for _, name := range []string{"Bond", "Xenia"} {
		if err := store.CreatePersona(ctx, 1, name); err != nil {
			t.Fatal(err)
		}
	}
	bond, _, _ := store.PersonaByName(ctx, "Bond")
	xenia, _, _ := store.PersonaByName(ctx, "Xenia")
	_, conn, cleanup := dialHandler(t, s)
	defer cleanup()

	seq := uint32(0xc0000000)
	call := func(txn string, fields ...fesl.Field) fesl.Message {
		t.Helper()
		seq++
		if err := fesl.Encode("rank", seq, txn, fields...).Write(conn); err != nil {
			t.Fatal(err)
		}
		f, err := aries.Read(conn)
		if err != nil {
			t.Fatal(err)
		}
		m := fesl.Decode(f)
		if code, ok := m.Get("errorCode"); ok {
			t.Fatalf("%s errorCode %s", txn, code)
		}
		return m
	}
	// GetSessionId creates the connection's session; sign it in as Bond
	// directly rather than through the account flow.
	call("GetSessionId")
	s.mu.Lock()
	for _, a := range s.sessions {
		a.personaID, a.personaName = bond.ID, bond.Name
	}
	s.mu.Unlock()

	report := func(kills, deaths string) {
		call("UpdateStats", fesl.Field{Key: "u.[]", Value: "1"},
			fesl.Field{Key: "u.0.o", Value: strconv.FormatInt(bond.ID, 10)}, fesl.Field{Key: "u.0.ot", Value: "1"},
			fesl.Field{Key: "u.0.s.[]", Value: "2"},
			fesl.Field{Key: "u.0.s.0.k", Value: "kills"}, fesl.Field{Key: "u.0.s.0.v", Value: kills}, fesl.Field{Key: "u.0.s.0.ut", Value: "3"},
			fesl.Field{Key: "u.0.s.1.k", Value: "deaths"}, fesl.Field{Key: "u.0.s.1.v", Value: deaths}, fesl.Field{Key: "u.0.s.1.ut", Value: "3"})
	}
	report("4.0000", "1.0000")
	report("3.0000", "2.0000")
	store.UpdateStats(ctx, gameKey, xenia.ID, []storage.StatUpdate{{Key: "kills", Value: "9", Add: true}})

	mine := call("GetRankedStats", fesl.Field{Key: "owner", Value: strconv.FormatInt(bond.ID, 10)},
		fesl.Field{Key: "ownerType", Value: "1"}, fesl.Field{Key: "periodId", Value: "0"}, fesl.Field{Key: "periodPast", Value: "0"},
		fesl.Field{Key: "keys.[]", Value: "3"}, fesl.Field{Key: "keys.0", Value: "kills"},
		fesl.Field{Key: "keys.1", Value: "deaths"}, fesl.Field{Key: "keys.2", Value: "wins"})
	expectField(t, mine, "stats.[]", "3")
	expectField(t, mine, "stats.0.value", "7.0000")
	expectField(t, mine, "stats.0.rank", "2")
	expectField(t, mine, "stats.0.name", "Bond")
	expectField(t, mine, "stats.1.value", "3.0000")
	expectField(t, mine, "stats.2.value", "0.0000")
	expectField(t, mine, "stats.2.rank", "0")

	board := call("GetTopNAndStats", fesl.Field{Key: "key", Value: "kills"}, fesl.Field{Key: "ownerType", Value: "1"},
		fesl.Field{Key: "minRank", Value: "1"}, fesl.Field{Key: "maxRank", Value: "40"},
		fesl.Field{Key: "keys.[]", Value: "1"}, fesl.Field{Key: "keys.0", Value: "deaths"})
	expectField(t, board, "stats.[]", "2")
	expectField(t, board, "stats.0.name", "Xenia")
	expectField(t, board, "stats.0.rank", "1")
	expectField(t, board, "stats.0.owner", strconv.FormatInt(xenia.ID, 10))
	expectField(t, board, "stats.1.name", "Bond")
	expectField(t, board, "stats.1.addStats.0.key", "deaths")
	expectField(t, board, "stats.1.addStats.0.value", "3.0000")

	// A report for another persona is not credited.
	call("UpdateStats", fesl.Field{Key: "u.[]", Value: "1"},
		fesl.Field{Key: "u.0.o", Value: strconv.FormatInt(xenia.ID, 10)}, fesl.Field{Key: "u.0.ot", Value: "1"},
		fesl.Field{Key: "u.0.s.[]", Value: "1"},
		fesl.Field{Key: "u.0.s.0.k", Value: "kills"}, fesl.Field{Key: "u.0.s.0.v", Value: "100"}, fesl.Field{Key: "u.0.s.0.ut", Value: "3"})
	top := call("GetTopN", fesl.Field{Key: "key", Value: "kills"}, fesl.Field{Key: "minRank", Value: "1"}, fesl.Field{Key: "maxRank", Value: "1"})
	expectField(t, top, "stats.[]", "1")
	expectField(t, top, "stats.0.value", "9.0000")
	// Once Xenia has joined Bond's game, Bond's report for her counts. Wins
	// and games also fill in the rank and win percentage the screens read,
	// and update type 5 replaces a value.
	s.theater.mu.Lock()
	s.theater.guests = map[int64]map[int64]time.Time{bond.ID: {xenia.ID: time.Now()}}
	s.theater.mu.Unlock()
	for _, won := range []string{"1", "0"} {
		call("UpdateStats", fesl.Field{Key: "u.[]", Value: "1"},
			fesl.Field{Key: "u.0.o", Value: strconv.FormatInt(xenia.ID, 10)}, fesl.Field{Key: "u.0.ot", Value: "1"},
			fesl.Field{Key: "u.0.s.[]", Value: "3"},
			fesl.Field{Key: "u.0.s.0.k", Value: "DM_W"}, fesl.Field{Key: "u.0.s.0.v", Value: won}, fesl.Field{Key: "u.0.s.0.ut", Value: "3"},
			fesl.Field{Key: "u.0.s.1.k", Value: "DM_T_G"}, fesl.Field{Key: "u.0.s.1.v", Value: "1"}, fesl.Field{Key: "u.0.s.1.ut", Value: "3"},
			fesl.Field{Key: "u.0.s.2.k", Value: "DM_CUR_L_STRK"}, fesl.Field{Key: "u.0.s.2.v", Value: "4"}, fesl.Field{Key: "u.0.s.2.ut", Value: "5"})
	}
	hers := call("GetRankedStats", fesl.Field{Key: "owner", Value: strconv.FormatInt(xenia.ID, 10)},
		fesl.Field{Key: "keys.[]", Value: "3"}, fesl.Field{Key: "keys.0", Value: "DM_RANK"},
		fesl.Field{Key: "keys.1", Value: "DM_W_PERC"}, fesl.Field{Key: "keys.2", Value: "DM_CUR_L_STRK"})
	expectField(t, hers, "stats.0.value", "1.0000")
	expectField(t, hers, "stats.0.rank", "1")
	expectField(t, hers, "stats.1.value", "50.0000")
	expectField(t, hers, "stats.2.value", "4.0000")
}
