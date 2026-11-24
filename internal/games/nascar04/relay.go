package nascar04

import (
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/dead-game-entrepreneurs/rei-nation/internal/connectivity"
)

const (
	guestPort = connectivity.DemanglerPort
	hostPort  = uint16(3659)
	peerPort  = uint16(9999)
)

// The original game connects to fixed ports. Each public address can belong
// to only one relay pair, because the peer packets carry no lobby identity.
type raceRelay struct {
	mu      sync.Mutex
	mode    string
	sockets map[uint16]*net.UDPConn
	pairs   map[netip.Addr]*relayPair
}

type relayPair struct {
	address [2]netip.Addr
	main    [2]netip.AddrPort
	extra   [2]netip.AddrPort
	expires time.Time
	ends    time.Time
}

func (r *raceRelay) admit(host, guest netip.Addr) func() {
	if r == nil || r.mode == "off" || host == guest {
		return nil
	}
	if r.mode != "always" && (!wanAddress(host) || !wanAddress(guest)) {
		return nil
	}
	now := time.Now()
	r.mu.Lock()
	defer r.mu.Unlock()
	for ip, p := range r.pairs {
		if now.After(p.expires) {
			delete(r.pairs, ip)
		}
	}
	if len(r.pairs) >= 128 || r.pairs[host] != nil || r.pairs[guest] != nil {
		return nil
	}
	p := &relayPair{address: [2]netip.Addr{host, guest}, expires: now.Add(20 * time.Minute), ends: now.Add(2 * time.Hour)}
	r.pairs[host], r.pairs[guest] = p, p
	return func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		for _, ip := range p.address {
			if r.pairs[ip] == p {
				delete(r.pairs, ip)
			}
		}
	}
}

func wanAddress(addr netip.Addr) bool {
	return addr.IsGlobalUnicast() && !addr.IsPrivate() &&
		!netip.MustParsePrefix("100.64.0.0/10").Contains(addr)
}

func (r *raceRelay) forward(port uint16, from netip.AddrPort, data []byte) {
	if len(data) == 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	p := r.pairs[from.Addr()]
	if p == nil || now.After(p.expires) {
		return
	}
	side := 0
	if from.Addr() == p.address[1] {
		side = 1
	}
	endpoints, out := &p.main, guestPort
	switch {
	case port == hostPort && side == 0:
	case port == guestPort && side == 1:
		out = hostPort
	case port == peerPort:
		endpoints, out = &p.extra, peerPort
	default:
		return
	}
	if current := endpoints[side]; current.IsValid() && current != from {
		return
	}
	endpoints[side] = from
	p.expires = now.Add(20 * time.Minute)
	if p.ends.Before(p.expires) {
		p.expires = p.ends
	}
	if to := endpoints[1-side]; to.IsValid() {
		s := r.sockets[out]
		if s.SetWriteDeadline(now.Add(100*time.Millisecond)) == nil {
			s.WriteToUDPAddrPort(data, to)
		}
	}
}
