package gera

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"maps"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dead-game-entrepreneurs/rei-nation/internal/games/gera/relay"
	"github.com/dead-game-entrepreneurs/rei-nation/internal/protocol/aries"
	"github.com/dead-game-entrepreneurs/rei-nation/internal/protocol/fesl"
)

// theater is the EA game browser on TCP and UDP 18041: listing, hosting, joining
// and endpoint discovery. See docs/games/gera/online-map.md#theater-game-browser.
type theater struct {
	srv *server

	mu       sync.Mutex
	sessions map[*aries.Client]*theaterSession
	games    map[int]*hostedGame
	connIP   map[*aries.Client]string // theater TCP peer IP, for ECHO attribution
	observed map[*aries.Client]addr   // latest UDP endpoint seen for a connection
	nextGID  int
	// guests records, per host persona, when each guest persona was last
	// admitted to one of its games. The host reports stats for its guests.
	guests map[int64]map[int64]time.Time
}

// guestReportWindow is how long after admitting a guest a host may report
// that guest's stats. A match and its results screen fit well inside it.
const guestReportWindow = 3 * time.Hour

// hostedGuest reports whether guest joined a game hosted by host recently.
func (h *theater) hostedGuest(host, guest int64) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	t, ok := h.guests[host][guest]
	return ok && time.Since(t) < guestReportWindow
}

type theaterSession struct {
	id     identity
	authed bool
}

// addr is an observed or reported UDP endpoint.
type addr struct {
	ip   string
	port int
}

// pcsx2SocketIP is the fixed internal address PCSX2's sockets adapter reports
// for every emulated console, so it never distinguishes one peer from another.
const pcsx2SocketIP = "192.0.2.100"

type hostedGame struct {
	gid          int
	owner        *aries.Client
	host         identity
	lid          string
	name         string
	max          int
	port         int
	hostIP       string
	secret       string
	internalIP   string
	internalPort int
	join         string // O(pen), C(losed), W(aiting)
	attrs        map[string]string
	details      map[string]string
	players      map[int]*gamePlayer
	pending      map[int]*gamePlayer
	nextPID      int

	// Relay fallback state; see relaypolicy.go. relayReachable is nil until
	// the relays have probed the host, then the relays it answered.
	relayRoom      *relay.Room
	relayReachable map[string]bool
	relayProbedEP  netip.AddrPort // the host endpoint relayReachable was measured at
	relayProbedAt  time.Time
	relayProbing   bool
	relayMu        sync.Mutex // serializes opening and growing relayRoom

	// canceled holds PIDs whose joiner cancelled while the host was deciding.
	canceled map[int]time.Time
}

type gamePlayer struct {
	writer  *aries.Client
	name    string
	uid     int64
	ip      string
	port    int
	ticket  string
	lid     string // the lobby the joiner asked from; echoed in EGEG
	entered bool
	attrs   map[string]string

	approvedAt time.Time
	relayMode  string // "", "fallback" or "relay"
	relayed    bool   // the host was given this player's relay address
	// opening delivers the fallback room (or nil) once the joiner is in it.
	opening chan *relay.Room
}

func (h *theater) session(c *aries.Client) *theaterSession {
	h.mu.Lock()
	defer h.mu.Unlock()
	t := h.sessions[c]
	if t == nil {
		t = &theaterSession{}
		h.sessions[c] = t
		h.connIP[c] = c.Addr().String()
	}
	return t
}

func (h *theater) Disconnected(c *aries.Client) {
	var notices []removal
	var kicks []kick
	h.mu.Lock()
	for _, g := range h.games {
		if g.owner == c {
			notices = append(notices, h.closeGame(g)...)
			continue
		}
		// Drop this connection's membership from any game it had joined.
		kicks = append(kicks, h.leaveGame(g, c)...)
	}
	delete(h.sessions, c)
	delete(h.connIP, c)
	delete(h.observed, c)
	h.mu.Unlock()
	sendRemovals(notices)
	h.sendKicks(kicks, "disconnected")
	h.srv.opts.Log.Info("theater client disconnected", "conn", c.ID(), "reason", c.CloseReason())
}

// kick is a KICK notice owed to a host for a guest that left.
type kick struct {
	host *aries.Client
	gid  int
	pid  int
}

// leaveGame removes c and returns KICKs for approved guests, whose seats the host
// holds until told. Pending guests are remembered as cancelled. Callers hold h.mu.
func (h *theater) leaveGame(g *hostedGame, c *aries.Client) []kick {
	var out []kick
	for pid, p := range g.players {
		if p.writer == c {
			h.noteFailedJoin(g, pid, p)
			h.releaseRelay(g, pid)
			delete(g.players, pid)
			if c != g.owner {
				out = append(out, kick{host: g.owner, gid: g.gid, pid: pid})
			}
		}
	}
	now := time.Now()
	for pid, t := range g.canceled {
		if now.Sub(t) > canceledJoinMemory {
			delete(g.canceled, pid)
		}
	}
	for pid, p := range g.pending {
		if p.writer == c {
			h.releaseRelay(g, pid)
			delete(g.pending, pid)
			if c != g.owner {
				g.canceled[pid] = now
			}
		}
	}
	return out
}

// canceledJoinMemory is how long a late host approval of a cancelled join is
// recognised and answered with KICK.
const canceledJoinMemory = 2 * time.Minute

func (h *theater) sendKicks(kicks []kick, reason string) {
	for _, k := range kicks {
		k.host.Send(fesl.Packet("KICK", msgOK, field("PID", strconv.Itoa(k.pid))))
		h.srv.opts.Log.Info("theater notified host of departure", "host_conn", k.host.ID(), "gid", k.gid,
			"pid", k.pid, "reason", reason)
	}
}

// removal is a GREM notice owed to a player whose game is gone.
type removal struct {
	to  *aries.Client
	lid string
	gid int
}

// closeGame deletes g and returns the GREM notices owed to its other players and
// pending joiners, each in its own lobby. Callers hold h.mu.
func (h *theater) closeGame(g *hostedGame) []removal {
	delete(h.games, g.gid)
	if g.relayRoom != nil {
		go h.srv.relays.CloseRoom(g.relayRoom)
		g.relayRoom = nil
	}
	var out []removal
	seen := map[*aries.Client]bool{g.owner: true}
	for _, list := range []map[int]*gamePlayer{g.players, g.pending} {
		for _, p := range list {
			if seen[p.writer] {
				continue
			}
			seen[p.writer] = true
			lid := p.lid
			if lid == "" {
				lid = g.lid
			}
			out = append(out, removal{to: p.writer, lid: lid, gid: g.gid})
		}
	}
	return out
}

func sendRemovals(notices []removal) {
	for _, n := range notices {
		n.to.Send(fesl.Packet("GREM", msgOK, field("LID", n.lid), field("GID", strconv.Itoa(n.gid))))
	}
}

func (h *theater) Handle(_ context.Context, c *aries.Client, f aries.Frame) {
	m := fesl.Decode(f)
	tid, _ := m.GetFold("tid")
	if !validID(tid) {
		tid = "0"
	}
	if m.Service != "PING" {
		h.srv.opts.Log.Info("theater request", "conn", c.ID(), "kind", m.Service, "tid", tid,
			"code", fmt.Sprintf("%#08x", m.Code), "fields", loggableFields(m))
	}
	switch m.Service {
	case "CONN":
		c.Send(fesl.Packet("CONN", msgOK, field("TID", tid), field("PROT", "2"), field("TIME", "0")))
	case "USER":
		h.user(c, m, tid)
	case "LLST":
		h.lobbyList(c, tid)
	case "GLST":
		h.gameList(c, m, tid)
	case "GDAT":
		h.gameData(c, m, tid)
	case "CGAM":
		h.createGame(c, m, tid)
	case "EGAM":
		h.enterGame(c, m, tid)
	case "EGRS":
		h.joinResponse(c, m, tid)
	case "ECNL":
		h.cancelJoin(c, m, tid)
	case "PLVT":
		h.playerLeft(c, m, tid)
	case "PENT":
		h.playerEntered(c, m, tid)
	case "UGAM", "UGDE", "UPLA", "KEEP", "UBRA":
		h.hostReport(c, m, tid)
	case "RGAM":
		h.removeGame(c, tid)
	case "PING":
		// Client keepalive; no reply expected.
	default:
		h.srv.opts.Log.Info("unhandled theater transaction", "conn", c.ID(), "kind", m.Service)
	}
}

func field(key, value string) fesl.Field { return fesl.Field{Key: key, Value: value} }

func (h *theater) user(c *aries.Client, m fesl.Message, tid string) {
	lkey, _ := m.GetFold("lkey")
	id, ok := h.srv.lookupKey(lkey)
	if !ok || id.personaID == 0 {
		h.srv.opts.Log.Info("theater auth rejected", "conn", c.ID())
		c.Send(fesl.Packet("USER", msgAuth, field("TID", tid)))
		return
	}
	t := h.session(c)
	h.mu.Lock()
	t.id = id
	t.authed = true
	h.mu.Unlock()
	c.ClearInitialReadDeadline()
	h.srv.opts.Log.Info("theater auth", "conn", c.ID(), "persona", id.personaName, "persona_id", id.personaID)
	c.Send(fesl.Packet("USER", msgOK, field("TID", tid), field("NAME", id.personaName)))
	if h.srv.relays.Available() {
		go h.measurePlayer(c)
	}
}

// lobbyName matches this disc's region string; with no matching lobby the client
// fails with nrom. See docs/games/gera/online-map.md#lobbies.
const lobbyName = "NorthAmerica"

// lobbies are offered in order, LID = index + 1; "Asia" covers consoles whose
// language setting selects that region string.
var lobbies = []string{lobbyName, "Asia"}

func (h *theater) lobbyList(c *aries.Client, tid string) {
	c.Send(fesl.Packet("LLST", msgOK, field("TID", tid), field("NUM-LOBBIES", strconv.Itoa(len(lobbies)))))
	for i, name := range lobbies {
		c.Send(fesl.Packet("LDAT", msgOK,
			field("TID", tid), field("LID", strconv.Itoa(i+1)), field("NAME", name), field("PASSING", "1"),
			field("FAVORITE-GAMES", "0"), field("FAVORITE-PLAYERS", "1"), field("LOCALE", "en_US"),
			field("MAX-GAMES", "100"), field("NUM-GAMES", "0"),
		))
	}
}

func (h *theater) gameList(c *aries.Client, m fesl.Message, tid string) {
	lid, ok := m.GetFold("lid")
	if !ok || !validID(lid) {
		lid = "1"
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	// Every lobby lists every game, since consoles on one network can pick
	// different lobbies from their region settings.
	n := strconv.Itoa(len(h.games))
	c.Send(fesl.Packet("GLST", msgOK, field("TID", tid), field("LID", lid),
		field("NUM-GAMES", n), field("LOBBY-MAX-GAMES", "100"), field("LOBBY-NUM-GAMES", n)))
	for _, g := range h.games {
		c.Send(fesl.Packet("GDAT", msgOK, h.gameDataFields(g, tid, lid, c.Addr().String())...))
	}
}

func (h *theater) gameData(c *aries.Client, m fesl.Message, tid string) {
	gid, ok := parseInt(m, "gid")
	if !ok {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	g := h.games[gid]
	if g == nil {
		return
	}
	lid, ok := m.GetFold("lid")
	if !ok || !validID(lid) {
		lid = g.lid
	}
	c.Send(fesl.Packet("GDAT", msgOK, h.gameDataFields(g, tid, lid, c.Addr().String())...))
	details := []fesl.Field{field("TID", tid), field("LID", lid), field("GID", strconv.Itoa(gid)), field("UGID", strconv.Itoa(gid))}
	for k, v := range g.details {
		details = append(details, field(k, v))
	}
	c.Send(fesl.Packet("GDET", msgOK, details...))
	for pid, p := range g.players {
		if !p.entered {
			continue
		}
		pf := []fesl.Field{field("TID", tid), field("LID", lid), field("GID", strconv.Itoa(gid)),
			field("PID", strconv.Itoa(pid)), field("NAME", p.name), field("UID", strconv.FormatInt(p.uid, 10))}
		for k, v := range p.attrs {
			pf = append(pf, field(k, v))
		}
		c.Send(fesl.Packet("PDAT", msgOK, pf...))
	}
}

// gameDataFields builds a GDAT body for a game, reported in the viewer's
// lobby. Callers hold h.mu.
func (h *theater) gameDataFields(g *hostedGame, tid, lid, viewerIP string) []fesl.Field {
	ext := h.reachable(h.externalOf(g.owner, g.hostIP, g.port), viewerIP)
	active, joining := playerCounts(g)
	out := []fesl.Field{
		field("TID", tid), field("LID", lid), field("GID", strconv.Itoa(g.gid)),
		field("N", g.name), field("MP", strconv.Itoa(g.max)), field("I", ext.ip), field("P", strconv.Itoa(ext.port)),
		field("PW", "0"), field("AP", strconv.Itoa(active)), field("JP", strconv.Itoa(joining)), field("QP", "0"),
		field("HN", g.host.personaName), field("HU", strconv.FormatInt(g.host.personaID, 10)),
		field("V", "1.0"), field("F", "0"), field("NF", "0"), field("PL", "PS2"), field("J", g.join),
	}
	for k, v := range g.attrs {
		out = append(out, field(k, v))
	}
	return out
}

func (h *theater) createGame(c *aries.Client, m fesl.Message, tid string) {
	t := h.session(c)
	if !t.authed {
		h.srv.opts.Log.Info("theater CGAM before auth", "conn", c.ID())
		return
	}
	lid, ok := m.GetFold("lid")
	if !ok || !validID(lid) {
		lid = "1"
	}
	maxP := 8
	if v, ok := parseInt(m, "max-players"); ok && v > 0 {
		maxP = v
	}
	port := 3000
	if v, ok := parseInt(m, "port"); ok && v >= 1 && v <= 65535 {
		port = v
	}
	name, _ := m.GetFold("name")
	if name == "" {
		name = "Game"
	}
	hostIP := c.Addr().String()
	intIP := hostIP
	if v, ok := m.GetFold("int-ip"); ok {
		if _, err := netip.ParseAddr(v); err == nil && v != "0.0.0.0" {
			intIP = v
		}
	}
	intPort := port
	if v, ok := parseInt(m, "int-port"); ok && v >= 1 && v <= 65535 {
		intPort = v
	}

	h.mu.Lock()
	var notices []removal
	for _, g := range h.games { // one current match per host
		if g.owner == c {
			notices = append(notices, h.closeGame(g)...)
		}
	}
	h.nextGID++
	gid := h.nextGID
	h.games[gid] = &hostedGame{
		gid: gid, owner: c, host: t.id, lid: lid, name: name, max: maxP, port: port, hostIP: hostIP,
		secret: newSecret(), internalIP: intIP, internalPort: intPort, join: "O",
		attrs: m.Prefixed("b-"), details: map[string]string{},
		players: map[int]*gamePlayer{}, pending: map[int]*gamePlayer{}, nextPID: 2,
		canceled: map[int]time.Time{},
	}
	created := h.games[gid]
	secret := created.secret
	h.startProbe(created)
	h.mu.Unlock()
	sendRemovals(notices)

	h.srv.opts.Log.Info("theater game created", "conn", c.ID(), "gid", gid, "name", name, "host", t.id.personaName)
	c.Send(fesl.Packet("CGAM", msgOK, field("TID", tid), field("LID", lid), field("GID", strconv.Itoa(gid)),
		field("UGID", strconv.Itoa(gid)), field("SECRET", secret), field("MAX-PLAYERS", strconv.Itoa(maxP))))
}

func (h *theater) enterGame(c *aries.Client, m fesl.Message, tid string) {
	t := h.session(c)
	if !t.authed {
		h.srv.opts.Log.Info("theater EGAM before auth", "conn", c.ID())
		return
	}
	gid, ok := parseInt(m, "gid")
	if !ok {
		return
	}
	lid, _ := m.GetFold("lid")
	port := 3000
	if v, ok := parseInt(m, "port"); ok && v >= 1 && v <= 65535 {
		port = v
	}

	h.mu.Lock()
	g := h.games[gid]
	if g == nil { // any lobby: see gameList
		h.mu.Unlock()
		c.Send(fesl.Packet("EGAM", 0x6E67616D, field("TID", tid))) // "ngam": no such game
		return
	}
	self := g.owner == c
	if !self {
		if len(g.players)+len(g.pending) >= g.max {
			h.mu.Unlock()
			c.Send(fesl.Packet("EGAM", 0x6A64656E, field("TID", tid), field("REASON", "f"))) // "jden": full
			return
		}
		if g.join == "C" {
			h.mu.Unlock()
			c.Send(fesl.Packet("EGAM", 0x6A64656E, field("TID", tid), field("REASON", "c"))) // "jden": closed
			return
		}
	}
	joinerIP := c.Addr().String()
	id := t.id // EGRQ uses the identity admitted here, even if USER changes it later
	relayEP, relayable := h.publicEndpoint(h.externalOf(c, joinerIP, port))
	mode, reason := "", ""
	if !self {
		mode, reason = h.relayDecision(g, id, joinerIP)
		if mode == "refuse" || (!relayable && strings.HasPrefix(reason, "forced_")) {
			h.mu.Unlock()
			h.srv.opts.Log.Warn("theater join refused: forced relay unavailable", "conn", c.ID(), "gid", gid,
				"persona_id", id.personaID, "reason", reason)
			c.Send(fesl.Packet("EGAM", 0x6A64656E, field("TID", tid), field("REASON", "c"))) // "jden"
			return
		}
		if !relayable {
			mode = ""
		}
		switch {
		case reason == "host_not_probed" || reason == "host_endpoint_changed":
			h.startProbe(g)
		case mode != "" && time.Since(g.relayProbedAt) > relayProbeRefresh:
			h.startProbe(g) // mappings and routers change in a long match
		}
	}
	pid := 1
	if !self {
		pid = g.nextPID
		g.nextPID++
	}
	ticket := newSecret()
	if !validID(lid) {
		lid = g.lid
	}
	joiner := &gamePlayer{writer: c, name: id.personaName, uid: id.personaID, ip: joinerIP, port: port,
		ticket: ticket, lid: lid, attrs: map[string]string{}, relayMode: mode}
	g.pending[pid] = joiner
	ext := h.reachable(h.externalOf(c, joinerIP, port), h.connIP[g.owner])
	owner := g.owner
	reachable := g.relayReachable
	hasRoom := g.relayRoom != nil
	h.mu.Unlock()

	c.Send(fesl.Packet("EGAM", msgOK, field("TID", tid), field("REASON", "0"), field("QPOS", "0"), field("QLEN", "0")))
	h.srv.opts.Log.Info("theater join request", "conn", c.ID(), "gid", gid, "pid", pid, "persona", id.personaName)
	if h.srv.relays != nil && !self {
		h.srv.opts.Log.Info("relay decision", "gid", gid, "pid", pid, "persona_id", id.personaID,
			"mode", modeName(mode), "reason", reason)
	}
	egrq := func(a addr) aries.Frame {
		return fesl.Packet("EGRQ", msgOK, field("TID", "0"), field("NAME", joiner.name),
			field("UID", strconv.FormatInt(joiner.uid, 10)), field("PID", strconv.Itoa(pid)),
			field("TICKET", ticket), field("IP", a.ip), field("PORT", strconv.Itoa(a.port)))
	}
	switch {
	case mode == "relay":
		// EGRQ waits for the room so the host's probes find the relay port open.
		// Without a room a forced join ends; any other join goes ahead direct.
		forced := strings.HasPrefix(reason, "forced_")
		go func() {
			room := h.roomMember(g, pid, relayEP, nil)
			address := ext
			h.mu.Lock()
			_, still := g.pending[pid]
			still = still && h.games[gid] == g
			if room != nil && still {
				if ep, ok := h.srv.relays.Port(room, strconv.Itoa(pid)); ok {
					// The host reaches the joiner at its relay port.
					joiner.relayed = true
					address = addr{ep.Addr().String(), int(ep.Port())}
				}
			}
			if still && forced && !joiner.relayed {
				delete(g.pending, pid)
				h.releaseRelay(g, pid)
				h.sendJoinAnswer(c, c, false, lid, gid, pid, "", "1", addr{"0.0.0.0", 0}, addr{"0.0.0.0", 0}, false)
				h.mu.Unlock()
				h.srv.opts.Log.Warn("theater join refused: forced relay room unavailable", "gid", gid, "pid", pid)
				return
			}
			if !joiner.relayed {
				joiner.relayMode = ""
			}
			relayed := joiner.relayed
			if still {
				owner.Send(egrq(address)) // under h.mu, so it cannot follow a removal notice
			}
			h.mu.Unlock()
			if !still {
				return
			}
			h.srv.opts.Log.Info("theater join relayed", "gid", gid, "pid", pid, "relayed", relayed,
				"address", address.ip+":"+strconv.Itoa(address.port))
		}()
		return
	case mode == "fallback":
		opening := make(chan *relay.Room, 1)
		h.mu.Lock()
		joiner.opening = opening
		h.mu.Unlock()
		go func() { opening <- h.roomMember(g, pid, relayEP, reachable) }()
	case hasRoom && !self && relayable:
		// A direct joiner of a match with a relay room joins it too, so
		// relayed players can reach it.
		go h.roomMember(g, pid, relayEP, nil)
	}
	owner.Send(egrq(ext)) // for a self-join, owner == c
}

func (h *theater) joinResponse(c *aries.Client, m fesl.Message, tid string) {
	pid, ok := parseInt(m, "pid")
	if !ok {
		return
	}
	allowed, _ := m.GetFold("allowed")
	approved := allowed == "1" || allowed == "t" || allowed == "T" || allowed == "true"
	reason, _ := m.GetFold("reason")
	if reason == "" {
		reason = "0"
	}

	h.mu.Lock()
	var g *hostedGame
	for _, candidate := range h.games {
		if candidate.owner == c {
			if _, ok := candidate.pending[pid]; ok {
				g = candidate
				break
			}
		}
	}
	if g == nil {
		late := false
		for _, candidate := range h.games {
			if _, ok := candidate.canceled[pid]; ok && candidate.owner == c {
				delete(candidate.canceled, pid)
				late, g = true, candidate
				break
			}
		}
		h.mu.Unlock()
		if late {
			// The joiner cancelled before this answer; free any seat the host reserved.
			if tid != "0" {
				c.Send(fesl.Packet("EGRS", msgOK, field("TID", tid)))
			}
			if approved {
				h.sendKicks([]kick{{host: c, gid: g.gid, pid: pid}}, "cancelled_before_approval")
			}
		}
		return
	}
	pending := g.pending[pid]
	delete(g.pending, pid)
	ticket := pending.ticket
	if !approved {
		ticket = ""
	}
	primary, alternate := h.hostAddresses(g, pending.ip, pending.writer == g.owner)
	if pending.relayMode == "relay" && pending.relayed && g.relayRoom != nil {
		if ep, ok := h.srv.relays.Port(g.relayRoom, hostKey); ok {
			// INT-IP 0 makes the joiner skip its alternate-address retry.
			primary, alternate = addr{ep.Addr().String(), int(ep.Port())}, addr{"0.0.0.0", 0}
		}
	}
	opening := pending.opening
	pending.opening = nil
	if !approved {
		h.releaseRelay(g, pid)
		opening = nil
	}
	if approved {
		pending.approvedAt = time.Now()
		pending.entered = false
		g.players[pid] = pending
		if g.host.personaID != 0 && pending.uid != 0 && pending.uid != g.host.personaID {
			if h.guests == nil {
				h.guests = make(map[int64]map[int64]time.Time)
			}
			if h.guests[g.host.personaID] == nil {
				h.guests[g.host.personaID] = make(map[int64]time.Time)
			}
			now := time.Now()
			for id, t := range h.guests[g.host.personaID] {
				if now.Sub(t) >= guestReportWindow {
					delete(h.guests[g.host.personaID], id)
				}
			}
			h.guests[g.host.personaID][pending.uid] = now
		}
	}
	self := pending.writer == g.owner
	joiner := pending.writer
	gid := g.gid
	lid := pending.lid
	h.mu.Unlock()

	if tid != "0" {
		c.Send(fesl.Packet("EGRS", msgOK, field("TID", tid)))
	}
	if opening != nil {
		// A fallback approval waits briefly for the joiner to be in the room,
		// then offers the host's relay port as the alternate address.
		go func() {
			var room *relay.Room
			select {
			case room = <-opening:
			case <-time.After(relayFallbackWait):
			}
			alt := alternate
			if room != nil {
				if ep, ok := h.srv.relays.Port(room, hostKey); ok {
					alt = addr{ep.Addr().String(), int(ep.Port())}
				}
			}
			h.srv.opts.Log.Info("theater join fallback", "gid", gid, "pid", pid, "relay", room != nil,
				"alternate", alt.ip+":"+strconv.Itoa(alt.port))
			// Answer under h.mu only while the match and player are current, so
			// the answer cannot follow a GREM.
			h.mu.Lock()
			defer h.mu.Unlock()
			if h.games[gid] != g || g.players[pid] != pending {
				h.srv.opts.Log.Info("theater join answer dropped: join ended while waiting", "gid", gid, "pid", pid)
				return
			}
			h.sendJoinAnswer(c, joiner, self, lid, gid, pid, ticket, reason, primary, alt, approved)
		}()
		return
	}
	h.sendJoinAnswer(c, joiner, self, lid, gid, pid, ticket, reason, primary, alternate, approved)
}

// sendJoinAnswer sends EGEG to the joiner; the host's own self-join gets it
// on its connection.
func (h *theater) sendJoinAnswer(c, joiner *aries.Client, self bool, lid string, gid, pid int,
	ticket, reason string, primary, alternate addr, approved bool) {
	egeg := fesl.Packet("EGEG", msgOK, field("TID", "0"), field("LID", lid), field("GID", strconv.Itoa(gid)),
		field("PID", strconv.Itoa(pid)), field("TICKET", ticket), field("REASON", reason),
		field("I", primary.ip), field("P", strconv.Itoa(primary.port)), field("PL", "PS2"), field("UGID", strconv.Itoa(gid)),
		field("INT-IP", alternate.ip), field("INT-PORT", strconv.Itoa(alternate.port)))
	h.srv.opts.Log.Info("theater join answer", "conn", c.ID(), "gid", gid, "pid", pid, "approved", approved,
		"host_endpoint", primary.ip+":"+strconv.Itoa(primary.port))
	if self {
		c.Send(egeg)
	} else {
		joiner.Send(egeg)
	}
}

func (h *theater) cancelJoin(c *aries.Client, m fesl.Message, tid string) {
	gid, ok := parseInt(m, "gid")
	if !ok {
		return
	}
	var kicks []kick
	h.mu.Lock()
	if g := h.games[gid]; g != nil {
		kicks = h.leaveGame(g, c)
	}
	h.mu.Unlock()
	if tid != "0" {
		c.Send(fesl.Packet("ECNL", msgOK, field("TID", tid)))
	}
	h.sendKicks(kicks, "cancelled")
}

func (h *theater) playerLeft(c *aries.Client, m fesl.Message, tid string) {
	pid, ok := parseInt(m, "pid")
	if !ok {
		return
	}
	h.mu.Lock()
	for _, g := range h.games {
		if g.owner == c {
			h.releaseRelay(g, pid)
			delete(g.players, pid)
			delete(g.pending, pid)
		}
	}
	h.mu.Unlock()
	if tid != "0" {
		c.Send(fesl.Packet("PLVT", msgOK, field("TID", tid)))
	}
}

func (h *theater) playerEntered(c *aries.Client, m fesl.Message, tid string) {
	pid, ok := parseInt(m, "pid")
	if !ok {
		return
	}
	h.mu.Lock()
	for _, g := range h.games {
		if g.owner == c {
			if p := g.players[pid]; p != nil {
				p.entered = true
			}
		}
	}
	h.mu.Unlock()
	if tid != "0" {
		c.Send(fesl.Packet("PENT", msgOK, field("TID", tid), field("PID", strconv.Itoa(pid))))
	}
}

func (h *theater) hostReport(c *aries.Client, m fesl.Message, tid string) {
	h.mu.Lock()
	for _, g := range h.games {
		if g.owner != c {
			continue
		}
		switch m.Service {
		case "UGDE":
			if !mergeBounded(g.details, m.Prefixed("d-")) {
				h.srv.opts.Log.Warn("theater details update dropped: too large", "conn", c.ID(), "gid", g.gid)
			}
		case "UGAM":
			if !mergeBounded(g.attrs, m.Prefixed("b-")) {
				h.srv.opts.Log.Warn("theater attribute update dropped: too large", "conn", c.ID(), "gid", g.gid)
			}
			if j, ok := m.GetFold("join"); ok {
				switch j {
				case "O", "C", "W":
					g.join = j
				}
			}
			if name, ok := m.GetFold("name"); ok && name != "" {
				g.name = name
			}
		case "UPLA":
			if pid, ok := parseInt(m, "pid"); ok {
				if p := g.players[pid]; p != nil && !mergeBounded(p.attrs, m.Prefixed("p-")) {
					h.srv.opts.Log.Warn("theater player update dropped: too large", "conn", c.ID(), "gid", g.gid, "pid", pid)
				}
			}
		}
	}
	h.mu.Unlock()
	if tid != "0" {
		c.Send(fesl.Packet(m.Service, msgOK, field("TID", tid)))
	}
}

func (h *theater) removeGame(c *aries.Client, tid string) {
	var notices []removal
	h.mu.Lock()
	for _, g := range h.games {
		if g.owner == c {
			notices = append(notices, h.closeGame(g)...)
		}
	}
	h.mu.Unlock()
	sendRemovals(notices)
	if len(notices) > 0 {
		h.srv.opts.Log.Info("theater game removed", "conn", c.ID(), "notified", len(notices))
	}
	if tid != "0" {
		c.Send(fesl.Packet("RGAM", msgOK, field("TID", tid)))
	}
}

// externalOf returns the UDP-observed endpoint for a connection, or the given
// fallback when none has been seen. Callers hold h.mu.
func (h *theater) externalOf(c *aries.Client, fallbackIP string, fallbackPort int) addr {
	if e, ok := h.observed[c]; ok {
		return e
	}
	return addr{fallbackIP, fallbackPort}
}

// hostAddresses returns the host endpoints for a joiner. The shared PCSX2 internal
// address goes only to the host's own self-join. Callers hold h.mu.
func (h *theater) hostAddresses(g *hostedGame, joinerIP string, self bool) (primary, alternate addr) {
	external := h.externalOf(g.owner, g.hostIP, g.port)
	internal := addr{g.internalIP, g.internalPort}
	switch {
	case joinerIP == g.hostIP && internal.ip == pcsx2SocketIP && !self:
		primary, alternate = external, addr{"0.0.0.0", 0}
	case joinerIP == g.hostIP:
		primary, alternate = internal, external
	default:
		primary, alternate = external, internal
	}
	primary, alternate = h.reachable(primary, joinerIP), h.reachable(alternate, joinerIP)
	if alternate.ip == pcsx2SocketIP || alternate == primary {
		alternate = addr{"0.0.0.0", 0}
	}
	return primary, alternate
}

// reachable swaps a LAN endpoint for the public address and same port when the
// viewer is outside the server's network. Callers hold h.mu.
func (h *theater) reachable(a addr, viewerIP string) addr {
	ip, err := netip.ParseAddr(a.ip)
	viewer, verr := netip.ParseAddr(viewerIP)
	public := h.srv.opts.PublicIP
	if err != nil || verr != nil || !public.IsValid() || lanAddr(public) {
		return a
	}
	if lanAddr(ip) && !lanAddr(viewer) {
		return addr{public.String(), a.port}
	}
	return a
}

// udpKeepAlive ECHOes each observed endpoint so PCSX2 does not unbind its fixed
// port after about 120 s of silence during a level load.
func (h *theater) udpKeepAlive(ctx context.Context, conn *net.UDPConn) error {
	ticker := time.NewTicker(45 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			h.mu.Lock()
			targets := make([]addr, 0, len(h.observed))
			for _, e := range h.observed {
				targets = append(targets, e)
			}
			h.mu.Unlock()
			for _, e := range targets {
				ap, err := netip.ParseAddrPort(net.JoinHostPort(e.ip, strconv.Itoa(e.port)))
				if err != nil {
					continue
				}
				var buf bytes.Buffer
				if fesl.Packet("ECHO", msgOK, field("TID", "0"), field("TYPE", "1"),
					field("ADDR", e.ip), field("PORT", strconv.Itoa(e.port))).Write(&buf) == nil {
					conn.WriteToUDPAddrPort(buf.Bytes(), ap)
				}
			}
		}
	}
}

// handleEcho observes a console's public UDP endpoint and echoes it back so the
// game learns the address peers should use. conn is the theater UDP socket.
func (h *theater) handleEcho(conn *net.UDPConn, from netip.AddrPort, data []byte) {
	frame, err := aries.Read(bytes.NewReader(data))
	if err != nil {
		// Not an Aries frame: log its size and leading bytes so an unknown UDP
		// protocol from the client is visible while bringing hosting online.
		head := data[:min(len(data), 16)]
		h.srv.opts.Log.Info("theater UDP unparsed datagram", "from", from.String(), "bytes", len(data),
			"head", hex.EncodeToString(head))
		return
	}
	if frame.Tag != "ECHO" {
		h.srv.opts.Log.Info("theater UDP datagram", "from", from.String(), "kind", frame.Tag, "bytes", len(data))
		return
	}
	m := fesl.Decode(frame)
	tid, _ := m.GetFold("tid")
	if !validID(tid) {
		tid = "0"
	}
	// TYPE 1 is the game browser's demangler and missing TYPE is channel 0; other
	// values are not requests this server understands.
	msgType, ok := m.GetFold("type")
	if !ok || msgType == "" {
		msgType = "0"
	}
	if msgType != "0" && msgType != "1" {
		return
	}
	srcIP := from.Addr().String()
	srcPort := int(from.Port())

	h.mu.Lock()
	writer := h.attributeEcho(m, srcIP, srcPort)
	if writer != nil {
		h.observed[writer] = addr{srcIP, srcPort}
	}
	h.mu.Unlock()

	reply := fesl.Packet("ECHO", msgOK, field("TID", tid), field("TYPE", msgType), field("ADDR", srcIP), field("PORT", strconv.Itoa(srcPort)))
	var buf bytes.Buffer
	if reply.Write(&buf) == nil {
		conn.WriteToUDPAddrPort(buf.Bytes(), from)
	}
	if writer != nil {
		h.srv.opts.Log.Info("theater UDP endpoint observed", "conn", writer.ID(), "endpoint", srcIP+":"+strconv.Itoa(srcPort))
	}
}

// attributeEcho maps an ECHO to a connection by host secret, else by source IP and
// port. Ambiguous datagrams are dropped (internet-play.md#shared-public-ips).
func (h *theater) attributeEcho(m fesl.Message, srcIP string, srcPort int) *aries.Client {
	gid, hasGID := parseInt(m, "ugid")
	secret, _ := m.GetFold("secret")
	if hasGID || secret != "" {
		g := h.games[gid]
		if g == nil || secret == "" || g.secret != secret || h.connIP[g.owner] != srcIP {
			return nil
		}
		return g.owner
	}
	var candidates, samePort, unseen []*aries.Client
	for conn, ip := range h.connIP {
		if ip != srcIP {
			continue
		}
		candidates = append(candidates, conn)
		if e, ok := h.observed[conn]; ok {
			if e.port == srcPort {
				samePort = append(samePort, conn)
			}
		} else {
			unseen = append(unseen, conn)
		}
	}
	// An unknown port on a shared IP is not given to an unseen connection: it may
	// be a seen console whose NAT moved, and EGAM carries the client's own port.
	switch {
	case len(candidates) == 1:
		return candidates[0]
	case len(samePort) == 1:
		return samePort[0]
	}
	if len(candidates) > 1 {
		h.srv.opts.Log.Info("theater UDP observation ambiguous", "source", srcIP+":"+strconv.Itoa(srcPort),
			"candidates", len(candidates), "same_port", len(samePort), "unseen", len(unseen))
	}
	return nil
}

// maxAccumulated bounds each merged attribute map so GDAT, GDET and PDAT replies
// stay well below aries.MaxFrame.
const maxAccumulated = 16 * 1024

// mergeBounded applies add to dst unless the encoded result would exceed maxAccumulated.
func mergeBounded(dst, add map[string]string) bool {
	merged := maps.Clone(dst)
	maps.Copy(merged, add)
	fields := make([]fesl.Field, 0, len(merged))
	for k, v := range merged {
		fields = append(fields, fesl.Field{Key: "b-" + k, Value: v})
	}
	if len(fesl.Packet("GDAT", msgOK, fields...).Body) > maxAccumulated {
		return false
	}
	maps.Copy(dst, add)
	return true
}

func playerCounts(g *hostedGame) (active, joining int) {
	for _, p := range g.players {
		if p.entered {
			active++
		}
	}
	joining = len(g.players) - active + len(g.pending)
	return active, joining
}

func parseInt(m fesl.Message, key string) (int, bool) {
	v, ok := m.GetFold(key)
	if !ok || v == "" || len(v) > 10 {
		return 0, false
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return 0, false
	}
	return n, true
}

func newSecret() string {
	var b [8]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// loggableFields returns a request's fields for diagnostics, without session
// keys, secrets, tickets or passwords.
func loggableFields(m fesl.Message) map[string]string {
	out := m.Fields()
	for k := range out {
		switch strings.ToLower(k) {
		case "lkey", "secret", "ticket", "password":
			out[k] = "[redacted]"
		}
	}
	return out
}
