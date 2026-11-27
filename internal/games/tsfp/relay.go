package tsfp

import (
	"bytes"
	"errors"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"time"

	"github.com/dead-game-entrepreneurs/rei-nation/internal/protocol/aries"
)

const relayFirstPort, relayLastPort = 20000, 20255

type relayRoute uint8

const (
	direct relayRoute = iota
	forcedRelay
	fallbackRelay
)

type relayRoom struct {
	mu      sync.Mutex
	ip      netip.Addr
	members map[int]*relayMember
}

type relayMember struct {
	socket   *net.UDPConn
	endpoint netip.AddrPort
	heard    bool
	budget   float64
	last     time.Time
	closed   bool
}

func (t *theater) joinRelay(g *match, pid int, personaID int64, endpoint netip.AddrPort) (netip.AddrPort, relayRoute) {
	if !t.relayIP.IsValid() || t.relayMode == "off" {
		return endpoint, direct
	}
	route := direct
	switch {
	case t.relayMode == "always", time.Now().Before(t.failedUntil[personaID]):
		route = forcedRelay
	case g.relayReachable && endpoint.Addr() != g.external.Addr():
		route = fallbackRelay
	case g.relay == nil:
		return endpoint, direct
	}
	room := g.relay
	if room == nil {
		// The game treats packets from the Theater IP as control traffic.
		room = &relayRoom{ip: t.relayIP, members: make(map[int]*relayMember)}
		if room.add(1, t.endpoint(g.owner, g.external)) != nil {
			return endpoint, direct
		}
		for otherPID, p := range g.players {
			if otherPID == 1 {
				continue
			}
			if room.add(otherPID, t.endpoint(p.client, p.endpoint)) != nil {
				room.close()
				return endpoint, direct
			}
		}
	}
	if room.add(pid, endpoint) != nil {
		if g.relay == nil {
			room.close()
		}
		return endpoint, direct
	}
	g.relay = room
	if route == forcedRelay {
		return room.address(pid), route
	}
	return endpoint, route
}

func (r *relayRoom) add(pid int, endpoint netip.AddrPort) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	socket, err := openRelaySocket(r.ip)
	if err != nil {
		return err
	}
	member := &relayMember{socket: socket, endpoint: endpoint, budget: 1 << 20, last: time.Now()}
	r.members[pid] = member
	go r.receive(member)
	return nil
}

func openRelaySocket(ip netip.Addr) (*net.UDPConn, error) {
	for port := relayFirstPort; port <= relayLastPort; port++ {
		addr := netip.AddrPortFrom(ip, uint16(port))
		if socket, err := net.ListenUDP("udp4", net.UDPAddrFromAddrPort(addr)); err == nil {
			return socket, nil
		}
	}
	return nil, errors.New("no TSFP relay port is available")
}

func (t *theater) probeRelay(g *match) {
	if !t.relayIP.IsValid() || t.relayMode == "off" || t.relayMode == "always" {
		return
	}
	target := t.endpoint(g.owner, g.external)
	if g.relayProbeAddr == target && time.Since(g.relayProbedAt) < 5*time.Minute {
		return
	}
	select {
	case t.probeSlots <- struct{}{}:
	default:
		return
	}
	if g.relayProbeAddr != target {
		g.relayReachable = false
	}
	g.relayProbeAddr, g.relayProbedAt = target, time.Now()
	probed := g.relayProbedAt
	go func() {
		defer func() { <-t.probeSlots }()
		ok := relayProbe(t.relayIP, target)
		t.mu.Lock()
		defer t.mu.Unlock()
		if t.games[g.id] == g && g.relayProbedAt == probed {
			g.relayReachable = ok
		}
	}()
}

func relayProbe(ip netip.Addr, target netip.AddrPort) bool {
	socket, err := openRelaySocket(ip)
	if err != nil {
		return false
	}
	defer socket.Close()
	n, _ := strconv.ParseUint(token(4), 16, 32)
	tid := strconv.FormatUint(n%0x7ffffffe+1, 10)
	probe := packet("PROB", "TID", tid, "TYPE", "1")
	probe.Code[0] = 0xc0
	var wire bytes.Buffer
	wire.Write([]byte{0xff, 0xff})
	if probe.Write(&wire) != nil {
		return false
	}
	var buf [128]byte
	for range 4 {
		if _, err := socket.WriteToUDPAddrPort(wire.Bytes(), target); err != nil {
			return false
		}
		if socket.SetReadDeadline(time.Now().Add(500*time.Millisecond)) != nil {
			return false
		}
		for {
			size, from, err := socket.ReadFromUDPAddrPort(buf[:])
			if err != nil {
				break
			}
			if from.Addr() != target.Addr() {
				continue
			}
			data := bytes.TrimPrefix(buf[:size], []byte{0xff, 0xff})
			if len(data) < 12 || data[8] != 0 || data[9] != 0 || data[10] != 0 || int(data[11]) != len(data) {
				continue
			}
			frame, err := aries.Read(bytes.NewReader(data))
			if err == nil && frame.Tag == "PROB" && frame.Code == [4]byte{} && fields(frame)["TID"] == tid {
				return true
			}
		}
	}
	return false
}

func (r *relayRoom) address(pid int) netip.AddrPort {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.members[pid].socket.LocalAddr().(*net.UDPAddr).AddrPort()
}

func (r *relayRoom) remove(pid int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if member := r.members[pid]; member != nil {
		delete(r.members, pid)
		member.closed = true
		member.socket.Close()
	}
}

func (r *relayRoom) close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for pid, member := range r.members {
		delete(r.members, pid)
		member.closed = true
		member.socket.Close()
	}
}

func (r *relayRoom) receive(target *relayMember) {
	var data [2049]byte
	for {
		n, from, err := target.socket.ReadFromUDPAddrPort(data[:])
		if err != nil {
			return
		}
		if n > 0 && n <= 2048 {
			r.forward(target, from, data[:n])
		}
	}
}

func (r *relayRoom) forward(target *relayMember, from netip.AddrPort, data []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if target.closed {
		return
	}
	var exact, unseen, candidate *relayMember
	var exactCount, unseenCount, candidates int
	for _, p := range r.members {
		if p == target || p.endpoint.Addr() != from.Addr() {
			continue
		}
		candidate, candidates = p, candidates+1
		if p.endpoint == from {
			exact, exactCount = p, exactCount+1
		}
		if !p.heard {
			unseen, unseenCount = p, unseenCount+1
		}
	}
	var sender *relayMember
	switch {
	case exactCount == 1:
		sender = exact
	case exactCount == 0 && candidates == 1:
		sender = candidate
	case exactCount == 0 && unseenCount == 1:
		sender = unseen
	default:
		return
	}
	now := time.Now()
	sender.budget = min(1<<20, sender.budget+now.Sub(sender.last).Seconds()*(512<<10))
	sender.last = now
	if sender.budget < float64(len(data)) {
		return
	}
	sender.budget -= float64(len(data))
	sender.endpoint, sender.heard = from, true
	_, _ = sender.socket.WriteToUDPAddrPort(data, target.endpoint)
}
