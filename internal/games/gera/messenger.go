package gera

import (
	"context"
	"sync"

	"github.com/dead-game-entrepreneurs/rei-nation/internal/protocol/aries"
	"github.com/dead-game-entrepreneurs/rei-nation/internal/protocol/fesl"
)

// EA Messenger status codes are four-character tags carried in the frame's code
// word. The client reads the tag, not a numeric value.
const (
	msgOK   uint32 = 0
	msgAuth uint32 = 0x61757468 // "auth": not authenticated
	msgUser uint32 = 0x75736572 // "user": no such user
)

// messenger is the EA Messenger presence service on the aux port. The client
// opens it right after persona login and will not consider itself online until
// the AUTH handshake succeeds. It shares the server's session-key store so the
// key issued at FESL login identifies the player here.
//
// The roster, search and message transactions are acknowledged so the client
// does not stall, but buddy lists are not stored yet; see the online map.
type messenger struct {
	srv *server

	mu       sync.Mutex
	sessions map[*aries.Client]*presence
}

type presence struct {
	id     identity
	authed bool
	show   string
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

func (h *messenger) Disconnected(c *aries.Client) {
	h.mu.Lock()
	delete(h.sessions, c)
	h.mu.Unlock()
	h.srv.opts.Log.Info("messenger client disconnected", "conn", c.ID(), "reason", c.CloseReason())
}

func (h *messenger) Handle(_ context.Context, c *aries.Client, f aries.Frame) {
	m := fesl.Decode(f)
	ident, _ := m.GetFold("id")
	if !validID(ident) {
		ident = "0"
	}
	p := h.session(c)

	if m.Service == "AUTH" {
		h.auth(c, p, m, ident)
		return
	}
	if !p.authed {
		c.Send(fesl.Packet(m.Service, msgAuth, fesl.Field{Key: "ID", Value: ident}))
		return
	}
	switch m.Service {
	case "PING":
		c.Send(fesl.Packet("PING", msgOK, fesl.Field{Key: "ID", Value: ident}))
	case "PSET":
		if show, ok := m.GetFold("show"); ok {
			p.show = show
		}
		c.Send(fesl.Packet("PSET", msgOK, fesl.Field{Key: "ID", Value: ident}))
	case "RGET":
		// Roster: empty until buddy lists are stored.
		c.Send(fesl.Packet("RGET", msgOK, fesl.Field{Key: "ID", Value: ident}, fesl.Field{Key: "SIZE", Value: "0"}))
	case "USCH":
		// Persona search: no results yet.
		c.Send(fesl.Packet("USCH", msgOK, fesl.Field{Key: "ID", Value: ident}, fesl.Field{Key: "SIZE", Value: "0"}))
	case "DISC":
		c.CloseWithReason("messenger disconnect")
	default:
		// Acknowledge unknown transactions so the client does not wait on them.
		h.srv.opts.Log.Info("unhandled messenger transaction", "conn", c.ID(), "kind", m.Service)
		c.Send(fesl.Packet(m.Service, msgOK, fesl.Field{Key: "ID", Value: ident}))
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
	p.id = id
	p.authed = true
	h.srv.opts.Log.Info("messenger auth", "conn", c.ID(), "persona", id.personaName, "persona_id", id.personaID)
	c.Send(fesl.Packet("AUTH", msgOK,
		fesl.Field{Key: "ID", Value: ident},
		fesl.Field{Key: "USER", Value: id.personaName},
		fesl.Field{Key: "TITL", Value: ""},
		fesl.Field{Key: "TIID", Value: "0"},
	))
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
