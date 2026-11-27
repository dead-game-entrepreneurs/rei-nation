package relay

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/netip"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	openTimeout = 3 * time.Second
	// The node waits up to 2 s for the console's answer.
	probeTimeout = 4 * time.Second
	// Round trips are re-measured after latencyRefresh and dropped after latencyMaxAge;
	// an unmeasured player counts as unmeasuredMS on every relay.
	latencyRefresh = 10 * time.Minute
	latencyMaxAge  = 3 * time.Hour
	unmeasuredMS   = 250
	maxLatency     = 20000
	pingEvery      = 30 * time.Second
	// Failed hellos from one IP before it is refused for the rest of the window.
	authFailureLimit  = 5
	authFailureWindow = 10 * time.Minute
	// A player normally sits in one match; this stops one IP holding many rooms.
	maxRoomsPerIP = 8
)

// Hub is the server side of the relay: nodes sign in to it, and the theater
// asks it to probe consoles and open rooms.
type Hub struct {
	log    *slog.Logger
	tokens map[string]tokenEntry
	tls    *tls.Config

	mu       sync.Mutex
	links    map[string]*link
	order    []string // connection order, for ties
	nextID   int
	latency  map[latencyKey]latencyEntry
	measured map[netip.Addr]bool
	rooms    map[string]*Room
	failures map[netip.Addr][]time.Time
}

type latencyKey struct {
	ip   netip.Addr
	node string
}

type latencyEntry struct {
	rtt float64 // ms; negative when unanswered
	at  time.Time
}

type link struct {
	name    string
	ip      netip.Addr
	conn    net.Conn
	wmu     sync.Mutex
	waiting map[string]chan message
}

func (l *link) send(m message) error {
	line, _ := json.Marshal(m)
	l.wmu.Lock()
	defer l.wmu.Unlock()
	l.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	_, err := l.conn.Write(append(line, '\n'))
	return err
}

// Room is one match's room on one relay: the relay's public IP and the port
// standing for each player (by key).
type Room struct {
	ID    string
	IP    netip.Addr
	label string
	link  *link
	// Ports and ips are guarded by the hub's mutex; use Port.
	ports  map[string]int
	ips    map[string]netip.Addr
	closed bool
}

// LoadHub reads the relay directory. A missing directory means no relay.
func LoadHub(dir string, log *slog.Logger) (*Hub, error) {
	tokens, err := loadTokens(filepath.Join(dir, TokensFile))
	if err != nil {
		return nil, err
	}
	conf, err := serverTLS(dir)
	if err != nil {
		return nil, err
	}
	return newHub(tokens, conf, log), nil
}

func newHub(tokens map[string]tokenEntry, conf *tls.Config, log *slog.Logger) *Hub {
	return &Hub{log: log, tokens: tokens, tls: conf, links: map[string]*link{}, latency: map[latencyKey]latencyEntry{},
		measured: map[netip.Addr]bool{}, rooms: map[string]*Room{}, failures: map[netip.Addr][]time.Time{}}
}

func (h *Hub) id() string {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.nextID++
	return strconv.Itoa(h.nextID)
}

// Available reports whether any relay node is connected.
func (h *Hub) Available() bool {
	if h == nil {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.links) > 0
}

// Serve accepts node control connections until ctx ends.
func (h *Hub) Serve(ctx context.Context, ln net.Listener) error {
	tl := tls.NewListener(ln, h.tls)
	// Cancelling on any return closes the listener and every node link, so a
	// fatal Accept error is returned instead of waiting on live links.
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(ctx, func() { tl.Close() })
	defer stop()
	h.log.Info("relay control listening", "addr", ln.Addr().String(), "nodes", len(h.tokens))
	var wg sync.WaitGroup
	defer wg.Wait()
	defer cancel()
	for {
		conn, err := tl.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			return err
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.client(ctx, conn)
		}()
	}
}

func (h *Hub) throttled(ip netip.Addr) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	var recent []time.Time
	for _, t := range h.failures[ip] {
		if time.Since(t) < authFailureWindow {
			recent = append(recent, t)
		}
	}
	h.failures[ip] = recent
	return len(recent) >= authFailureLimit
}

func (h *Hub) authFailed(ip netip.Addr) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.failures) > 4096 {
		clear(h.failures)
	}
	h.failures[ip] = append(h.failures[ip], time.Now())
}

func (h *Hub) authenticate(token string, peer netip.Addr) (string, bool) {
	for name, e := range h.tokens {
		if tokenMatches(token, e) {
			return name, e.ip == peer
		}
	}
	return "", false
}

func (h *Hub) client(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	peer := netip.Addr{}
	if a, ok := conn.RemoteAddr().(*net.TCPAddr); ok {
		peer = a.AddrPort().Addr().Unmap()
	}
	if h.throttled(peer) {
		h.log.Info("relay control refused: too many failed attempts", "from", peer)
		return
	}
	r := bufio.NewReaderSize(conn, maxLine)
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	line, err := r.ReadSlice('\n')
	var hello message
	if err != nil || json.Unmarshal(line, &hello) != nil || hello.Op != "hello" {
		h.authFailed(peer)
		h.log.Info("relay control closed before hello", "from", peer)
		return
	}
	name, ok := h.authenticate(hello.Token, peer)
	if !ok {
		h.authFailed(peer)
		h.log.Warn("relay control refused: unknown token or not its registered IP", "from", peer)
		return
	}
	// Players are always sent to the registered IP, never to one the node claims.
	l := &link{name: name, ip: h.tokens[name].ip, conn: conn, waiting: map[string]chan message{}}
	if hello.PublicIP != "" && hello.PublicIP != l.ip.String() {
		h.log.Warn("relay announced a different public IP; using its registered one", "node", name,
			"announced", hello.PublicIP, "registered", l.ip)
	}
	h.mu.Lock()
	if old := h.links[name]; old != nil {
		old.conn.Close()
	}
	h.links[name] = l
	h.order = append(slicesWithout(h.order, name), name)
	h.mu.Unlock()
	h.log.Info("relay connected", "node", name, "from", peer, "public_ip", l.ip)

	pingDone := make(chan struct{})
	go func() {
		t := time.NewTicker(pingEvery)
		defer t.Stop()
		for {
			select {
			case <-pingDone:
				return
			case <-t.C:
				if l.send(message{Op: "ping"}) != nil {
					conn.Close()
					return
				}
			}
		}
	}()
	defer close(pingDone)
	defer h.dropLink(l)
	for {
		conn.SetReadDeadline(time.Now().Add(3 * pingEvery))
		line, err := r.ReadSlice('\n')
		if err != nil {
			return
		}
		var m message
		if json.Unmarshal(line, &m) != nil {
			return
		}
		if m.Op == "closed" {
			// The answer to a close, or a room the node expired. forwarded > 0
			// means the players actually talked through the relay.
			h.mu.Lock()
			room := h.rooms[m.Room]
			delete(h.rooms, m.Room)
			if room != nil {
				room.closed = true
			}
			h.mu.Unlock()
			label := ""
			if room != nil {
				label = room.label
			}
			h.log.Info("relay room closed", "room", m.Room, "match", label, "node", name, "forwarded", m.Forwarded,
				"dropped", m.Dropped, "rejected", m.Rejected, "idle_s", m.IdleS)
			continue
		}
		h.mu.Lock()
		ch := l.waiting[m.ID]
		h.mu.Unlock()
		if ch != nil {
			select {
			case ch <- m:
			default:
			}
		}
	}
}

func slicesWithout(s []string, v string) []string {
	out := s[:0]
	for _, x := range s {
		if x != v {
			out = append(out, x)
		}
	}
	return out
}

func (h *Hub) dropLink(l *link) {
	h.mu.Lock()
	defer h.mu.Unlock()
	// A link replaced by a reconnect still loses its rooms; only the current
	// link is removed from the node list.
	if h.links[l.name] == l {
		delete(h.links, l.name)
		h.order = slicesWithout(h.order, l.name)
	}
	var lost []string
	for id, room := range h.rooms {
		if room.link == l {
			// The node keeps live rooms through a control outage, but new joins
			// can't be added to them from here.
			room.closed = true
			delete(h.rooms, id)
			lost = append(lost, id)
		}
	}
	h.log.Info("relay disconnected", "node", l.name, "rooms_lost", lost)
}

// request sends m on l and waits for the reply with the same ID.
func (h *Hub) request(ctx context.Context, l *link, m message, timeout time.Duration) (message, error) {
	ch := make(chan message, 1)
	h.mu.Lock()
	l.waiting[m.ID] = ch
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(l.waiting, m.ID)
		h.mu.Unlock()
	}()
	if err := l.send(m); err != nil {
		return message{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	select {
	case reply := <-ch:
		return reply, nil
	case <-ctx.Done():
		return message{}, ctx.Err()
	}
}

// ProbeAll reports, per answering relay, whether target answered an unsolicited
// PROB, meaning its router admits that relay. Round trips feed relay choice.
func (h *Hub) ProbeAll(ctx context.Context, target netip.AddrPort) map[string]bool {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	links := make([]*link, 0, len(h.links))
	for _, l := range h.links {
		links = append(links, l)
	}
	h.mu.Unlock()
	type result struct {
		name      string
		reachable bool
		ok        bool
	}
	results := make(chan result, len(links))
	for _, l := range links {
		go func() {
			reply, err := h.request(ctx, l, message{Op: "probe", ID: "probe-" + h.id(), Target: target.String()}, probeTimeout)
			if err != nil || reply.Op != "probed" {
				h.log.Info("relay did not answer probe", "node", l.name, "error", err, "reply", reply.Error)
				results <- result{name: l.name}
				return
			}
			e := latencyEntry{rtt: -1, at: time.Now()}
			if reply.Reachable {
				e.rtt = reply.RTT
			}
			h.mu.Lock()
			if len(h.latency) >= maxLatency {
				clear(h.latency)
			}
			h.latency[latencyKey{target.Addr(), l.name}] = e
			h.mu.Unlock()
			results <- result{name: l.name, reachable: reply.Reachable, ok: true}
		}()
	}
	out := map[string]bool{}
	for range links {
		if r := <-results; r.ok {
			out[r.name] = r.reachable
		}
	}
	return out
}

// Measure refreshes a player's round trips to every relay, at most every
// latencyRefresh.
func (h *Hub) Measure(ctx context.Context, target netip.AddrPort) {
	if h == nil {
		return
	}
	ip := target.Addr()
	h.mu.Lock()
	fresh := true
	for name := range h.links {
		e, ok := h.latency[latencyKey{ip, name}]
		if !ok || time.Since(e.at) >= latencyRefresh {
			fresh = false
		}
	}
	if len(h.links) == 0 || fresh || h.measured[ip] {
		h.mu.Unlock()
		return
	}
	h.measured[ip] = true
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		delete(h.measured, ip)
		h.mu.Unlock()
	}()
	results := h.ProbeAll(ctx, target)
	h.log.Info("relay latency", "ip", ip, "reachable", results)
}

// rtt is a fresh round trip from node to ip, if measured. Callers hold h.mu.
func (h *Hub) rtt(ip netip.Addr, node string) (float64, bool) {
	e, ok := h.latency[latencyKey{ip, node}]
	if !ok || e.rtt < 0 || time.Since(e.at) > latencyMaxAge {
		return 0, false
	}
	return e.rtt, true
}

// choose picks the relay with the shortest total round trip to these players,
// limited to allowed when it is non-nil. Callers hold h.mu.
func (h *Hub) choose(ips []netip.Addr, allowed map[string]bool) *link {
	var best *link
	var bestCost float64
	for _, name := range h.order {
		if allowed != nil && !allowed[name] {
			continue
		}
		cost := 0.0
		for _, ip := range ips {
			if rtt, ok := h.rtt(ip, name); ok {
				cost += rtt
			} else {
				cost += unmeasuredMS
			}
		}
		if best == nil || cost < bestCost {
			best, bestCost = h.links[name], cost
		}
	}
	return best
}

// busy lists player IPs already in maxRoomsPerIP open rooms. Callers hold h.mu.
func (h *Hub) busy(ips []netip.Addr) []netip.Addr {
	var out []netip.Addr
	for _, ip := range ips {
		n := 0
		for _, room := range h.rooms {
			for _, other := range room.ips {
				if !room.closed && other == ip {
					n++
					break
				}
			}
		}
		if n >= maxRoomsPerIP {
			out = append(out, ip)
		}
	}
	return out
}

// OpenRoom opens a room on the closest relay for members (key -> endpoint),
// limited to allowed relay names when allowed is non-nil.
func (h *Hub) OpenRoom(ctx context.Context, members map[string]netip.AddrPort, allowed map[string]bool, label string) *Room {
	if h == nil {
		return nil
	}
	var ips []netip.Addr
	for _, ep := range members {
		ips = append(ips, ep.Addr())
	}
	h.mu.Lock()
	if busy := h.busy(ips); len(busy) > 0 {
		h.mu.Unlock()
		h.log.Warn("relay room refused: player already in too many rooms", "match", label, "ips", busy)
		return nil
	}
	l := h.choose(ips, allowed)
	if l == nil {
		h.mu.Unlock()
		h.log.Info("relay room: no relay", "match", label)
		return nil
	}
	h.nextID++
	room := &Room{ID: strconv.Itoa(h.nextID), IP: l.ip, label: label, link: l, ports: map[string]int{}, ips: map[string]netip.Addr{}}
	h.rooms[room.ID] = room
	h.mu.Unlock()
	h.log.Info("relay choice", "match", label, "room", room.ID, "node", l.name)
	if !h.add(ctx, room, members) {
		// Close the new room on the node too, releasing any ports it bound.
		h.mu.Lock()
		delete(h.rooms, room.ID)
		room.closed = true
		live := h.links[l.name] == l
		h.mu.Unlock()
		if live {
			l.send(message{Op: "close", Room: room.ID})
		}
		return nil
	}
	return room
}

// Node names the relay that holds room.
func (h *Hub) Node(room *Room) string { return room.link.name }

// AddMembers adds players to an open room.
func (h *Hub) AddMembers(ctx context.Context, room *Room, members map[string]netip.AddrPort) bool {
	h.mu.Lock()
	if room.closed || h.links[room.link.name] != room.link {
		h.mu.Unlock()
		return false
	}
	var ips []netip.Addr
	for key, ep := range members {
		if _, ok := room.ips[key]; !ok {
			ips = append(ips, ep.Addr())
		}
	}
	busy := h.busy(ips)
	h.mu.Unlock()
	if len(busy) > 0 {
		h.log.Warn("relay room refused: player already in too many rooms", "room", room.ID, "ips", busy)
		return false
	}
	return h.add(ctx, room, members)
}

func (h *Hub) add(ctx context.Context, room *Room, members map[string]netip.AddrPort) bool {
	wire := make(map[string]string, len(members))
	for k, ep := range members {
		wire[k] = ep.String()
	}
	reply, err := h.request(ctx, room.link, message{Op: "add", ID: h.id(), Room: room.ID, Members: wire}, openTimeout)
	if err != nil || reply.Op != "added" {
		h.log.Info("relay refused room", "room", room.ID, "match", room.label, "node", room.link.name, "error", err, "reason", reply.Error)
		return false
	}
	h.mu.Lock()
	for k, p := range reply.Ports {
		room.ports[k] = p
	}
	for k, ep := range members {
		room.ips[k] = ep.Addr()
	}
	ports := make([]string, 0, len(room.ports))
	for k, p := range room.ports {
		ports = append(ports, k+":"+strconv.Itoa(p))
	}
	h.mu.Unlock()
	sort.Strings(ports)
	h.log.Info("relay room ready", "room", room.ID, "match", room.label, "node", room.link.name, "ports", strings.Join(ports, " "))
	return true
}

// Port is the relay port standing for the member with key, if any.
func (h *Hub) Port(room *Room, key string) (netip.AddrPort, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	p, ok := room.ports[key]
	if !ok || room.closed {
		return netip.AddrPort{}, false
	}
	return netip.AddrPortFrom(room.IP, uint16(p)), true
}

// Closed reports whether the room is gone (closed here or lost with its relay).
func (h *Hub) Closed(room *Room) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return room.closed
}

// RemoveMembers takes players who left out of a room.
func (h *Hub) RemoveMembers(room *Room, keys ...string) {
	if h == nil || room == nil {
		return
	}
	h.mu.Lock()
	var gone []string
	for _, k := range keys {
		if _, ok := room.ips[k]; ok {
			gone = append(gone, k)
			delete(room.ips, k)
			delete(room.ports, k)
		}
	}
	live := !room.closed && h.links[room.link.name] == room.link
	h.mu.Unlock()
	if len(gone) > 0 && live {
		h.log.Info("relay room remove", "room", room.ID, "match", room.label, "keys", gone)
		room.link.send(message{Op: "remove", Room: room.ID, Keys: gone})
	}
}

// CloseRoom closes a room. The relay reports its traffic when it is done.
func (h *Hub) CloseRoom(room *Room) {
	if h == nil || room == nil {
		return
	}
	h.mu.Lock()
	if room.closed {
		h.mu.Unlock()
		return
	}
	room.closed = true
	live := h.links[room.link.name] == room.link
	if !live {
		delete(h.rooms, room.ID)
	}
	h.mu.Unlock()
	if live {
		h.log.Info("relay room closing", "room", room.ID, "match", room.label, "node", room.link.name)
		room.link.send(message{Op: "close", Room: room.ID})
	}
}
