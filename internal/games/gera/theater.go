package gera

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dead-game-entrepreneurs/rei-nation/internal/protocol/aries"
	"github.com/dead-game-entrepreneurs/rei-nation/internal/protocol/fesl"
)

// theater is the EA game-browser service. After the messenger brings the
// persona online, the client opens the theater (TCP) to list, host and join
// games, and uses the theater's UDP port to learn its own public endpoint so
// peers can reach it. Like the messenger it is plaintext Aries framing with the
// command in the frame tag; the server also sends a PING heartbeat.
//
// This targets a LAN of separate machines, where consoles reach each other
// directly by address, so there is no relay or NAT-traversal fallback. The
// server exchanges each peer's UDP-observed endpoint. See the online map.
type theater struct {
	srv *server

	mu       sync.Mutex
	sessions map[*aries.Client]*theaterSession
	games    map[int]*hostedGame
	connIP   map[*aries.Client]string // theater TCP peer IP, for ECHO attribution
	observed map[*aries.Client]addr   // latest UDP endpoint seen for a connection
	nextGID  int
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
}

type gamePlayer struct {
	writer  *aries.Client
	name    string
	uid     int64
	ip      string
	port    int
	ticket  string
	entered bool
	attrs   map[string]string
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
	h.mu.Lock()
	for gid, g := range h.games {
		if g.owner == c {
			delete(h.games, gid)
			continue
		}
		// Drop this connection's membership from any game it had joined.
		for pid, p := range g.players {
			if p.writer == c {
				delete(g.players, pid)
			}
		}
		for pid, p := range g.pending {
			if p.writer == c {
				delete(g.pending, pid)
			}
		}
	}
	delete(h.sessions, c)
	delete(h.connIP, c)
	delete(h.observed, c)
	h.mu.Unlock()
	h.srv.opts.Log.Info("theater client disconnected", "conn", c.ID(), "reason", c.CloseReason())
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
	h.srv.opts.Log.Info("theater auth", "conn", c.ID(), "persona", id.personaName, "persona_id", id.personaID)
	c.Send(fesl.Packet("USER", msgOK, field("TID", tid), field("NAME", id.personaName)))
}

// lobbyName is the lobby the client will host and join in. After each lobby
// list the client's online manager (FUN_00338920) picks the first lobby that
// passes one of two checks, depending on its mode flags: NAME equals its region
// string from FUN_00331a98 ("NorthAmerica" on this disc, "Asia" otherwise), or
// FAVORITE-PLAYERS (lobby field +0x34) is non-zero. With no match it records
// error 'nrom', never sends CGAM, and the host times out after the level loads.
// The single lobby satisfies both checks.
const lobbyName = "NorthAmerica"

// lobbies are offered in order; LID is the index plus one. The client takes
// the first one that passes its check, so "Asia" covers a console whose
// language setting makes it pick that region string instead.
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
	var found []*hostedGame
	for _, g := range h.games {
		if g.lid == lid {
			found = append(found, g)
		}
	}
	n := strconv.Itoa(len(found))
	c.Send(fesl.Packet("GLST", msgOK, field("TID", tid), field("LID", lid),
		field("NUM-GAMES", n), field("LOBBY-MAX-GAMES", "100"), field("LOBBY-NUM-GAMES", n)))
	for _, g := range h.games {
		if g.lid == lid {
			c.Send(fesl.Packet("GDAT", msgOK, h.gameDataFields(g, tid, c.Addr().String())...))
		}
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
	c.Send(fesl.Packet("GDAT", msgOK, h.gameDataFields(g, tid, c.Addr().String())...))
	details := []fesl.Field{field("TID", tid), field("LID", g.lid), field("GID", strconv.Itoa(gid)), field("UGID", strconv.Itoa(gid))}
	for k, v := range g.details {
		details = append(details, field(k, v))
	}
	c.Send(fesl.Packet("GDET", msgOK, details...))
	for pid, p := range g.players {
		if !p.entered {
			continue
		}
		pf := []fesl.Field{field("TID", tid), field("LID", g.lid), field("GID", strconv.Itoa(gid)),
			field("PID", strconv.Itoa(pid)), field("NAME", p.name), field("UID", strconv.FormatInt(p.uid, 10))}
		for k, v := range p.attrs {
			pf = append(pf, field(k, v))
		}
		c.Send(fesl.Packet("PDAT", msgOK, pf...))
	}
}

// gameDataFields builds a GDAT body for a game as seen by a viewer. Callers hold h.mu.
func (h *theater) gameDataFields(g *hostedGame, tid, viewerIP string) []fesl.Field {
	ext := h.externalOf(g.owner, g.hostIP, g.port)
	active, joining := playerCounts(g)
	out := []fesl.Field{
		field("TID", tid), field("LID", g.lid), field("GID", strconv.Itoa(g.gid)),
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
	for gid, g := range h.games { // one current match per host
		if g.owner == c {
			delete(h.games, gid)
		}
	}
	h.nextGID++
	gid := h.nextGID
	h.games[gid] = &hostedGame{
		gid: gid, owner: c, host: t.id, lid: lid, name: name, max: maxP, port: port, hostIP: hostIP,
		secret: newSecret(), internalIP: intIP, internalPort: intPort, join: "O",
		attrs: m.Prefixed("b-"), details: map[string]string{},
		players: map[int]*gamePlayer{}, pending: map[int]*gamePlayer{}, nextPID: 2,
	}
	secret := h.games[gid].secret
	h.mu.Unlock()

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
	if g == nil || g.lid != lid {
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
	pid := 1
	if !self {
		pid = g.nextPID
		g.nextPID++
	}
	joinerIP := c.Addr().String()
	ticket := newSecret()
	g.pending[pid] = &gamePlayer{writer: c, name: t.id.personaName, uid: t.id.personaID, ip: joinerIP, port: port, ticket: ticket, attrs: map[string]string{}}
	ext := h.externalOf(c, joinerIP, port)
	owner := g.owner
	h.mu.Unlock()

	egrq := fesl.Packet("EGRQ", msgOK, field("TID", "0"), field("NAME", t.id.personaName),
		field("UID", strconv.FormatInt(t.id.personaID, 10)), field("PID", strconv.Itoa(pid)),
		field("TICKET", ticket), field("IP", ext.ip), field("PORT", strconv.Itoa(ext.port)))
	c.Send(fesl.Packet("EGAM", msgOK, field("TID", tid), field("REASON", "0"), field("QPOS", "0"), field("QLEN", "0")))
	h.srv.opts.Log.Info("theater join request", "conn", c.ID(), "gid", gid, "pid", pid, "persona", t.id.personaName)
	owner.Send(egrq) // for a self-join, owner == c
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
		h.mu.Unlock()
		return
	}
	pending := g.pending[pid]
	delete(g.pending, pid)
	ticket := pending.ticket
	if !approved {
		ticket = ""
	}
	primary, alternate := h.hostAddresses(g, pending.ip, pending.writer == g.owner)
	if approved {
		pending.entered = false
		g.players[pid] = pending
	}
	self := pending.writer == g.owner
	joiner := pending.writer
	gid := g.gid
	lid := g.lid
	h.mu.Unlock()

	if tid != "0" {
		c.Send(fesl.Packet("EGRS", msgOK, field("TID", tid)))
	}
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
	h.mu.Lock()
	if g := h.games[gid]; g != nil {
		for pid, p := range g.players {
			if p.writer == c {
				delete(g.players, pid)
			}
		}
		for pid, p := range g.pending {
			if p.writer == c {
				delete(g.pending, pid)
			}
		}
	}
	h.mu.Unlock()
	if tid != "0" {
		c.Send(fesl.Packet("ECNL", msgOK, field("TID", tid)))
	}
}

func (h *theater) playerLeft(c *aries.Client, m fesl.Message, tid string) {
	pid, ok := parseInt(m, "pid")
	if !ok {
		return
	}
	h.mu.Lock()
	for _, g := range h.games {
		if g.owner == c {
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
			for k, v := range m.Prefixed("d-") {
				g.details[k] = v
			}
		case "UGAM":
			for k, v := range m.Prefixed("b-") {
				g.attrs[k] = v
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
				if p := g.players[pid]; p != nil {
					for k, v := range m.Prefixed("p-") {
						p.attrs[k] = v
					}
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
	h.mu.Lock()
	for gid, g := range h.games {
		if g.owner == c {
			delete(h.games, gid)
		}
	}
	h.mu.Unlock()
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

// hostAddresses returns the primary and alternate host endpoints for a joiner.
// With separate machines the joiner uses the host's observed public endpoint;
// the PCSX2 virtual internal address is never sent to another console, because
// every console shares it and a second emulator would admit itself. Only the
// host's own self-join may receive the internal address. Callers hold h.mu.
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
	if alternate.ip == pcsx2SocketIP || alternate == primary {
		alternate = addr{"0.0.0.0", 0}
	}
	return primary, alternate
}

// udpKeepAlive keeps each console's theater UDP socket alive. PCSX2 unbinds a
// "fixed port" after about 120 seconds with no traffic, which otherwise drops
// the game's session during a level load. A periodic ECHO to the observed
// endpoint resets that idle timer without the game treating it as new state.
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
	msgType, _ := m.GetFold("type")
	if msgType != "0" && msgType != "1" {
		msgType = "0"
	}
	srcIP := from.Addr().String()
	srcPort := int(from.Port())

	h.mu.Lock()
	writer := h.attributeEcho(m, srcIP)
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

// attributeEcho maps an ECHO datagram to a theater connection: by host secret
// when present, otherwise to the single connection from the same IP. Callers
// hold h.mu.
func (h *theater) attributeEcho(m fesl.Message, srcIP string) *aries.Client {
	gid, hasGID := parseInt(m, "ugid")
	secret, _ := m.GetFold("secret")
	if hasGID || secret != "" {
		g := h.games[gid]
		if g == nil || secret == "" || g.secret != secret || h.connIP[g.owner] != srcIP {
			return nil
		}
		return g.owner
	}
	var match *aries.Client
	for conn, ip := range h.connIP {
		if ip == srcIP {
			if match != nil {
				return nil // ambiguous; needs the host-binding path
			}
			match = conn
		}
	}
	return match
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
