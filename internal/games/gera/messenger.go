package gera

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/dead-game-entrepreneurs/rei-nation/internal/protocol/aries"
	"github.com/dead-game-entrepreneurs/rei-nation/internal/protocol/fesl"
	"github.com/dead-game-entrepreneurs/rei-nation/internal/storage"
)

// EA Messenger status codes are four-character tags carried in the frame's code
// word. The client reads the tag, not a numeric value.
const (
	msgOK   uint32 = 0
	msgAuth uint32 = 0x61757468 // "auth": not authenticated
	msgUser uint32 = 0x75736572 // "user": no such user
	msgFull uint32 = 0x66756c6c // "full": the recipient's mailbox is full
)

const (
	msgDomain = "messaging.ea.com"
	// msgResource is the resource the client appends to its messenger JID.
	msgResource = "CSO/GERA-PS2-2005"
	// mailKind marks GoldenEye messages in the shared mailbox. Delivered rows
	// are deleted, unlike NASCAR 04 history.
	mailKind = "gera-message"
	// The recipient copies BODY into a 256-byte buffer and SUBJ into 32 in the
	// TimeSplitters build of this library.
	maxBody    = 255
	maxSubject = 31
	maxSearch  = 20
	// storeTimeout bounds store work done outside a request, such as the
	// offline notice when a player disconnects.
	storeTimeout = 5 * time.Second
)

// presenceKeys are the PSET fields a friend sees in PGET.
var presenceKeys = []string{"SHOW", "STAT", "PROD", "ATTR", "SESS", "TITL", "TIID", "EXTR"}

// socialStore is what the messenger needs from storage for rosters, search and
// offline messages.
type socialStore interface {
	storage.FriendStore
	SearchPersonas(context.Context, string, int) ([]string, error)
	ChangeBuddy(context.Context, string, string, string, string) (bool, error)
	QueueMail(context.Context, string, storage.Mail) (storage.Mail, bool, error)
	Mailbox(context.Context, string, string) ([]storage.Mail, error)
	AckMail(context.Context, string, string, int64) error
}

// messenger is the EA Messenger service on the aux port. The client opens it
// right after persona login and will not consider itself online until the
// AUTH handshake succeeds. It shares the server's session-key store so the key
// issued at FESL login identifies the player here.
//
// Friends are request-and-accept, kept in the shared buddies table under the
// gera title. The command set and field names match the GoldenEye binary's
// messenger tag table; the roster flag letters and notification order follow
// the TimeSplitters server, which uses the same library.
type messenger struct {
	srv *server

	mu       sync.Mutex
	sessions map[*aries.Client]*presence

	// deliverMu serializes mailbox delivery so a message is not sent twice.
	deliverMu sync.Mutex
}

type presence struct {
	id     identity
	authed bool
	// ready is set by the first PSET. The client sets up its roster after AUTH
	// and should not receive pushes before then.
	ready  bool
	fields map[string]string
}

type mailBody struct {
	Type    string `json:"type"`
	Subject string `json:"subject"`
	Body    string `json:"body"`
}

func (h *messenger) session(c *aries.Client) *presence {
	h.mu.Lock()
	defer h.mu.Unlock()
	p := h.sessions[c]
	if p == nil {
		p = &presence{}
		h.sessions[c] = p
	}
	return p
}

// peer is one signed-in connection and a copy of its presence.
type peer struct {
	c      *aries.Client
	ready  bool
	fields map[string]string
}

// online returns the signed-in connections for a persona.
func (h *messenger) online(name string) []peer {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []peer
	for c, p := range h.sessions {
		if p.authed && strings.EqualFold(p.id.personaName, name) && !c.Closed() {
			out = append(out, peer{c: c, ready: p.ready, fields: p.fields})
		}
	}
	return out
}

func (h *messenger) Disconnected(c *aries.Client) {
	h.mu.Lock()
	p := h.sessions[c]
	delete(h.sessions, c)
	h.mu.Unlock()
	h.srv.opts.Log.Info("messenger client disconnected", "conn", c.ID(), "reason", c.CloseReason())
	if p != nil && p.authed && h.srv.social != nil && len(h.online(p.id.personaName)) == 0 {
		ctx, cancel := context.WithTimeout(context.Background(), storeTimeout)
		defer cancel()
		h.notifyPresence(ctx, p.id.personaName, nil)
	}
}

func (h *messenger) Handle(ctx context.Context, c *aries.Client, f aries.Frame) {
	m := fesl.Decode(f)
	ident, _ := m.GetFold("id")
	if !validID(ident) {
		ident = "0"
	}
	p := h.session(c)
	ack := func(code uint32, extra ...fesl.Field) {
		c.Send(fesl.Packet(m.Service, code, append([]fesl.Field{{Key: "ID", Value: ident}}, extra...)...))
	}

	if m.Service == "AUTH" {
		h.auth(c, p, m, ident)
		return
	}
	if !p.authed {
		ack(msgAuth)
		return
	}
	self := p.id.personaName
	if h.srv.social == nil {
		h.withoutStore(c, p, m, ack)
		return
	}
	switch m.Service {
	case "PING":
		ack(msgOK)
	case "PSET":
		h.setPresence(p, m)
		ack(msgOK)
		h.notifyPresence(ctx, self, h.presenceOf(c))
		h.deliver(ctx, self)
	case "USCH":
		h.search(ctx, c, m, ident)
	case "RGET":
		h.roster(ctx, c, m, p, ident)
	case "PADD", "PDEL":
		other, ok := h.friend(ctx, self, m)
		if !ok {
			ack(msgUser)
			return
		}
		ack(msgOK)
		if m.Service == "PADD" {
			c.Send(h.presencePacket(other.Name, h.latest(other.Name)))
		}
	case "RADD", "RDEL", "RADM", "RRSP", "RDEM":
		h.changeRoster(ctx, c, m, p, ack)
	case "SEND", "BRDC":
		h.send(ctx, c, m, p, ack)
	case "DISC":
		c.CloseWithReason("messenger disconnect")
	default:
		// EPGT, EPST, game invites and the rest are acknowledged so the client
		// does not wait on them.
		h.srv.opts.Log.Info("unhandled messenger transaction", "conn", c.ID(), "kind", m.Service)
		ack(msgOK)
	}
}

// withoutStore answers a server running without a database: the player can
// sign in, but rosters and searches are empty.
func (h *messenger) withoutStore(c *aries.Client, p *presence, m fesl.Message, ack func(uint32, ...fesl.Field)) {
	switch m.Service {
	case "PSET":
		h.setPresence(p, m)
		ack(msgOK)
	case "RGET", "USCH":
		ack(msgOK, fesl.Field{Key: "SIZE", Value: "0"})
	case "DISC":
		c.CloseWithReason("messenger disconnect")
	default:
		ack(msgOK)
	}
}

func (h *messenger) auth(c *aries.Client, p *presence, m fesl.Message, ident string) {
	lkey, _ := m.GetFold("lkey")
	id, ok := h.srv.lookupKey(lkey)
	if !ok || id.personaID == 0 {
		h.srv.opts.Log.Info("messenger auth rejected", "conn", c.ID())
		c.Send(fesl.Packet("AUTH", msgAuth, fesl.Field{Key: "ID", Value: ident}))
		return
	}
	h.mu.Lock()
	p.id = id
	p.authed = true
	h.mu.Unlock()
	h.srv.opts.Log.Info("messenger auth", "conn", c.ID(), "persona", id.personaName, "persona_id", id.personaID)
	c.Send(fesl.Packet("AUTH", msgOK,
		fesl.Field{Key: "ID", Value: ident},
		fesl.Field{Key: "USER", Value: id.personaName},
		fesl.Field{Key: "TITL", Value: ""},
		fesl.Field{Key: "TIID", Value: "0"},
	))
}

func (h *messenger) setPresence(p *presence, m fesl.Message) {
	fields := make(map[string]string)
	for _, k := range presenceKeys {
		if v, ok := m.GetFold(k); ok {
			fields[k] = v
		}
	}
	h.mu.Lock()
	p.ready = true
	p.fields = fields
	h.mu.Unlock()
}

func (h *messenger) presenceOf(c *aries.Client) map[string]string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if p := h.sessions[c]; p != nil {
		return p.fields
	}
	return nil
}

// latest is the presence of a persona's most recent ready connection, or nil
// when it is offline.
func (h *messenger) latest(name string) map[string]string {
	var best map[string]string
	var bestID uint64
	for _, o := range h.online(name) {
		if o.ready && o.c.ID() >= bestID {
			best, bestID = o.fields, o.c.ID()
		}
	}
	return best
}

// presencePacket is a PGET push. A nil presence reports the persona offline.
func (h *messenger) presencePacket(name string, fields map[string]string) aries.Frame {
	if fields == nil {
		fields = map[string]string{"SHOW": "DISC"}
	}
	get := func(k, def string) string {
		if v, ok := fields[k]; ok {
			return v
		}
		return def
	}
	return fesl.Packet("PGET", msgOK,
		fesl.Field{Key: "USER", Value: name},
		fesl.Field{Key: "DOMN", Value: msgDomain},
		fesl.Field{Key: "RSRC", Value: msgResource},
		fesl.Field{Key: "SHOW", Value: get("SHOW", "CHAT")},
		fesl.Field{Key: "STAT", Value: get("STAT", "")},
		fesl.Field{Key: "PROD", Value: get("PROD", "")},
		fesl.Field{Key: "ATTR", Value: get("ATTR", "")},
		fesl.Field{Key: "SESS", Value: get("SESS", "")},
		fesl.Field{Key: "TITL", Value: get("TITL", "")},
		fesl.Field{Key: "TIID", Value: get("TIID", "0")},
		fesl.Field{Key: "EXTR", Value: get("EXTR", "")},
	)
}

// notifyPresence pushes a persona's presence to its online accepted friends.
func (h *messenger) notifyPresence(ctx context.Context, name string, fields map[string]string) {
	friends, err := h.srv.social.Friends(ctx, gameKey, name)
	if err != nil {
		h.srv.opts.Log.Error("messenger roster read failed", "error", err)
		return
	}
	packet := h.presencePacket(name, fields)
	for _, f := range friends {
		if f.State != storage.FriendAccepted {
			continue
		}
		for _, o := range h.online(f.Name) {
			if o.ready {
				o.c.Send(packet)
			}
		}
	}
}

// rosterFlag is the ATTR letter the client ORs into a roster entry.
func rosterFlag(s storage.FriendState) string {
	switch s {
	case storage.FriendAccepted:
		return "S"
	case storage.FriendIncoming:
		return "R"
	case storage.FriendOutgoing:
		return "T"
	}
	return ""
}

// notifyRoster sends an RNOT roster change about other to every connection of
// to. change is A (add) or D (delete).
func (h *messenger) notifyRoster(to string, other storage.Persona, change string, state storage.FriendState) {
	packet := fesl.Packet("RNOT", msgOK,
		fesl.Field{Key: "USER", Value: other.Name},
		fesl.Field{Key: "UID", Value: strconv.FormatInt(other.ID, 10)},
		fesl.Field{Key: "GROUP", Value: ""},
		fesl.Field{Key: "CHNG", Value: change},
		fesl.Field{Key: "ATTR", Value: rosterFlag(state)},
	)
	for _, o := range h.online(to) {
		o.c.Send(packet)
	}
}

func (h *messenger) search(ctx context.Context, c *aries.Client, m fesl.Message, ident string) {
	query, _ := m.GetFold("user")
	query, _, _ = strings.Cut(query, "@")
	query = strings.TrimSpace(strings.TrimRight(query, "*"))
	limit, _ := intField(m, "maxr", maxSearch, 0, 1<<31-1)
	limit = min(limit, maxSearch)
	var names []string
	if query != "" && !strings.ContainsAny(query, "*?") && limit > 0 {
		var err error
		names, err = h.srv.social.SearchPersonas(ctx, query, limit)
		if err != nil {
			h.srv.opts.Log.Error("messenger search failed", "conn", c.ID(), "error", err)
			names = nil
		}
	}
	c.Send(fesl.Packet("USCH", msgOK, fesl.Field{Key: "ID", Value: ident}, fesl.Field{Key: "SIZE", Value: strconv.Itoa(len(names))}))
	for _, n := range names {
		c.Send(fesl.Packet("USER", msgOK, fesl.Field{Key: "USER", Value: n},
			fesl.Field{Key: "DOMN", Value: msgDomain}, fesl.Field{Key: "RSRC", Value: msgResource}))
	}
}

func (h *messenger) roster(ctx context.Context, c *aries.Client, m fesl.Message, p *presence, ident string) {
	var friends []storage.Friend
	list, _ := m.GetFold("list")
	if list == "" || strings.EqualFold(list, "B") {
		var err error
		friends, err = h.srv.social.Friends(ctx, gameKey, p.id.personaName)
		if err != nil {
			h.srv.opts.Log.Error("messenger roster read failed", "conn", c.ID(), "error", err)
		}
	}
	if pend, _ := m.GetFold("pend"); !strings.EqualFold(pend, "Y") {
		accepted := friends[:0]
		for _, f := range friends {
			if f.State == storage.FriendAccepted {
				accepted = append(accepted, f)
			}
		}
		friends = accepted
	}
	c.Send(fesl.Packet("RGET", msgOK, fesl.Field{Key: "ID", Value: ident}, fesl.Field{Key: "SIZE", Value: strconv.Itoa(len(friends))}))
	for _, f := range friends {
		c.Send(fesl.Packet("ROST", msgOK,
			fesl.Field{Key: "USER", Value: f.Name},
			fesl.Field{Key: "UID", Value: strconv.FormatInt(f.ID, 10)},
			fesl.Field{Key: "DOMN", Value: msgDomain},
			fesl.Field{Key: "RSRC", Value: msgResource},
			fesl.Field{Key: "GROUP", Value: ""},
			fesl.Field{Key: "ATTR", Value: rosterFlag(f.State)},
		))
	}
	if pres, _ := m.GetFold("pres"); strings.EqualFold(pres, "Y") {
		for _, f := range friends {
			if f.State != storage.FriendAccepted {
				continue
			}
			if fields := h.latest(f.Name); fields != nil {
				c.Send(h.presencePacket(f.Name, fields))
			}
		}
	}
}

// target resolves the USER field to a persona other than self.
func (h *messenger) target(ctx context.Context, self string, m fesl.Message) (storage.Persona, bool) {
	name, _ := m.GetFold("user")
	name, _, _ = strings.Cut(name, "@")
	name, _, _ = strings.Cut(name, "/")
	if name == "" {
		return storage.Persona{}, false
	}
	other, ok, err := h.srv.social.PersonaByName(ctx, name)
	if err != nil {
		h.srv.opts.Log.Error("messenger persona lookup failed", "error", err)
	}
	return other, ok && !strings.EqualFold(other.Name, self)
}

// friend resolves USER to an accepted friend of self.
func (h *messenger) friend(ctx context.Context, self string, m fesl.Message) (storage.Persona, bool) {
	other, ok := h.target(ctx, self, m)
	if !ok {
		return other, false
	}
	return other, h.state(ctx, self, other.Name) == storage.FriendAccepted
}

// state is self's roster state for other, or 0 when there is none.
func (h *messenger) state(ctx context.Context, self, other string) storage.FriendState {
	friends, err := h.srv.social.Friends(ctx, gameKey, self)
	if err != nil {
		h.srv.opts.Log.Error("messenger roster read failed", "error", err)
	}
	for _, f := range friends {
		if f.Name == other {
			return f.State
		}
	}
	return 0
}

func (h *messenger) changeRoster(ctx context.Context, c *aries.Client, m fesl.Message, p *presence, ack func(uint32, ...fesl.Field)) {
	self := storage.Persona{ID: p.id.personaID, Name: p.id.personaName}
	other, ok := h.target(ctx, self.Name, m)
	if !ok {
		h.srv.opts.Log.Info("messenger roster change for unknown user", "conn", c.ID(), "kind", m.Service)
		ack(msgUser)
		return
	}
	list, _ := m.GetFold("list")
	ignoreList := strings.EqualFold(list, "I")
	h.srv.opts.Log.Info("messenger roster change", "conn", c.ID(), "kind", m.Service, "list", list,
		"persona", self.Name, "target", other.Name)
	social := h.srv.social
	before := h.state(ctx, self.Name, other.Name)
	switch {
	case ignoreList && m.Service == "RADD":
		if _, err := social.ChangeBuddy(ctx, gameKey, self.Name, other.Name, "block"); err != nil {
			h.storeFailed(c, ack, err)
			return
		}
		if before != 0 {
			h.dropBoth(self, other)
		}
	case ignoreList && m.Service == "RDEL":
		if _, err := social.ChangeBuddy(ctx, gameKey, self.Name, other.Name, "unblock"); err != nil {
			h.storeFailed(c, ack, err)
			return
		}
	case m.Service == "RADM" || m.Service == "RADD":
		state, err := social.RequestFriend(ctx, gameKey, self.Name, other.Name)
		if errors.Is(err, storage.ErrFriendUnavailable) {
			ack(msgUser)
			return
		}
		if err != nil {
			h.storeFailed(c, ack, err)
			return
		}
		if state == storage.FriendAccepted && before == storage.FriendIncoming {
			// The client ORs flags into an existing entry, so the pending entry
			// goes before the accepted one is added.
			h.dropBoth(self, other)
		}
		if state == before {
			break
		}
		theirs := storage.FriendIncoming
		if state == storage.FriendAccepted {
			theirs = storage.FriendAccepted
		}
		h.notifyRoster(self.Name, other, "A", state)
		h.notifyRoster(other.Name, self, "A", theirs)
		if state == storage.FriendAccepted {
			h.exchangePresence(ctx, c, self, other)
		}
	case m.Service == "RRSP":
		answer, _ := m.GetFold("answ")
		accept := strings.EqualFold(answer, "Y")
		changed, err := social.AnswerFriend(ctx, gameKey, self.Name, other.Name, accept)
		if err != nil {
			h.storeFailed(c, ack, err)
			return
		}
		if !changed && !(accept && before == storage.FriendAccepted) {
			ack(msgUser)
			return
		}
		if changed {
			h.dropBoth(self, other)
			if accept {
				h.notifyRoster(self.Name, other, "A", storage.FriendAccepted)
				h.notifyRoster(other.Name, self, "A", storage.FriendAccepted)
				h.exchangePresence(ctx, c, self, other)
			}
		}
	default: // RDEL or RDEM on the buddy list
		changed, err := social.ChangeBuddy(ctx, gameKey, self.Name, other.Name, "remove")
		if err != nil {
			h.storeFailed(c, ack, err)
			return
		}
		// A D for a name the client does not hold creates an empty entry in the
		// TimeSplitters build, so only send it for a friendship that existed.
		if changed {
			h.dropBoth(self, other)
		}
	}
	ack(msgOK)
}

// dropBoth removes each persona from the other's roster on screen.
func (h *messenger) dropBoth(a, b storage.Persona) {
	h.notifyRoster(a.Name, b, "D", 0)
	h.notifyRoster(b.Name, a, "D", 0)
}

// exchangePresence tells two new friends about each other's presence.
func (h *messenger) exchangePresence(_ context.Context, c *aries.Client, self, other storage.Persona) {
	if fields := h.presenceOf(c); fields != nil {
		for _, o := range h.online(other.Name) {
			if o.ready {
				o.c.Send(h.presencePacket(self.Name, fields))
			}
		}
	}
	if fields := h.latest(other.Name); fields != nil {
		for _, o := range h.online(self.Name) {
			if o.ready {
				o.c.Send(h.presencePacket(other.Name, fields))
			}
		}
	}
}

func (h *messenger) storeFailed(c *aries.Client, ack func(uint32, ...fesl.Field), err error) {
	h.srv.opts.Log.Error("messenger store failed", "conn", c.ID(), "error", err)
	ack(msgUser)
}

func (h *messenger) send(ctx context.Context, c *aries.Client, m fesl.Message, p *presence, ack func(uint32, ...fesl.Field)) {
	other, ok := h.target(ctx, p.id.personaName, m)
	body, _ := m.GetFold("body")
	subject, _ := m.GetFold("subj")
	kind, _ := m.GetFold("type")
	if kind == "" {
		kind = "C"
	}
	if !ok || len(body) > maxBody || len(subject) > maxSubject || !utf8.ValidString(body+subject) || (kind != "C" && kind != "A") {
		ack(msgUser)
		return
	}
	payload, _ := json.Marshal(mailBody{Type: kind, Subject: subject, Body: body})
	mail, queued, err := h.srv.social.QueueMail(ctx, gameKey, storage.Mail{From: p.id.personaName, To: other.Name, Kind: mailKind, Body: string(payload)})
	if err != nil {
		h.storeFailed(c, ack, err)
		return
	}
	if !queued {
		// A full mailbox, or one side has blocked the other.
		h.srv.opts.Log.Info("messenger message refused", "conn", c.ID(), "target", other.Name)
		ack(msgFull)
		return
	}
	h.srv.opts.Log.Info("messenger message queued", "conn", c.ID(), "message_id", mail.ID, "target", other.Name)
	ack(msgOK, fesl.Field{Key: "ERRS", Value: "0"})
	h.deliver(ctx, other.Name)
}

// deliver pushes queued messages to a persona's ready connections and removes
// each one once it is queued for sending.
func (h *messenger) deliver(ctx context.Context, name string) {
	h.deliverMu.Lock()
	defer h.deliverMu.Unlock()
	var ready []peer
	for _, o := range h.online(name) {
		if o.ready {
			ready = append(ready, o)
		}
	}
	if len(ready) == 0 {
		return
	}
	mails, err := h.srv.social.Mailbox(ctx, gameKey, name)
	if err != nil {
		h.srv.opts.Log.Error("messenger mailbox read failed", "error", err)
		return
	}
	for _, mail := range mails {
		if mail.Kind != mailKind {
			continue
		}
		var b mailBody
		if json.Unmarshal([]byte(mail.Body), &b) != nil {
			b = mailBody{Type: "C"}
		}
		packet := fesl.Packet("RECV", msgOK,
			fesl.Field{Key: "TYPE", Value: b.Type},
			fesl.Field{Key: "USER", Value: mail.From},
			fesl.Field{Key: "DOMN", Value: msgDomain},
			fesl.Field{Key: "RSRC", Value: msgResource},
			fesl.Field{Key: "TIME", Value: strconv.FormatInt(mail.Sent.Unix(), 10)},
			fesl.Field{Key: "SUBJ", Value: b.Subject},
			fesl.Field{Key: "BODY", Value: b.Body},
			fesl.Field{Key: "NOREPLY", Value: "0"},
		)
		for _, o := range ready {
			o.c.Send(packet)
		}
		if err := h.srv.social.AckMail(ctx, gameKey, name, mail.ID); err != nil {
			h.srv.opts.Log.Error("messenger mailbox ack failed", "error", err)
			return
		}
		h.srv.opts.Log.Info("messenger message delivered", "message_id", mail.ID, "connections", len(ready))
	}
}

// validID accepts the decimal transaction identifier the client echoes.
func validID(s string) bool {
	if s == "" || len(s) > 10 {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
