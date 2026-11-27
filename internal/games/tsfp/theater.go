package tsfp

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/binary"
	"net"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dead-game-entrepreneurs/rei-nation/internal/protocol/aries"
	"github.com/dead-game-entrepreneurs/rei-nation/internal/storage"
)

type theater struct {
	mu          sync.Mutex
	keys        *loginKeys
	sessions    map[*aries.Client]*theaterSession
	games       map[int]*match
	hosting     map[*aries.Client]*match
	nextID      int
	relayIP     netip.Addr
	udp         *net.UDPConn
	relayMode   string
	failedUntil map[int64]time.Time
	probeSlots  chan struct{}
}

type theaterSession struct {
	persona  storage.Persona
	endpoint netip.AddrPort
	seen     time.Time
	probe    string
	probed   time.Time
}

type match struct {
	id, nextPID        int
	owner              *aries.Client
	host               storage.Persona
	lid, name          string
	join, secret       string
	capacity           int
	external, internal netip.AddrPort
	attrs, details     map[string]string
	players            map[int]*entrant
	activity           time.Time
	relay              *relayRoom
	relayReachable     bool
	relayProbeAddr     netip.AddrPort
	relayProbedAt      time.Time
}

type entrant struct {
	client    *aries.Client
	persona   storage.Persona
	ticket    string
	requested time.Time
	approved  time.Time
	active    bool
	attrs     map[string]string
	route     relayRoute
	endpoint  netip.AddrPort
}

func newTheater(keys *loginKeys, relayIP netip.Addr, mode string) *theater {
	return &theater{keys: keys, relayIP: relayIP, relayMode: mode,
		sessions: make(map[*aries.Client]*theaterSession), games: make(map[int]*match),
		hosting: make(map[*aries.Client]*match), failedUntil: make(map[int64]time.Time), probeSlots: make(chan struct{}, 16)}
}

var noEndpoint = netip.AddrPortFrom(netip.IPv4Unspecified(), 0)
var pcsx2SocketIP = netip.MustParseAddr("192.0.2.100")

func (t *theater) endpoint(c *aries.Client, declared netip.AddrPort) netip.AddrPort {
	if session := t.sessions[c]; session != nil && session.endpoint.IsValid() {
		return session.endpoint
	}
	return declared
}

func (t *theater) addresses(g *match, viewer *aries.Client) (netip.AddrPort, netip.AddrPort) {
	public := t.endpoint(g.owner, g.external)
	local := g.internal
	if local.Addr() == pcsx2SocketIP && viewer != g.owner {
		return public, noEndpoint
	}
	if public == local {
		return public, noEndpoint
	}
	if viewer.Addr() == g.owner.Addr() {
		return local, public
	}
	return public, local
}

func (t *theater) Handle(_ context.Context, c *aries.Client, in aries.Frame) {
	f := fields(in)
	tid := fallback(f, "TID", "0")
	if !transactionID(tid) {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.sessions[c]
	if s == nil {
		s = &theaterSession{}
		t.sessions[c] = s
	}
	hosted := t.hosting[c]
	if in.Tag == "CONN" {
		c.Send(packet("CONN", "TID", tid, "PROT", "2", "TIME", "0"))
		return
	}
	if in.Tag == "USER" {
		p, ok := t.keys.lookup(f["LKEY"])
		if !ok || (s.persona.ID != 0 && s.persona.ID != p.ID) {
			c.Send(packet("USERauth", "TID", tid))
			return
		}
		s.persona = p
		c.Send(packet("USER", "TID", tid, "NAME", p.Name))
		return
	}
	if s.persona.ID == 0 {
		return
	}
	id, _ := decimal(f["GID"], 2147483647)
	g := t.games[id]
	if g != nil && g.lid != f["LID"] {
		g = nil
	}
	pid, _ := decimal(f["PID"], 2147483647)
	switch in.Tag {
	case "LLST":
		c.Send(packet("LLST", "TID", tid, "NUM-LOBBIES", "1"))
		c.Send(packet("LDAT", "TID", tid, "LID", "1", "NAME", "Local", "PASSING", "1", "FAVORITE-GAMES", "0",
			"FAVORITE-PLAYERS", "0", "LOCALE", "en_US", "MAX-GAMES", "100", "NUM-GAMES", strconv.Itoa(len(t.games))))
	case "CGAM":
		t.create(c, s.persona, tid, f, hosted)
	case "GLST":
		t.list(c, tid, fallback(f, "LID", "1"))
	case "GDAT":
		if g != nil {
			t.details(c, tid, g)
		}
	case "EGAM":
		t.join(c, s.persona, tid, f, g)
	case "EGRS":
		t.approve(c, tid, f, hosted, pid)
	case "ECNL":
		if g != nil {
			t.removeGuest(g, c, true)
			ack(c, in.Tag, tid)
		}
	case "RGAM":
		if hosted != nil {
			t.closeMatch(hosted)
		}
		ack(c, in.Tag, tid)
	case "PLVT":
		if hosted != nil {
			t.removeEntrant(hosted, pid, false, false)
			ack(c, in.Tag, tid)
		}
	case "PENT":
		if hosted != nil && hosted.players[pid] != nil && !hosted.players[pid].approved.IsZero() {
			hosted.players[pid].active = true
		}
		if pid != 0 && tid != "0" {
			c.Send(packet("PENT", "TID", tid, "PID", strconv.Itoa(pid)))
		}
	case "UGAM", "UGDE", "UPLA", "KEEP", "PING", "UBRA":
		updateMatch(hosted, in.Tag, pid, f)
		if in.Tag != "PING" && in.Tag != "UBRA" {
			ack(c, in.Tag, tid)
		}
	}
}

func ack(c *aries.Client, tag, tid string) {
	if tid != "0" {
		c.Send(packet(tag, "TID", tid))
	}
}

func (t *theater) create(c *aries.Client, persona storage.Persona, tid string, f map[string]string, prior *match) {
	lid := fallback(f, "LID", "1")
	capacity, capOK := decimal(fallback(f, "MAX-PLAYERS", "8"), 16)
	port, portOK := decimal(fallback(f, "PORT", "3000"), 65535)
	if lid != "1" || !capOK || capacity == 0 || !portOK || port == 0 || (len(t.games) >= 100 && prior == nil) {
		return
	}
	if prior != nil {
		t.closeMatch(prior)
	}
	external := netip.AddrPortFrom(c.Addr(), uint16(port))
	local := external
	if ip, err := netip.ParseAddr(f["INT-IP"]); err == nil && ip.Is4() && !ip.IsUnspecified() && !ip.IsMulticast() {
		if n, ok := decimal(fallback(f, "INT-PORT", strconv.Itoa(port)), 65535); ok && n > 0 {
			local = netip.AddrPortFrom(ip, uint16(n))
		}
	}
	t.nextID++
	g := &match{id: t.nextID, nextPID: 2, owner: c, host: persona, lid: lid, name: short(fallback(f, "NAME", "Game"), 128),
		capacity: capacity, external: external, internal: local, join: "O", secret: token(8),
		attrs: make(map[string]string), details: make(map[string]string), players: make(map[int]*entrant), activity: time.Now()}
	copyAttributes(g.attrs, f, "B-")
	t.games[g.id], t.hosting[c] = g, g
	t.probeRelay(g)
	id := strconv.Itoa(g.id)
	c.Send(packet("CGAM", "TID", tid, "LID", lid, "GID", id, "UGID", id,
		"SECRET", g.secret, "MAX-PLAYERS", strconv.Itoa(capacity)))
}

func (t *theater) list(c *aries.Client, tid, lid string) {
	if lid != "1" {
		return
	}
	var ids []int
	for id := range t.games {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	count := strconv.Itoa(len(ids))
	c.Send(packet("GLST", "TID", tid, "LID", lid, "NUM-GAMES", count, "LOBBY-MAX-GAMES", "100", "LOBBY-NUM-GAMES", count))
	for _, id := range ids {
		c.Send(t.gameData(c, tid, t.games[id]))
	}
}

func (t *theater) gameData(c *aries.Client, tid string, g *match) aries.Frame {
	endpoint, _ := t.addresses(g, c)
	active := 0
	for _, p := range g.players {
		if p.active {
			active++
		}
	}
	pairs := []string{"TID", tid, "LID", g.lid, "GID", strconv.Itoa(g.id), "N", g.name, "MP", strconv.Itoa(g.capacity),
		"I", endpoint.Addr().String(), "P", strconv.Itoa(int(endpoint.Port())), "PW", "0", "AP", strconv.Itoa(active),
		"JP", strconv.Itoa(len(g.players) - active), "QP", "0", "HN", g.host.Name, "HU", strconv.FormatInt(g.host.ID, 10),
		"V", "1.0", "F", "0", "NF", "0", "PL", "PS2", "J", g.join}
	return packet("GDAT", appendFields(pairs, g.attrs)...)
}

func (t *theater) details(c *aries.Client, tid string, g *match) {
	c.Send(t.gameData(c, tid, g))
	id := strconv.Itoa(g.id)
	c.Send(packet("GDET", appendFields([]string{"TID", tid, "LID", g.lid, "GID", id, "UGID", id}, g.details)...))
	var players []int
	for pid, p := range g.players {
		if p.active {
			players = append(players, pid)
		}
	}
	sort.Ints(players)
	for _, pid := range players {
		p := g.players[pid]
		c.Send(packet("PDAT", appendFields([]string{"TID", tid, "LID", g.lid, "GID", id, "PID", strconv.Itoa(pid),
			"NAME", p.persona.Name, "UID", strconv.FormatInt(p.persona.ID, 10)}, p.attrs)...))
	}
}

func (t *theater) join(c *aries.Client, persona storage.Persona, tid string, f map[string]string, g *match) {
	port, ok := decimal(fallback(f, "PORT", "3000"), 65535)
	if !ok || port == 0 {
		return
	}
	if g == nil {
		c.Send(packet("EGAMngam", "TID", tid))
		return
	}
	pid := 1
	if g.owner != c {
		for _, p := range g.players {
			if p.persona.ID == persona.ID {
				c.Send(packet("EGAMjden", "TID", tid, "REASON", "f"))
				return
			}
		}
		reason := ""
		switch {
		case len(g.players)-boolInt(g.players[1] != nil)+1 >= g.playerLimit():
			reason = "f"
		case g.join == "C":
			reason = "c"
		}
		if reason != "" {
			c.Send(packet("EGAMjden", "TID", tid, "REASON", reason))
			return
		}
		pid, g.nextPID = g.nextPID, g.nextPID+1
	}
	if g.players[pid] != nil {
		c.Send(packet("EGAM", "TID", tid, "REASON", "0", "QPOS", "0", "QLEN", "0"))
		return
	}
	endpoint := t.endpoint(c, netip.AddrPortFrom(c.Addr(), uint16(port)))
	p := &entrant{client: c, persona: persona, ticket: token(8), requested: time.Now(), attrs: make(map[string]string)}
	p.endpoint = endpoint
	if pid != 1 {
		t.probeRelay(g)
		endpoint, p.route = t.joinRelay(g, pid, persona.ID, endpoint)
	}
	g.players[pid] = p
	c.Send(packet("EGAM", "TID", tid, "REASON", "0", "QPOS", "0", "QLEN", "0"))
	g.owner.Send(packet("EGRQ", "TID", "0", "NAME", persona.Name, "UID", strconv.FormatInt(persona.ID, 10),
		"PID", strconv.Itoa(pid), "TICKET", p.ticket, "IP", endpoint.Addr().String(), "PORT", strconv.Itoa(int(endpoint.Port()))))
}

func (t *theater) approve(c *aries.Client, tid string, values map[string]string, g *match, pid int) {
	if g == nil {
		return
	}
	p := g.players[pid]
	if p == nil || !p.approved.IsZero() {
		return
	}
	allowed := strings.ToLower(values["ALLOWED"])
	ticket := ""
	primary, alternate := t.addresses(g, p.client)
	if allowed == "1" || allowed == "t" || allowed == "true" {
		ticket, p.approved = p.ticket, time.Now()
		switch p.route {
		case forcedRelay:
			primary, alternate = g.relay.address(1), noEndpoint
		case fallbackRelay:
			alternate = g.relay.address(1)
		}
	} else {
		t.removeEntrant(g, pid, false, false)
	}
	ack(c, "EGRS", tid)
	id := strconv.Itoa(g.id)
	p.client.Send(packet("EGEG", "TID", "0", "LID", g.lid, "GID", id, "PID", strconv.Itoa(pid),
		"TICKET", ticket, "REASON", fallback(values, "REASON", "0"), "I", primary.Addr().String(),
		"P", strconv.Itoa(int(primary.Port())), "PL", "PS2", "UGID", id,
		"INT-IP", alternate.Addr().String(), "INT-PORT", strconv.Itoa(int(alternate.Port()))))
}

func (t *theater) Disconnected(c *aries.Client) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.sessions, c)
	if hosted := t.hosting[c]; hosted != nil {
		t.closeMatch(hosted)
	}
	for _, g := range t.games {
		t.removeGuest(g, c, true)
	}
}

func (t *theater) removeGuest(g *match, c *aries.Client, cancelled bool) {
	for pid, p := range g.players {
		if p.client == c && pid != 1 {
			t.removeEntrant(g, pid, true, cancelled)
		}
	}
}

func (t *theater) removeEntrant(g *match, pid int, notify, failed bool) {
	p := g.players[pid]
	if p == nil {
		return
	}
	if pid != 1 && !p.approved.IsZero() {
		if notify {
			g.owner.Send(packet("KICK", "PID", strconv.Itoa(pid)))
		}
		if failed && !p.active && p.route != forcedRelay && t.relayIP.IsValid() && time.Since(p.approved) <= 30*time.Second {
			t.failedUntil[p.persona.ID] = time.Now().Add(24 * time.Hour)
		}
	}
	delete(g.players, pid)
	if g.relay != nil && pid != 1 {
		g.relay.remove(pid)
	}
}

func (t *theater) closeMatch(g *match) {
	notice := packet("GREM", "LID", g.lid, "GID", strconv.Itoa(g.id))
	for pid, p := range g.players {
		if pid != 1 {
			p.client.Send(notice)
		}
	}
	if g.relay != nil {
		g.relay.close()
	}
	delete(t.games, g.id)
	delete(t.hosting, g.owner)
}

func updateMatch(g *match, tag string, pid int, values map[string]string) {
	if g == nil {
		return
	}
	g.activity = time.Now()
	switch tag {
	case "UGAM":
		copyAttributes(g.attrs, values, "B-")
		if join := strings.ToUpper(values["JOIN"]); join == "O" || join == "C" || join == "W" {
			g.join = join
		}
		if name := values["NAME"]; name != "" {
			g.name = short(name, 128)
		}
	case "UGDE":
		copyAttributes(g.details, values, "D-")
	case "UPLA":
		if p := g.players[pid]; p != nil {
			copyAttributes(p.attrs, values, "P-")
		}
	}
}

func copyAttributes(dest, src map[string]string, prefix string) {
	for key, value := range src {
		if strings.HasPrefix(key, prefix) && len(key) <= 32 && len(value) <= 128 &&
			!strings.ContainsFunc(key, func(c rune) bool { return c <= ' ' || c >= 127 || c == '=' }) {
			if _, exists := dest[key]; exists || len(dest) < 64 {
				dest[key] = value
			}
		}
	}
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (g *match) playerLimit() int {
	count := 0
	for _, key := range []string{"B-FILLEDSLOTS", "B-PUBLICSLOTS", "B-FRIENDSLOTS"} {
		n, ok := decimal(g.attrs[key], 16)
		if !ok {
			return g.capacity
		}
		count += n
	}
	return min(max(count, 1), g.capacity)
}

func short(s string, limit int) string {
	return strings.ToValidUTF8(s[:min(len(s), limit)], "")
}

func (t *theater) echo(from netip.AddrPort, data []byte) {
	if len(data) < 12 || len(data) > 512 || binary.BigEndian.Uint32(data[8:12]) != uint32(len(data)) {
		return
	}
	buffer := bytes.NewBuffer(data)
	frame, err := aries.Read(buffer)
	if err != nil || buffer.Len() != 0 {
		return
	}
	f := fields(frame)
	tid, kind := fallback(f, "TID", "0"), fallback(f, "TYPE", "0")
	if !transactionID(tid) || (kind != "0" && kind != "1") {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if frame.Tag == "PROB" && frame.Code == [4]byte{0xc0} && kind == "1" {
		for c, s := range t.sessions {
			if s.probe == tid && c.Addr() == from.Addr() && time.Since(s.probed) < 15*time.Second {
				s.probe, s.endpoint, s.seen = "", from, time.Now()
				break
			}
		}
		return
	}
	if frame.Tag != "ECHO" || frame.Code != [4]byte{} {
		return
	}
	s, knownIP := t.observe(from, f)
	if !knownIP {
		return
	}
	if s != nil {
		s.endpoint, s.seen = from, time.Now()
		if id, ok := decimal(f["UGID"], maxRecordID); ok && t.games[id] != nil {
			t.probeRelay(t.games[id])
		}
	}
	sendUDP(t.udp, from, packet("ECHO", "TID", tid, "TYPE", kind, "ADDR", from.Addr().String(),
		"PORT", strconv.Itoa(int(from.Port()))))
}

func (t *theater) observe(from netip.AddrPort, f map[string]string) (*theaterSession, bool) {
	if f["UGID"] != "" || f["SECRET"] != "" {
		id, _ := decimal(f["UGID"], 2147483647)
		g := t.games[id]
		if g != nil && from.Addr() == g.owner.Addr() &&
			subtle.ConstantTimeCompare([]byte(f["SECRET"]), []byte(g.secret)) == 1 {
			return t.sessions[g.owner], true
		}
		return nil, false
	}
	var last, known, unseen *theaterSession
	var count, knownCount, unseenCount int
	for c, s := range t.sessions {
		if c.Addr() != from.Addr() || s.persona.ID == 0 {
			continue
		}
		count++
		last = s
		if time.Since(s.seen) > 5*time.Minute {
			unseen, unseenCount = s, unseenCount+1
		} else if s.endpoint == from {
			known, knownCount = s, knownCount+1
		}
	}
	switch {
	case count == 1:
		return last, true
	case knownCount == 1:
		return known, true
	case knownCount == 0 && unseenCount == 1:
		return unseen, true
	}
	// Even an ambiguous request tells the client which port to put in EGAM.
	return nil, count != 0
}

func sendUDP(socket *net.UDPConn, to netip.AddrPort, f aries.Frame) {
	var buf bytes.Buffer
	if socket != nil && f.Write(&buf) == nil {
		_, _ = socket.WriteToUDPAddrPort(buf.Bytes(), to)
	}
}

func (t *theater) sweep(ctx context.Context, socket *net.UDPConn) error {
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			t.mu.Lock()
			for _, g := range t.games {
				t.closeMatch(g)
			}
			t.mu.Unlock()
			return nil
		case now := <-tick.C:
			t.mu.Lock()
			t.expire(now, socket)
			t.mu.Unlock()
		}
	}
}

func (t *theater) expire(now time.Time, socket *net.UDPConn) {
	for _, g := range t.games {
		if now.Sub(g.activity) >= time.Hour {
			t.closeMatch(g)
			continue
		}
		for pid, p := range g.players {
			if p.approved.IsZero() && now.Sub(p.requested) >= 45*time.Second {
				t.removeEntrant(g, pid, false, false)
			}
		}
	}
	for id, until := range t.failedUntil {
		if !now.Before(until) {
			delete(t.failedUntil, id)
		}
	}
	for _, s := range t.sessions {
		if !s.endpoint.IsValid() || s.persona.ID == 0 || now.Sub(s.probed) < 30*time.Second {
			continue
		}
		n, _ := strconv.ParseUint(token(4), 16, 32)
		s.probe, s.probed = strconv.FormatUint(n%0x7ffffffe+1, 10), now
		sendUDP(socket, s.endpoint, packet("PROB", "TID", s.probe, "TYPE", "1"))
	}
}
