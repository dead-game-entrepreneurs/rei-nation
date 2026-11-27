package tsfp

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dead-game-entrepreneurs/rei-nation/internal/protocol/aries"
	"github.com/dead-game-entrepreneurs/rei-nation/internal/storage"
)

const messengerDomain = "messaging.ea.com"
const messengerResource = "CSO/TSFP-PS2-2005"

type messenger struct {
	mu       sync.Mutex
	friendMu sync.Mutex
	keys     *loginKeys
	store    storage.TSFPSocialStore
	clients  map[*aries.Client]*buddySession
	delivery map[string]*mailDelivery
}

type buddySession struct {
	persona  storage.Persona
	presence map[string]string
}

type mailDelivery struct {
	requested bool
}

func newMessenger(keys *loginKeys, identity storage.IdentityStore) *messenger {
	store, _ := identity.(storage.TSFPSocialStore)
	return &messenger{keys: keys, store: store, clients: make(map[*aries.Client]*buddySession),
		delivery: make(map[string]*mailDelivery)}
}

func (m *messenger) Handle(ctx context.Context, c *aries.Client, in aries.Frame) {
	f := fields(in)
	id := fallback(f, "ID", "0")
	if !transactionID(id) {
		return
	}
	tag := in.Tag
	reply := func(status string, pairs ...string) {
		c.Send(packet(tag+status, append([]string{"ID", id}, pairs...)...))
	}
	m.mu.Lock()
	s := m.clients[c]
	if tag == "AUTH" {
		p, ok := m.keys.lookup(f["LKEY"])
		if ok && (s == nil || s.persona.ID == p.ID) {
			if s == nil {
				m.clients[c] = &buddySession{persona: p}
			}
			c.ClearInitialReadDeadline()
			m.mu.Unlock()
			reply("", "USER", p.Name, "TITL", "", "TIID", "0")
			return
		}
		m.mu.Unlock()
		reply("auth")
		return
	}
	m.mu.Unlock()
	if s == nil {
		reply("auth")
		return
	}
	persona := s.persona
	switch tag {
	case "DISC":
		c.Close()
	case "PING":
		reply("")
	case "PSET":
		m.mu.Lock()
		s.presence = make(map[string]string)
		for _, key := range []string{"SHOW", "STAT", "PROD", "ATTR", "SESS", "TITL", "TIID", "EXTR"} {
			if val, ok := f[key]; ok {
				s.presence[key] = short(val, 255)
			}
		}
		m.mu.Unlock()
		reply("")
		m.publishPresence(ctx, persona)
		m.flush(ctx, persona.Name)
	case "USCH":
		m.search(ctx, c, id, f)
	case "SEND", "BRDC":
		switch m.queue(ctx, persona, f) {
		case "user":
			reply("user")
		case "full":
			reply("full")
		default:
			reply("", "ERRS", "0")
			m.flush(ctx, userName(f["USER"]))
		}
	case "RGET":
		m.roster(ctx, c, id, persona.Name, f)
	case "PADD", "PDEL":
		buddy, ok := m.target(ctx, f["USER"])
		if !ok || !m.accepted(ctx, persona.Name, buddy.ID) {
			reply("user")
			return
		}
		reply("")
		if tag == "PADD" {
			presence, _ := m.presence(buddy.ID)
			c.Send(presencePacket(buddy.Name, presence))
		}
	case "RADD", "RDEL":
		if strings.EqualFold(f["LIST"], "I") {
			reply(m.block(ctx, persona, f["USER"], tag))
		} else if tag == "RDEL" {
			reply(m.friend(ctx, persona, f, tag))
		}
	case "RADM", "RRSP", "RDEM":
		reply(m.friend(ctx, persona, f, tag))
	}
}

func userName(value string) string {
	name, _, _ := strings.Cut(value, "@")
	name, _, _ = strings.Cut(name, "/")
	return name
}

func (m *messenger) target(ctx context.Context, name string) (storage.Persona, bool) {
	name = userName(name)
	if m.store == nil || !validIdentity(name) {
		return storage.Persona{}, false
	}
	p, ok, err := m.store.TSFPPersona(ctx, name)
	return p, ok && err == nil
}

func (m *messenger) search(ctx context.Context, c *aries.Client, id string, f map[string]string) {
	limit, err := strconv.Atoi(fallback(f, "MAXR", "20"))
	if err != nil {
		limit = 20
	}
	query := strings.TrimSpace(userName(f["USER"]))
	var found []storage.Persona
	if m.store != nil && len(query) <= 32 && query != "" && limit > 0 && !strings.ContainsRune(query, 0) {
		pattern := strings.NewReplacer("\\", "\\\\", "%", "\\%", "_", "\\_", "*", "%", "?", "_").Replace(query)
		if !strings.ContainsAny(query, "*?") {
			pattern += "%"
		}
		found, _ = m.store.TSFPSearch(ctx, pattern, min(limit, 20))
	}
	c.Send(packet("USCH", "ID", id, "SIZE", strconv.Itoa(len(found))))
	for _, p := range found {
		c.Send(packet("USER", "USER", p.Name, "DOMN", messengerDomain, "RSRC", messengerResource))
	}
}

func (m *messenger) queue(ctx context.Context, persona storage.Persona, f map[string]string) string {
	target, ok := m.target(ctx, f["USER"])
	body, subject, kind := f["BODY"], f["SUBJ"], fallback(f, "TYPE", "C")
	if !ok || target.ID == persona.ID || len(body) > 255 || len(subject) > 31 ||
		(kind != "C" && kind != "A") || strings.ContainsRune(subject, 0) {
		return "user"
	}
	allowed, err := m.store.CanContact(ctx, gameKey, persona.Name, target.Name)
	if err != nil || !allowed {
		return "user"
	}
	// TSFP consumes queued messages after delivery and has no inbox replay.
	mail := storage.Mail{From: persona.Name, To: target.Name, Kind: "tsfp-" + kind, Body: subject + "\x00" + body}
	_, queued, err := m.store.QueueMail(ctx, gameKey, mail)
	if err != nil || !queued {
		return "full"
	}
	return ""
}

func (m *messenger) flush(ctx context.Context, name string) {
	p, ok := m.target(ctx, name)
	if !ok {
		return
	}
	m.mu.Lock()
	if pending := m.delivery[p.Name]; pending != nil {
		pending.requested = true
		m.mu.Unlock()
		return
	}
	run := &mailDelivery{}
	m.delivery[p.Name] = run
	m.mu.Unlock()
	go func() {
		for {
			deliveryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			m.deliver(deliveryCtx, p)
			cancel()
			m.mu.Lock()
			if !run.requested {
				delete(m.delivery, p.Name)
				m.mu.Unlock()
				return
			}
			run.requested = false
			m.mu.Unlock()
		}
	}()
}

func (m *messenger) deliver(ctx context.Context, p storage.Persona) {
	recipients := m.online(p.ID, true)
	if len(recipients) == 0 {
		return
	}
	pending, err := m.store.Mailbox(ctx, gameKey, p.Name)
	if err != nil {
		return
	}
	for _, mail := range pending {
		if mail.Delivered || !strings.HasPrefix(mail.Kind, "tsfp-") {
			continue
		}
		subject, message, _ := strings.Cut(mail.Body, "\x00")
		out := packet("RECV", "TYPE", strings.TrimPrefix(mail.Kind, "tsfp-"), "USER", mail.From,
			"DOMN", messengerDomain, "RSRC", messengerResource, "TIME", strconv.FormatInt(mail.Sent.Unix(), 10),
			"SUBJ", subject, "BODY", message, "NOREPLY", "0")
		written := make(chan struct{}, len(recipients))
		for _, c := range recipients {
			c.SendAfter(out, func() { written <- struct{}{} })
		}
		timer := time.NewTimer(5 * time.Second)
		var delivered bool
		select {
		case <-written:
			delivered = true
		case <-timer.C:
		case <-ctx.Done():
		}
		timer.Stop()
		if !delivered || m.store.AckMail(ctx, gameKey, p.Name, mail.ID) != nil {
			return
		}
	}
}

func (m *messenger) online(id int64, ready bool) []*aries.Client {
	m.mu.Lock()
	defer m.mu.Unlock()
	var result []*aries.Client
	for c, s := range m.clients {
		if s.persona.ID == id && (!ready || s.presence != nil) && !c.Closed() {
			result = append(result, c)
		}
	}
	return result
}

func (m *messenger) presence(id int64) (map[string]string, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var latest uint64
	var presence map[string]string
	for c, s := range m.clients {
		if s.persona.ID == id && !c.Closed() && c.ID() > latest {
			latest, presence = c.ID(), s.presence
		}
	}
	if latest != 0 && presence == nil {
		presence = map[string]string{}
	}
	return presence, latest != 0
}

func presencePacket(name string, f map[string]string) aries.Frame {
	show := "DISC"
	if f != nil {
		show = fallback(f, "SHOW", "CHAT")
	}
	return packet("PGET", "USER", name, "DOMN", messengerDomain, "RSRC", messengerResource, "SHOW", show,
		"STAT", f["STAT"], "PROD", f["PROD"], "ATTR", f["ATTR"], "SESS", f["SESS"], "TITL", f["TITL"],
		"TIID", fallback(f, "TIID", "0"), "EXTR", f["EXTR"])
}

func (m *messenger) friends(ctx context.Context, self string) []storage.TSFPBuddy {
	if m.store == nil {
		return nil
	}
	roster, _ := m.store.TSFPRoster(ctx, self)
	return roster
}

func (m *messenger) accepted(ctx context.Context, self string, target int64) bool {
	for _, b := range m.friends(ctx, self) {
		if b.ID == target && b.State == "S" {
			return true
		}
	}
	return false
}

func (m *messenger) roster(ctx context.Context, c *aries.Client, id, self string, f map[string]string) {
	var roster []storage.TSFPBuddy
	switch strings.ToUpper(fallback(f, "LIST", "B")) {
	case "B":
		for _, b := range m.friends(ctx, self) {
			if b.State == "S" || strings.EqualFold(f["PEND"], "Y") {
				roster = append(roster, b)
			}
		}
	case "I":
		if m.store != nil {
			ignored, _ := m.store.Roster(ctx, gameKey, self, "I")
			for _, name := range ignored {
				if p, ok := m.target(ctx, name); ok {
					roster = append(roster, storage.TSFPBuddy{Persona: p})
				}
			}
		}
	}
	c.Send(packet("RGET", "ID", id, "SIZE", strconv.Itoa(len(roster))))
	for _, b := range roster {
		c.Send(packet("ROST", "USER", b.Name, "UID", strconv.FormatInt(b.ID, 10), "DOMN", messengerDomain,
			"RSRC", messengerResource, "GROUP", "", "ATTR", b.State))
	}
	if strings.EqualFold(f["PRES"], "Y") {
		for _, b := range roster {
			if presence, online := m.presence(b.ID); b.State == "S" && online {
				c.Send(presencePacket(b.Name, presence))
			}
		}
	}
}

func (m *messenger) friend(ctx context.Context, self storage.Persona, f map[string]string, tag string) string {
	m.friendMu.Lock()
	defer m.friendMu.Unlock()
	other, ok := m.target(ctx, f["USER"])
	if !ok || self.ID == other.ID {
		return "user"
	}
	operation := "remove"
	switch tag {
	case "RADM":
		operation = "request"
	case "RRSP":
		operation = "decline"
		if strings.EqualFold(f["ANSW"], "Y") {
			operation = "accept"
		}
	}
	before, after, err := m.store.TSFPFriendship(ctx, self.Name, other.Name, operation)
	if err != nil || operation == "request" && after == "" || operation == "accept" && after != "S" ||
		operation == "decline" && before != "R" {
		return "user"
	}
	m.notifyFriend(self, other, before, after)
	if before != after && after == "S" {
		m.publishPresence(ctx, self)
		m.publishPresence(ctx, other)
	}
	return ""
}

func (m *messenger) block(ctx context.Context, self storage.Persona, name, tag string) string {
	m.friendMu.Lock()
	defer m.friendMu.Unlock()
	other, ok := m.target(ctx, name)
	if !ok || self.ID == other.ID {
		return "user"
	}
	operation := "unblock"
	if tag == "RADD" {
		operation = "block"
	}
	before := ""
	if tag == "RADD" {
		for _, b := range m.friends(ctx, self.Name) {
			if b.ID == other.ID {
				before = b.State
				break
			}
		}
	}
	changed, err := m.store.ChangeBuddy(ctx, gameKey, self.Name, other.Name, operation)
	if err != nil || !changed {
		return "user"
	}
	if before != "" {
		m.notifyFriend(self, other, before, "")
	}
	return ""
}

func (m *messenger) notifyFriend(self, other storage.Persona, before, after string) {
	if before == after {
		return
	}
	if before != "" {
		m.rosterNotice(self.ID, other, "D", "")
		m.rosterNotice(other.ID, self, "D", "")
	}
	if after != "" {
		reciprocal := after
		if after == "T" {
			reciprocal = "R"
		}
		m.rosterNotice(self.ID, other, "A", after)
		m.rosterNotice(other.ID, self, "A", reciprocal)
	}
}

func (m *messenger) rosterNotice(to int64, other storage.Persona, change, attr string) {
	frame := packet("RNOT", "USER", other.Name, "UID", strconv.FormatInt(other.ID, 10), "GROUP", "", "CHNG", change, "ATTR", attr)
	for _, c := range m.online(to, false) {
		c.Send(frame)
	}
}

func (m *messenger) publishPresence(ctx context.Context, self storage.Persona) {
	presence, _ := m.presence(self.ID)
	out := presencePacket(self.Name, presence)
	for _, b := range m.friends(ctx, self.Name) {
		if b.State == "S" {
			for _, c := range m.online(b.ID, false) {
				c.Send(out)
			}
		}
	}
}

func (m *messenger) Disconnected(c *aries.Client) {
	m.mu.Lock()
	s := m.clients[c]
	delete(m.clients, c)
	m.mu.Unlock()
	if s != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		m.publishPresence(ctx, s.persona)
	}
}
