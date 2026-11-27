package relay

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// idleRoom closes a room with no traffic. In-match traffic is constant.
	idleRoom    = 180 * time.Second
	maxDatagram = 2048
	// Per member, for everything it sends. A host streams a few KB/s to each
	// guest; this only stops a room being used as a bulk pipe.
	memberBytesPerSecond = 512 * 1024
	memberBurstBytes     = 1024 * 1024
	maxMembers           = 16
	statsEvery           = time.Minute
	probeWait            = 2 * time.Second
	probeSends           = 4
)

// Node is a relay node: it opens rooms for the server and forwards datagrams
// between the players in each.
type Node struct {
	Bind         netip.Addr
	PortLow      int
	PortHigh     int
	MaxRooms     int
	AllowPrivate bool // only for LAN tests
	Log          *slog.Logger

	mu    sync.Mutex
	rooms map[string]*room
	used  map[int]bool
	// notify sends a message to the server while the control link is up.
	notify func(message)
}

type room struct {
	id                           string
	members                      map[string]*member
	last                         time.Time
	forwarded, dropped, rejected int64
	rejectedFrom                 map[netip.Addr]int
	loggedDrop                   time.Time
}

type member struct {
	key       string
	ip        netip.Addr
	endpoint  netip.AddrPort
	heard     bool // the real port was learned from a datagram
	conn      *net.UDPConn
	port      int
	allowance float64
	refilled  time.Time
	sent      int64
	sentBytes int64
}

// checkTarget accepts only public IPv4 player addresses unless allowPrivate.
func checkTarget(a netip.Addr, allowPrivate bool) error {
	a = a.Unmap()
	if !a.Is4() || a.IsMulticast() || a.IsUnspecified() {
		return fmt.Errorf("not a usable address: %s", a)
	}
	if !allowPrivate && (!a.IsGlobalUnicast() || a.IsPrivate() || a.IsLoopback() || a.IsLinkLocalUnicast()) {
		return fmt.Errorf("not a public address: %s", a)
	}
	return nil
}

func parseEndpoint(s string, allowPrivate bool) (netip.AddrPort, error) {
	ap, err := netip.ParseAddrPort(s)
	if err != nil || ap.Port() == 0 {
		return netip.AddrPort{}, fmt.Errorf("bad endpoint %q", s)
	}
	ap = netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port())
	return ap, checkTarget(ap.Addr(), allowPrivate)
}

func (n *Node) init() {
	if n.rooms == nil {
		n.rooms = make(map[string]*room)
		n.used = make(map[int]bool)
	}
	if n.MaxRooms == 0 {
		n.MaxRooms = 64
	}
	if n.Log == nil {
		n.Log = slog.Default()
	}
}

// bind opens a random free port of the range. Callers hold n.mu.
func (n *Node) bind() (*net.UDPConn, int, error) {
	var free []int
	for p := n.PortLow; p <= n.PortHigh; p++ {
		if !n.used[p] {
			free = append(free, p)
		}
	}
	lastErr := errors.New("no free relay port")
	for i := 0; i < 64 && len(free) > 0; i++ {
		j, _ := rand.Int(rand.Reader, big.NewInt(int64(len(free))))
		p := free[j.Int64()]
		free[j.Int64()] = free[len(free)-1]
		free = free[:len(free)-1]
		conn, err := net.ListenUDP("udp4", net.UDPAddrFromAddrPort(netip.AddrPortFrom(n.Bind, uint16(p))))
		if err == nil {
			n.used[p] = true
			return conn, p, nil
		}
		lastErr = fmt.Errorf("no free relay port: %w", err)
	}
	return nil, 0, lastErr
}

// Add puts members (key -> endpoint) in room id, opening it if needed, and
// returns the relay port standing for each member of the room.
func (n *Node) Add(id string, members map[string]string) (map[string]int, error) {
	checked := make(map[string]netip.AddrPort, len(members))
	for key, ep := range members {
		ap, err := parseEndpoint(ep, n.AllowPrivate)
		if err != nil {
			return nil, err
		}
		if len(key) > 16 {
			key = key[:16]
		}
		checked[key] = ap
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	n.init()
	r := n.rooms[id]
	if r == nil {
		if len(n.rooms) >= n.MaxRooms {
			return nil, errors.New("relay full")
		}
		r = &room{id: id, members: map[string]*member{}, rejectedFrom: map[netip.Addr]int{}}
		n.rooms[id] = r
		n.Log.Info("relay room opened", "room", id)
	}
	total := len(r.members)
	for key := range checked {
		if r.members[key] == nil {
			total++
		}
	}
	if total > maxMembers {
		return nil, errors.New("room full")
	}
	now := time.Now()
	var added []string
	for key, ap := range checked {
		if m := r.members[key]; m != nil {
			m.ip, m.endpoint = ap.Addr(), ap
			continue
		}
		conn, port, err := n.bind()
		if err != nil {
			if len(r.members) == 0 {
				delete(n.rooms, id)
			}
			return nil, err
		}
		m := &member{key: key, ip: ap.Addr(), endpoint: ap, conn: conn, port: port, allowance: memberBurstBytes, refilled: now}
		r.members[key] = m
		go n.read(r, m)
		added = append(added, key+"@"+ap.String())
	}
	r.last = now
	ports := r.ports()
	n.Log.Info("relay room added", "room", id, "members", added, "ports", ports)
	return ports, nil
}

func (r *room) ports() map[string]int {
	out := make(map[string]int, len(r.members))
	for k, m := range r.members {
		out[k] = m.port
	}
	return out
}

// read forwards datagrams sent to the port standing for target.
func (n *Node) read(r *room, target *member) {
	buf := make([]byte, maxDatagram+1)
	for {
		size, from, err := target.conn.ReadFromUDPAddrPort(buf)
		if err != nil {
			return
		}
		n.received(r, target, buf[:size], netip.AddrPortFrom(from.Addr().Unmap(), from.Port()))
	}
}

// sender finds a datagram's member by exact endpoint, else by shared IP.
// See docs/games/gera/internet-play.md#shared-public-ips.
func (r *room) sender(from netip.AddrPort, target *member) *member {
	var candidates []*member
	for _, m := range r.members {
		if m.endpoint == from {
			return m
		}
		if m.ip == from.Addr() && m != target {
			candidates = append(candidates, m)
		}
	}
	if len(candidates) == 1 {
		return candidates[0]
	}
	var unheard []*member
	for _, m := range candidates {
		if !m.heard {
			unheard = append(unheard, m)
		}
	}
	if len(unheard) == 1 {
		return unheard[0]
	}
	return nil
}

func (n *Node) received(r *room, target *member, data []byte, from netip.AddrPort) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if r.members[target.key] != target {
		return // removed meanwhile
	}
	var s *member
	if len(data) <= maxDatagram {
		s = r.sender(from, target)
	}
	if s == nil || s == target {
		r.rejected++
		if _, seen := r.rejectedFrom[from.Addr()]; !seen && len(r.rejectedFrom) < 64 {
			n.Log.Info("relay rejected datagram", "room", r.id, "from", from.String(), "member", target.key, "bytes", len(data))
		}
		if len(r.rejectedFrom) < 64 {
			r.rejectedFrom[from.Addr()]++
		}
		return
	}
	now := time.Now()
	allowance := min(memberBurstBytes, s.allowance+now.Sub(s.refilled).Seconds()*memberBytesPerSecond)
	s.refilled = now
	if allowance < float64(len(data)) {
		s.allowance = allowance
		r.dropped++
		if now.Sub(r.loggedDrop) > time.Minute {
			r.loggedDrop = now
			n.Log.Warn("relay member over its rate limit", "room", r.id, "member", s.key)
		}
		return
	}
	s.allowance = allowance - float64(len(data))
	if !s.heard || s.endpoint != from {
		n.Log.Info("relay member endpoint", "room", r.id, "member", s.key, "now", from.String(), "was", s.endpoint.String(), "heard", s.heard)
	}
	s.endpoint, s.heard = from, true
	s.sent++
	s.sentBytes += int64(len(data))
	r.last = now
	if _, err := s.conn.WriteToUDPAddrPort(data, target.endpoint); err == nil {
		r.forwarded++
	}
}

// Remove takes members out of a room.
func (n *Node) Remove(id string, keys []string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.init()
	r := n.rooms[id]
	if r == nil {
		return
	}
	for _, k := range keys {
		if m := r.members[k]; m != nil {
			n.Log.Info("relay room removed member", "room", id, "member", k, "sent", m.sent)
			n.closeMember(r, m)
		}
	}
}

func (n *Node) closeMember(r *room, m *member) {
	m.conn.Close()
	delete(n.used, m.port)
	delete(r.members, m.key)
}

// Close closes a room and returns its traffic for the server's log.
func (n *Node) Close(id string) message {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.init()
	return n.closeLocked(id)
}

func (n *Node) closeLocked(id string) message {
	out := message{Op: "closed", Room: id}
	r := n.rooms[id]
	if r == nil {
		return out
	}
	for _, m := range r.members {
		n.closeMember(r, m)
	}
	delete(n.rooms, id)
	out.Forwarded, out.Dropped, out.Rejected = r.forwarded, r.dropped, r.rejected
	out.IdleS = time.Since(r.last).Round(100 * time.Millisecond).Seconds()
	n.Log.Info("relay room closed", "room", id, "forwarded", r.forwarded, "dropped", r.dropped, "rejected", r.rejected)
	return out
}

// Probe sends PROBs to target from a fresh port and returns the round trip,
// or false without an answer.
func (n *Node) Probe(ctx context.Context, target string) (time.Duration, bool, error) {
	ap, err := parseEndpoint(target, n.AllowPrivate)
	if err != nil {
		return 0, false, err
	}
	n.mu.Lock()
	n.init()
	conn, port, err := n.bind()
	n.mu.Unlock()
	if err != nil {
		return 0, false, err
	}
	defer func() {
		conn.Close()
		n.mu.Lock()
		delete(n.used, port)
		n.mu.Unlock()
	}()
	var b [4]byte
	rand.Read(b[:])
	tid := binary.BigEndian.Uint32(b[:])%999999999 + 1
	answered := make(chan time.Time, 1)
	go func() {
		buf := make([]byte, 256)
		for {
			size, from, err := conn.ReadFromUDPAddrPort(buf)
			if err != nil {
				return
			}
			if from.Addr().Unmap() == ap.Addr() {
				if got, ok := probeReplyTID(buf[:size]); ok && got == tid {
					select {
					case answered <- time.Now():
					default:
					}
					return
				}
			}
		}
	}()
	packet := probePacket(tid)
	for i := 0; i < probeSends; i++ {
		// Every send has the same TID, so time from the latest one: an answer to
		// an earlier send only makes the estimate smaller.
		sent := time.Now()
		conn.WriteToUDPAddrPort(packet, ap)
		select {
		case at := <-answered:
			return at.Sub(sent), true, nil
		case <-time.After(probeWait / probeSends):
		case <-ctx.Done():
			return 0, false, ctx.Err()
		}
	}
	return 0, false, nil
}

// expire closes idle rooms and logs room stats now and then.
func (n *Node) sweep(ctx context.Context) {
	tick := time.NewTicker(10 * time.Second)
	defer tick.Stop()
	lastStats := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		n.mu.Lock()
		n.init()
		for id, r := range n.rooms {
			if time.Since(r.last) >= idleRoom {
				closed := n.closeLocked(id)
				if n.notify != nil {
					n.notify(closed)
				}
			}
		}
		if time.Since(lastStats) >= statsEvery && len(n.rooms) > 0 {
			lastStats = time.Now()
			for _, r := range n.rooms {
				var players []string
				for _, m := range r.members {
					players = append(players, fmt.Sprintf("%s@%s/port%d:sent=%d,%dB", m.key, m.endpoint, m.port, m.sent, m.sentBytes))
				}
				sort.Strings(players)
				n.Log.Info("relay room stats", "room", r.id, "forwarded", r.forwarded, "dropped", r.dropped,
					"rejected", r.rejected, "idle_s", int(time.Since(r.last).Seconds()), "members", strings.Join(players, " "))
				clear(r.rejectedFrom)
			}
		}
		n.mu.Unlock()
	}
}

// handle answers one control message; nil means no reply.
func (n *Node) handle(ctx context.Context, m message) *message {
	switch m.Op {
	case "add":
		ports, err := n.Add(m.Room, m.Members)
		if err != nil {
			return &message{Op: "failed", ID: m.ID, Error: truncate(err.Error())}
		}
		return &message{Op: "added", ID: m.ID, Ports: ports}
	case "remove":
		n.Remove(m.Room, m.Keys)
	case "probe":
		rtt, ok, err := n.Probe(ctx, m.Target)
		if err != nil {
			return &message{Op: "failed", ID: m.ID, Error: truncate(err.Error())}
		}
		n.Log.Info("relay probe", "id", m.ID, "target", m.Target, "reachable", ok, "rtt_ms", rtt.Milliseconds())
		reply := &message{Op: "probed", ID: m.ID, Reachable: ok}
		if ok {
			reply.RTT = float64(rtt.Microseconds()) / 1000
		}
		return reply
	case "close":
		// Always answer, so the server can forget a room the node already expired.
		closed := n.Close(m.Room)
		return &closed
	case "ping":
		return &message{Op: "pong"}
	}
	return nil
}

func truncate(s string) string {
	if len(s) > 200 {
		return s[:200]
	}
	return s
}

// ControlConfig is how a node reaches the server.
type ControlConfig struct {
	Server   string // host:port of the server's relay control listener
	CertPEM  []byte // the server's control certificate, pinned
	Token    string
	PublicIP string // informational; the server uses the IP registered for the token
	Name     string
}

// Run keeps the control link to the server up and serves it until ctx ends.
// Rooms stay up through a control outage so live matches are not cut.
func (n *Node) Run(ctx context.Context, cfg ControlConfig) error {
	n.mu.Lock()
	n.init()
	n.mu.Unlock()
	tlsConf, err := pinnedClientConfig(cfg.CertPEM)
	if err != nil {
		return err
	}
	go n.sweep(ctx)
	const maxDelay = time.Minute
	delay := time.Second
	for ctx.Err() == nil {
		started := time.Now()
		err := n.serveControl(ctx, cfg, tlsConf)
		if ctx.Err() != nil {
			break
		}
		// A link that stayed up longer than the longest backoff starts over at 1 s.
		if time.Since(started) > maxDelay {
			delay = time.Second
		}
		n.Log.Info("relay control link down; retrying", "error", err, "delay", delay)
		select {
		case <-ctx.Done():
		case <-time.After(delay):
		}
		delay = min(delay*2, maxDelay)
	}
	n.mu.Lock()
	for id := range n.rooms {
		n.closeLocked(id)
	}
	n.mu.Unlock()
	return nil
}

func (n *Node) serveControl(ctx context.Context, cfg ControlConfig, tlsConf *tls.Config) error {
	dialer := &tls.Dialer{Config: tlsConf, NetDialer: &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}}
	conn, err := dialer.DialContext(ctx, "tcp", cfg.Server)
	if err != nil {
		return err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	var wmu sync.Mutex
	send := func(m message) {
		line, _ := json.Marshal(m)
		wmu.Lock()
		defer wmu.Unlock()
		conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
		conn.Write(append(line, '\n'))
	}
	send(message{Op: "hello", Token: cfg.Token, PublicIP: cfg.PublicIP, Name: cfg.Name})
	n.mu.Lock()
	n.notify = send
	rooms := len(n.rooms)
	n.mu.Unlock()
	defer func() {
		n.mu.Lock()
		n.notify = nil
		n.mu.Unlock()
	}()
	n.Log.Info("relay connected to server", "server", cfg.Server, "name", cfg.Name, "rooms_kept", rooms)
	r := bufio.NewReaderSize(conn, maxLine)
	for {
		// The server pings every 30 s; two missed pings mean the link is gone.
		conn.SetReadDeadline(time.Now().Add(75 * time.Second))
		line, err := r.ReadSlice('\n')
		if err != nil {
			return err
		}
		var m message
		if err := json.Unmarshal(line, &m); err != nil {
			return err
		}
		if m.Op == "probe" {
			// A probe waits for the console; don't hold up room changes.
			go func() {
				if reply := n.handle(ctx, m); reply != nil {
					send(*reply)
				}
			}()
			continue
		}
		if reply := n.handle(ctx, m); reply != nil {
			send(*reply)
		}
	}
}
