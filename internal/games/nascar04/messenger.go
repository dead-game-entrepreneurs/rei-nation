package nascar04

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dead-game-entrepreneurs/rei-nation/internal/protocol/aries"
	"github.com/dead-game-entrepreneurs/rei-nation/internal/storage"
)

const buddyPort = 5000
const buddyNotice = `"Buddy list updated. You are now buddies."`
const messengerTitle = "PS2_NASCAR04"
const lobbyPresenceProduct = `"is online"`

type buddyClient struct {
	conn           *aries.Client
	owner          *player
	name, show     string
	title, product string
	watch          map[string]struct{}
	inboxReady     bool
}

type messenger struct {
	lobby       *lobby
	store       storage.MessengerStore
	mu          sync.Mutex
	clients     map[*aries.Client]*buddyClient
	online      map[string]*buddyClient
	lobbyOnline map[string]*player
	pendingAuth map[*aries.Client]time.Time
}

func (m *messenger) Handle(ctx context.Context, conn *aries.Client, f aries.Frame) {
	if f.Tag == "AUTH" {
		m.authenticate(conn, f.Fields())
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	user := m.clients[conn]
	if user == nil && m.pendingAuth[conn].IsZero() {
		m.pendingAuth[conn] = time.Now()
	}
	if f.Tag == "PING" || f.Tag == "~png" {
		return
	}
	if user == nil {
		if f.Tag == "PSET" {
			m.lobby.opts.Log.Info("NASCAR Messenger presence arrived before sign-in", "messenger_connection", conn.ID())
		}
		conn.Send(aries.Reply(f.Tag+"maut", nil))
		return
	}

	fields := f.Fields()
	id := fields["ID"]
	if !wireText(id, 32) {
		id = ""
	}
	switch f.Tag {
	case "EPGT":
		conn.Send(aries.Reply("EPGT", map[string]string{"ID": id}))
	case "PSET":
		conn.Send(aries.Reply("PSET", nil))
		if show := fields["SHOW"]; show != "" && wireText(show, 16) {
			user.show = show
			if show == "PASS" {
				user.inboxReady = false
			}
		}
		if product := fields["PROD"]; product != "" && wireText(product, 64) {
			user.product = product
		}
		m.broadcast(user.name)
	case "PADD", "PDEL":
		m.subscription(ctx, user, f.Tag, fields["USER"])
	case "RGET":
		list := fields["LIST"]
		m.roster(ctx, user, id, list)
		// Retail requests buddies first, then ignores, on AUTH and after PASS.
		// Deliver after the roster so the game recognizes senders as buddies.
		if !user.inboxReady && user.show != "PASS" && list == "I" {
			user.inboxReady = m.replayInbox(ctx, user)
		}
	case "USCH":
		m.search(ctx, user, id, fields)
	case "RADM", "RADD", "RDEL", "RDEM":
		m.relationship(ctx, user, f.Tag, id, fields)
	case "SEND":
		m.send(ctx, user, id, fields)
	case "DISC":
		m.remove(user)
		conn.CloseWithReason("client sent DISC")
		m.logClose(user)
	default:
		conn.Send(aries.Reply(f.Tag+"unsp", nil))
	}
}

func (m *messenger) authenticate(conn *aries.Client, f map[string]string) {
	token := strings.ToLower(strings.TrimSpace(strings.Trim(f["LKEY"], "\"")))
	// This build truncates the 64-character lobby key. Require a unique
	// prefix of at least 128 bits and bind it to the active lobby connection.
	if len(token) < 32 || len(token) > 64 {
		m.rejectSignIn(conn, "invalid lobby key length")
		return
	}
	if strings.IndexFunc(token, func(r rune) bool { return !strings.ContainsRune("0123456789abcdef", r) }) >= 0 {
		m.rejectSignIn(conn, "invalid lobby key format")
		return
	}
	product := f["PROD"]
	if !wireText(product, 64) {
		product = ""
	}
	m.lobby.mu.Lock()
	var owner *player
	for _, p := range m.lobby.online {
		if strings.HasPrefix(p.lkey, token) && p.client.Addr() == conn.Addr() {
			if owner != nil {
				owner = nil
				break
			}
			owner = p
		}
	}
	if owner == nil || f["NAME"] != "" && !strings.EqualFold(owner.persona, nameValue(f["NAME"])) {
		m.lobby.mu.Unlock()
		m.rejectSignIn(conn, "no matching active lobby session")
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if conn.Closed() {
		m.lobby.mu.Unlock()
		return
	}
	user := m.clients[conn]
	first := user == nil
	if user != nil && user.owner != owner {
		m.lobby.mu.Unlock()
		m.rejectSignIn(conn, "socket already belongs to another lobby session")
		return
	}
	if user == nil {
		title := f["VERS"]
		if title == "" || !wireText(title, 64) {
			title = messengerTitle
		}
		user = &buddyClient{conn: conn, name: owner.persona, owner: owner, show: "CHAT", title: title, product: product,
			watch: make(map[string]struct{})}
		if prior := m.online[user.name]; prior != nil {
			m.remove(prior)
			prior.conn.CloseWithReason("replaced by Messenger sign-in")
			m.logClose(prior)
		}
		m.clients[conn], m.online[user.name] = user, user
	}
	delete(m.pendingAuth, conn)
	owner.messengerLostAt = time.Time{}
	m.lobby.mu.Unlock()
	conn.Send(aries.Reply("AUTH", map[string]string{"TITL": product}))
	if !first {
		return
	}
	m.lobby.opts.Log.Info("NASCAR Messenger signed in", "messenger_connection", conn.ID(), "lobby_connection", owner.client.ID())
	m.broadcast(user.name)
}

func (m *messenger) replayInbox(ctx context.Context, user *buddyClient) bool {
	messages, err := m.store.Mailbox(ctx, gameKey, user.name)
	if err != nil {
		m.lobby.opts.Log.Warn("could not load NASCAR Messenger inbox")
		return false
	}
	for _, msg := range messages {
		m.deliver(user, msg)
	}
	return true
}

func (m *messenger) rejectSignIn(conn *aries.Client, reason string) {
	m.lobby.opts.Log.Info("NASCAR Messenger sign-in rejected", "messenger_connection", conn.ID(), "reason", reason)
	conn.Send(aries.Reply("AUTHfail", nil))
}

func (m *messenger) Disconnected(conn *aries.Client) {
	m.lobby.mu.Lock()
	defer m.lobby.mu.Unlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.pendingAuth, conn)
	if user := m.clients[conn]; user != nil {
		m.logClose(user)
		m.remove(user)
		// An explicit DISC or replacement already removed its session.
		// Only the live owner of this authenticated socket can be reset.
		owner := user.owner
		if owner.lkey != "" && m.lobby.online[strings.ToLower(owner.persona)] == owner {
			owner.messengerLostAt = time.Now()
		}
	}
}

func (m *messenger) logClose(user *buddyClient) {
	m.lobby.opts.Log.Info("NASCAR Messenger socket closed", "messenger_connection", user.conn.ID(),
		"lobby_connection", user.owner.client.ID(), "inbox_ready", user.inboxReady, "reason", user.conn.CloseReason())
}

// Called under the lobby lock after the player authenticates and selects a persona.
func (m *messenger) resumeLobby(p *player) {
	m.mu.Lock()
	defer m.mu.Unlock()
	prior := m.lobbyOnline[p.persona]
	m.lobbyOnline[p.persona] = p
	user := m.online[p.persona]
	if user == nil {
		if prior == nil {
			m.broadcast(p.persona)
		}
		return
	}
	if user.owner == p {
		return
	}
	if user.owner.personaID == p.personaID && user.conn.Addr() == p.client.Addr() {
		user.owner = p
		p.messengerLostAt = time.Time{}
		m.lobby.opts.Log.Info("NASCAR Messenger resumed on lobby sign-in", "messenger_connection", user.conn.ID(),
			"lobby_connection", p.client.ID())
		return
	}
	m.remove(user)
	user.conn.CloseWithReason("lobby session changed")
	m.logClose(user)
}

// The Messenger connection can remain active while its owner races.
func (m *messenger) leaveLobby(p *player) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.lobbyOnline[p.persona] == p {
		delete(m.lobbyOnline, p.persona)
		if m.online[p.persona] == nil {
			m.broadcast(p.persona)
		}
	}
}

// An owner restricts revocation to its session. Persona deletion revokes any session.
func (m *messenger) revoke(name string, owner *player) {
	m.mu.Lock()
	defer m.mu.Unlock()
	lobbyOwner := m.lobbyOnline[name]
	clearPresence := lobbyOwner != nil && (owner == nil || lobbyOwner == owner)
	if clearPresence {
		delete(m.lobbyOnline, name)
	}
	if user := m.online[name]; user != nil && (owner == nil || user.owner == owner) {
		m.remove(user)
		user.conn.CloseWithReason("persona session revoked")
		m.logClose(user)
	} else if clearPresence && m.online[name] == nil {
		m.broadcast(name)
	}
}

func (m *messenger) remove(user *buddyClient) {
	delete(m.clients, user.conn)
	if m.online[user.name] == user {
		delete(m.online, user.name)
		m.broadcast(user.name)
	}
}

func (m *messenger) roster(ctx context.Context, user *buddyClient, id, list string) {
	if list != "I" {
		list = "B"
	}
	entries, err := m.store.Roster(ctx, gameKey, user.name, list)
	if err != nil {
		user.conn.Send(aries.Reply("RGETfail", map[string]string{"ID": id, "SIZE": "0"}))
		return
	}
	user.conn.Send(aries.Reply("RGET", map[string]string{"ID": id, "SIZE": strconv.Itoa(len(entries))}))
	for _, name := range entries {
		sendRoster(user, id, name)
		if list == "B" {
			m.watch(user, name)
		}
	}
}

func (m *messenger) subscription(ctx context.Context, user *buddyClient, tag, target string) {
	other, found := m.lookup(ctx, target)
	defer user.conn.Send(aries.Reply(tag, nil))
	if !found {
		return
	}
	if tag == "PDEL" {
		delete(user.watch, other)
		return
	}
	if allowed, err := m.store.CanContact(ctx, gameKey, user.name, other); err == nil && allowed {
		m.watch(user, other)
	}
}

func (m *messenger) search(ctx context.Context, user *buddyClient, id string, f map[string]string) {
	prefix := nameValue(f["USER"])
	limit := int(boundedNumber(f["MAXR"], 100))
	if limit == 0 {
		limit = 20
	}
	var names []string
	if prefix != "" && wireText(prefix, 12) {
		names, _ = m.store.SearchPersonas(ctx, prefix, limit)
	}
	user.conn.Send(aries.Reply("USCH", map[string]string{"ID": id, "SIZE": strconv.Itoa(len(names))}))
	for _, name := range names {
		user.conn.Send(aries.Reply("ROST", map[string]string{"ID": id, "USER": name, "GROUP": ""}))
	}
}

func (m *messenger) relationship(ctx context.Context, user *buddyClient, tag, id string, fields map[string]string) {
	other, found := m.lookup(ctx, fields["USER"])
	reply := map[string]string{"ID": id}
	op := "add"
	switch tag {
	case "RADD":
		if fields["LIST"] == "I" {
			op = "block"
		}
	case "RDEM", "RDEL":
		op = "remove"
		if tag == "RDEL" && fields["LIST"] == "I" {
			op = "unblock"
		}
	}
	if !found || other == user.name {
		if op == "add" {
			tag += "fail"
		}
		user.conn.Send(aries.Reply(tag, reply))
		return
	}
	if op == "add" {
		m.addBuddy(ctx, user, tag, other, reply)
		return
	}
	changed, err := m.store.ChangeBuddy(ctx, gameKey, user.name, other, op)
	if err != nil {
		user.conn.Send(aries.Reply(tag+"fail", reply))
		return
	}
	if changed || op == "remove" || op == "unblock" {
		reply["FUSR"] = other
	}
	user.conn.Send(aries.Reply(tag, reply))
	if changed && (op == "remove" || op == "block") {
		delete(user.watch, other)
		if to := m.online[other]; to != nil {
			delete(to.watch, user.name)
		}
	}
}

func (m *messenger) addBuddy(ctx context.Context, user *buddyClient, tag, other string, reply map[string]string) {
	notice, added, err := m.store.AddBuddy(ctx, gameKey, user.name, other, buddyNotice)
	if err != nil {
		user.conn.Send(aries.Reply(tag+"fail", reply))
		return
	}
	reply["FUSR"] = other
	user.conn.Send(aries.Reply(tag, reply))
	if !added {
		return
	}
	m.watch(user, other)
	if recipient := m.online[other]; recipient != nil {
		sendRoster(recipient, "1", user.name)
		m.watch(recipient, user.name)
		if notice.ID != 0 && recipient.inboxReady {
			m.deliver(recipient, notice)
		}
	}
}

func (m *messenger) lookup(ctx context.Context, target string) (string, bool) {
	name, _, _ := strings.Cut(strings.TrimSpace(target), "/")
	name = nameValue(name)
	if !personaName.MatchString(name) {
		return "", false
	}
	name, found, err := m.store.FindPersona(ctx, name)
	return name, found && err == nil
}

func sendRoster(user *buddyClient, id, name string) {
	user.conn.Send(aries.Reply("ROST", map[string]string{"ID": id, "USER": name, "GROUP": "", "ATTR": "T"}))
}

func (m *messenger) watch(user *buddyClient, other string) {
	if len(user.watch) < 200 {
		user.watch[other] = struct{}{}
		m.presence(user, other)
	}
}

func (m *messenger) broadcast(name string) {
	for _, viewer := range m.online {
		if _, subscribed := viewer.watch[name]; subscribed {
			m.presence(viewer, name)
		}
	}
}

func (m *messenger) presence(viewer *buddyClient, name string) {
	fields := map[string]string{"USER": name, "SHOW": "DISC", "TITL": ""}
	if other := m.online[name]; other != nil {
		fields["SHOW"], fields["TITL"] = other.show, other.title
		// The retail presence reader uses PROD to mark an online buddy as playing.
		if other.product != "" {
			fields["PROD"] = other.product
		}
	} else if m.lobbyOnline[name] != nil {
		// Keep the buddy eligible for room challenges while Messenger reconnects.
		fields["SHOW"], fields["TITL"], fields["PROD"] = "CHAT", messengerTitle, lobbyPresenceProduct
	}
	viewer.conn.Send(aries.Reply("PGET", fields))
}

func (m *messenger) send(ctx context.Context, user *buddyClient, id string, f map[string]string) {
	other, found := m.lookup(ctx, f["USER"])
	ack := map[string]string{"ID": id}
	body, kind := f["BODY"], f["TYPE"]
	if kind == "" {
		kind = "C"
	}
	if !found || other == user.name || body == "" || !wireText(body, 255) || !wireText(kind, 8) {
		m.lobby.opts.Log.Info("NASCAR Messenger send rejected", "reason", "invalid recipient or message fields")
		user.conn.Send(aries.Reply("SENDfail", ack))
		return
	}
	mail, queued, err := m.store.QueueMail(ctx, gameKey, storage.Mail{From: user.name, To: other, Kind: kind, Body: body})
	if !queued || err != nil {
		m.lobby.opts.Log.Info("NASCAR Messenger send rejected", "reason", "message could not be queued")
		user.conn.Send(aries.Reply("SENDfail", ack))
		return
	}
	user.conn.Send(aries.Reply("SEND", ack))
	if to := m.online[other]; to != nil && to.inboxReady {
		m.deliver(to, mail)
	}
}

func (m *messenger) deliver(to *buddyClient, mail storage.Mail) {
	stamp := strconv.FormatInt(mail.Sent.Unix(), 10)
	kind := mail.Kind
	if kind == storage.BuddyAddedMail {
		kind = "C"
	}
	frame := aries.Reply("RECV", map[string]string{
		"USER": mail.From, "TYPE": kind, "BODY": mail.Body, "SIZE": strconv.Itoa(len(mail.Body)),
		"TIME": stamp, "SECS": stamp,
	})
	if mail.Delivered {
		to.conn.Send(frame)
		return
	}
	to.conn.SendAfter(frame, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if m.store.AckMail(ctx, gameKey, mail.To, mail.ID) != nil {
			m.lobby.opts.Log.Warn("could not acknowledge Messenger delivery")
		}
	})
}
