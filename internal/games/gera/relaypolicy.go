package gera

import (
	"context"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dead-game-entrepreneurs/rei-nation/internal/games/gera/relay"
	"github.com/dead-game-entrepreneurs/rei-nation/internal/protocol/aries"
)

// The relay fallback follows the TimeSplitters theater (theater.py and
// relay_hub.py's RelayPolicy). Each join is one of:
//
//   - direct: both consoles get each other's observed endpoints, as before.
//   - fallback: direct endpoints, with the host's relay port as the alternate
//     (EGEG INT-IP). The joiner tries the alternate by itself only when its
//     direct probes fail, so a working direct path is unaffected. This needs a
//     host whose router accepts packets from the relay without having sent to
//     it, which the relay's PROB after CGAM tests.
//   - relay: both consoles get relay addresses. Used for forced personas and
//     for a joiner whose direct join failed recently (approved, then left
//     without entering).
//
// One room per match holds the host and every player, direct ones too: once
// the host announces a relayed player's address the other guests send to it.
const (
	relayControlPort = 18043
	// failedJoinWindow: an approved guest that leaves without PENT within this
	// long failed to reach the host (the joiner gives up about 4 s after
	// approval, 8 s with an alternate).
	failedJoinWindow = 30 * time.Second
	relayMemory      = 24 * time.Hour
	maxRemembered    = 4096
	// relayProbeDelay lets the host's ECHO refresh after CGAM, and a new
	// player's login ECHO, arrive before the relay probes the mapping.
	relayProbeDelay   = 2 * time.Second
	relayProbeRefresh = 10 * time.Minute
	// relayFallbackWait is how long a fallback approval waits for its room.
	relayFallbackWait = 1500 * time.Millisecond
	relayOpTimeout    = 10 * time.Second
	// hostKey is the host's member key in a room; the host is PID 1.
	hostKey = "1"
)

type relayPolicy struct {
	forced map[string]bool // persona names or IDs, lowercased; "*" relays every join

	mu     sync.Mutex
	failed map[int64]time.Time
}

func newRelayPolicy(forced []string) *relayPolicy {
	p := &relayPolicy{forced: map[string]bool{}, failed: map[int64]time.Time{}}
	for _, f := range forced {
		if f = strings.ToLower(strings.TrimSpace(f)); f != "" {
			p.forced[f] = true
		}
	}
	return p
}

func (p *relayPolicy) isForced(id identity) bool {
	return p.forced[strings.ToLower(id.personaName)] || p.forced[strconv.FormatInt(id.personaID, 10)]
}

func (p *relayPolicy) remembered(persona int64) (time.Duration, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	at, ok := p.failed[persona]
	if !ok || time.Since(at) >= relayMemory {
		return 0, false
	}
	return time.Since(at), true
}

// reason says why a join is relayed, or "" when it is not.
func (p *relayPolicy) reason(joiner, host identity) string {
	switch {
	case p.forced["*"]:
		return "forced_all"
	case p.isForced(joiner):
		return "forced_joiner"
	case p.isForced(host):
		return "forced_host"
	}
	if ago, ok := p.remembered(joiner.personaID); ok {
		return "failed_join_" + strconv.Itoa(int(ago.Seconds())) + "s_ago"
	}
	return ""
}

func (p *relayPolicy) recordFailure(persona int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.failed) >= maxRemembered {
		var oldest int64
		var at time.Time
		for id, t := range p.failed {
			if at.IsZero() || t.Before(at) {
				oldest, at = id, t
			}
		}
		delete(p.failed, oldest)
	}
	p.failed[persona] = time.Now()
}

// relayDecision picks direct (""), "fallback" or "relay" for a join, with the
// reason for the log. Callers hold h.mu.
func (h *theater) relayDecision(g *hostedGame, joiner identity, joinerIP string) (string, string) {
	hub := h.srv.relays
	if hub == nil {
		return "", "relay_off"
	}
	if !hub.Available() {
		return "", "no_relay_connected"
	}
	if joinerIP == g.hostIP {
		// Behind one router the direct path works, except between PCSX2s in
		// Sockets mode: they share a virtual address and need the router to
		// hairpin. After a failed join both sending out to the relay needs no
		// hairpin.
		if _, failed := h.srv.relayPolicy.remembered(joiner.personaID); failed && g.internalIP == pcsx2SocketIP {
			return "relay", "same_ip_emulator_after_failed_join"
		}
		return "", "same_public_ip"
	}
	if reason := h.srv.relayPolicy.reason(joiner, g.host); reason != "" {
		return "relay", reason
	}
	switch {
	case g.relayReachable == nil:
		return "", "host_not_probed"
	case len(g.relayReachable) == 0:
		return "", "host_filters_relays"
	}
	names := make([]string, 0, len(g.relayReachable))
	for n := range g.relayReachable {
		names = append(names, n)
	}
	sort.Strings(names)
	return "fallback", "host_accepts=" + strings.Join(names, ",")
}

// publicEndpoint is the address a relay on the Internet reaches a console at.
func (h *theater) publicEndpoint(a addr) (netip.AddrPort, bool) {
	a = h.reachable(a, "203.0.113.1") // any public viewer
	ip, err := netip.ParseAddr(a.ip)
	if err != nil || a.port <= 0 || a.port > 65535 {
		return netip.AddrPort{}, false
	}
	return netip.AddrPortFrom(ip.Unmap(), uint16(a.port)), true
}

func matchLabel(g *hostedGame) string { return "gid=" + strconv.Itoa(g.gid) }

// matchRoom returns the match's relay room holding the joining players,
// opening it with the host and every current player when there is none.
func (h *theater) matchRoom(g *hostedGame, joining map[string]netip.AddrPort, allowed map[string]bool) *relay.Room {
	hub := h.srv.relays
	g.relayMu.Lock()
	defer g.relayMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), relayOpTimeout)
	defer cancel()
	h.mu.Lock()
	room := g.relayRoom
	if room != nil && hub.Closed(room) {
		// The relay expired or lost it; a new room gives everyone new ports.
		h.srv.opts.Log.Warn("relay room gone; opening a new one", "gid", g.gid, "room", room.ID)
		g.relayRoom, room = nil, nil
	}
	members := map[string]netip.AddrPort{}
	if room == nil {
		if ep, ok := h.publicEndpoint(h.externalOf(g.owner, g.hostIP, g.port)); ok {
			members[hostKey] = ep
		}
		for pid, p := range g.players {
			if ep, ok := h.publicEndpoint(h.externalOf(p.writer, p.ip, p.port)); ok {
				members[strconv.Itoa(pid)] = ep
			}
		}
	}
	h.mu.Unlock()
	for k, ep := range joining {
		members[k] = ep
	}
	if room == nil {
		room = hub.OpenRoom(ctx, members, allowed, matchLabel(g))
		if room != nil {
			h.mu.Lock()
			if h.games[g.gid] == g {
				g.relayRoom = room
			} else {
				hub.CloseRoom(room) // the match ended meanwhile
				room = nil
			}
			h.mu.Unlock()
		}
		return room
	}
	missing := map[string]netip.AddrPort{}
	for k, ep := range joining {
		if _, ok := hub.Port(room, k); !ok {
			missing[k] = ep
		}
	}
	if len(missing) > 0 && !hub.AddMembers(ctx, room, missing) {
		return nil
	}
	return room
}

// roomMember is matchRoom for one joiner, undone if the joiner left meanwhile.
func (h *theater) roomMember(g *hostedGame, pid int, ep netip.AddrPort, allowed map[string]bool) *relay.Room {
	room := h.matchRoom(g, map[string]netip.AddrPort{strconv.Itoa(pid): ep}, allowed)
	if room != nil {
		h.mu.Lock()
		_, pending := g.pending[pid]
		_, playing := g.players[pid]
		h.mu.Unlock()
		if !pending && !playing {
			h.srv.relays.RemoveMembers(room, strconv.Itoa(pid))
		}
	}
	return room
}

// releaseRelay takes a departed player out of the match's room. Callers hold h.mu.
func (h *theater) releaseRelay(g *hostedGame, pid int) {
	if g.relayRoom != nil {
		room := g.relayRoom
		go h.srv.relays.RemoveMembers(room, strconv.Itoa(pid))
	}
}

// noteFailedJoin records an approved guest that left without entering: it
// could not reach the host, so its next joins are relayed. Callers hold h.mu.
func (h *theater) noteFailedJoin(g *hostedGame, pid int, p *gamePlayer) {
	if h.srv.relays == nil || p.writer == g.owner || p.entered || p.approvedAt.IsZero() ||
		time.Since(p.approvedAt) > failedJoinWindow {
		return
	}
	h.srv.opts.Log.Info("theater join failed before entry", "gid", g.gid, "pid", pid, "persona", p.name,
		"mode", modeName(p.relayMode))
	if p.relayMode != "relay" && p.uid != 0 {
		h.srv.relayPolicy.recordFailure(p.uid)
		h.srv.opts.Log.Info("relay memory: joins relayed", "persona_id", p.uid, "hours", int(relayMemory.Hours()))
	}
}

func modeName(m string) string {
	if m == "" {
		return "direct"
	}
	return m
}

// probeHost asks every relay whether the new host answers a PROB it never
// asked for.
func (h *theater) probeHost(g *hostedGame) {
	time.Sleep(relayProbeDelay)
	hub := h.srv.relays
	if !hub.Available() {
		return
	}
	h.mu.Lock()
	target, ok := h.publicEndpoint(h.externalOf(g.owner, g.hostIP, g.port))
	g.relayProbedAt = time.Now()
	h.mu.Unlock()
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), relayOpTimeout)
	defer cancel()
	results := hub.ProbeAll(ctx, target)
	if len(results) == 0 {
		return
	}
	reachable := map[string]bool{}
	for name, ok := range results {
		if ok {
			reachable[name] = true
		}
	}
	h.mu.Lock()
	// Per relay: a router filtering by address can accept one and not another.
	g.relayReachable = reachable
	h.mu.Unlock()
	h.srv.opts.Log.Info("relay probe of host", "gid", g.gid, "target", target.String(), "reachable", results)
}

// measurePlayer times every relay to a newly signed-in player, for choosing
// a relay later. The login ECHO that gives its public UDP port follows USER.
func (h *theater) measurePlayer(c *aries.Client) {
	time.Sleep(relayProbeDelay)
	hub := h.srv.relays
	if !hub.Available() {
		return
	}
	h.mu.Lock()
	target, ok := h.publicEndpoint(h.externalOf(c, c.Addr().String(), feslPort))
	h.mu.Unlock()
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), relayOpTimeout)
	defer cancel()
	hub.Measure(ctx, target)
}
