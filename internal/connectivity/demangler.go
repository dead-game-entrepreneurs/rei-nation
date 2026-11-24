// Package connectivity implements the shared DirtySock peer introduction service.
package connectivity

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"
)

const DemanglerPort uint16 = 3658

// Demangler answers ProtoMangle v1 probes for a pair admitted by a game session.
// Advertised peers come from observed UDP endpoints.
type Demangler struct {
	publicIP netip.Addr
	port     uint16
	mu       sync.Mutex
	sessions map[string]*session
}

type session struct {
	ip      [2]netip.Addr
	expires time.Time
	clients [2]peer
}

type peer struct {
	tag      string
	local    netip.AddrPort
	observed netip.AddrPort
	seen     time.Time
}

func NewDemangler(publicIP netip.Addr, port uint16) *Demangler {
	return &Demangler{publicIP: publicIP, port: port, sessions: make(map[string]*session)}
}

func (d *Demangler) Admit(id string, host, guest netip.Addr, expires time.Time) func() {
	id = strings.ToLower(id)
	d.mu.Lock()
	defer d.mu.Unlock()
	for key, existing := range d.sessions {
		if time.Now().After(existing.expires) {
			delete(d.sessions, key)
		}
	}
	s := &session{ip: [2]netip.Addr{host, guest}, expires: expires}
	for i := range s.clients {
		var token [12]byte
		rand.Read(token[:])
		s.clients[i].tag = hex.EncodeToString(token[:])
	}
	if len(d.sessions) < 128 || d.sessions[id] != nil {
		d.sessions[id] = s
	}
	return func() {
		d.mu.Lock()
		defer d.mu.Unlock()
		if d.sessions[id] == s {
			delete(d.sessions, id)
		}
	}
}

func (d *Demangler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Older clients read the key/value body directly and expect a fixed size.
	body := "status=failure\n"
	if source, err := netip.ParseAddrPort(r.RemoteAddr); err == nil {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/getPeerAddress":
			body = d.getPeer(r, source.Addr().Unmap())
		case r.Method == http.MethodPost && r.URL.Path == "/connectionStatus":
			body = ""
		}
	}
	w.Header().Set("Content-Type", "text/plain")
	w.Header().Set("Connection", "close")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	fmt.Fprint(w, body)
}

func (d *Demangler) getPeer(r *http.Request, source netip.Addr) string {
	const failure = "status=failure\n"
	cookie, err := r.Cookie("sessionID")
	q := r.URL.Query()
	localIP, ipErr := netip.ParseAddr(q.Get("myIP"))
	localPort, portErr := strconv.ParseUint(q.Get("myPort"), 10, 16)
	if err != nil || ipErr != nil || !localIP.Is4() || portErr != nil || localPort == 0 || q.Get("version") != "1.0" {
		return failure
	}
	local := netip.AddrPortFrom(localIP, uint16(localPort))
	now := time.Now()
	d.mu.Lock()
	defer d.mu.Unlock()
	s := d.sessions[strings.ToLower(strings.Trim(cookie.Value, "\""))]
	if s == nil || now.After(s.expires) {
		return failure
	}
	i := -1
	for index, candidate := range s.clients {
		if s.ip[index] == source && candidate.local == local {
			i = index
			break
		}
	}
	if i == -1 {
		for index, candidate := range s.clients {
			if s.ip[index] == source && !candidate.local.IsValid() {
				i = index
				s.clients[i].local = local
				break
			}
		}
	}
	if i == -1 {
		return failure
	}
	me, other := s.clients[i], s.clients[1-i]
	if now.Sub(me.seen) < time.Minute && now.Sub(other.seen) < time.Minute {
		addr := other.observed
		if s.ip[0] == s.ip[1] && other.local.Addr().IsPrivate() {
			addr = other.local
		}
		return fmt.Sprintf("status=success\npeerIP=%s\npeerPort=%d\n", addr.Addr(), addr.Port())
	}
	return fmt.Sprintf("status=probe\ntargetIP-0=%s\ntargetPort-0=%d\ntag-0=%s\nsendCount-0=4\nsourcePort-0=%d\n",
		d.publicIP, d.port, me.tag, localPort)
}

// Probe returns true for ProtoMangle datagrams, including invalid and expired tags.
func (d *Demangler) Probe(from netip.AddrPort, data []byte) bool {
	if !bytes.HasPrefix(data, []byte("sourceIP=")) && !bytes.HasPrefix(data, []byte("tag=")) {
		return false
	}
	if len(data) > 2048 {
		return true
	}
	var tag string
	for line := range strings.SplitSeq(string(data), "\n") {
		if val, ok := strings.CutPrefix(line, "tag="); ok {
			tag = strings.TrimSpace(val)
			break
		}
	}
	if len(tag) != 24 {
		return true
	}
	now := time.Now()
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, s := range d.sessions {
		if now.After(s.expires) {
			continue
		}
		for i := range s.clients {
			p := &s.clients[i]
			if p.tag == tag && p.local.IsValid() && s.ip[i] == from.Addr() {
				if p.observed == from || now.Sub(p.seen) >= time.Minute {
					p.observed, p.seen = from, now
				}
				return true
			}
		}
	}
	return true
}

func ServeHTTP(ctx context.Context, listener net.Listener, h http.Handler) error {
	s := &http.Server{Handler: h, ReadHeaderTimeout: 3 * time.Second, ReadTimeout: 5 * time.Second,
		WriteTimeout: 5 * time.Second, MaxHeaderBytes: 4096, BaseContext: func(net.Listener) context.Context { return ctx }}
	stop := context.AfterFunc(ctx, func() { s.Close() })
	defer stop()
	err := s.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func ServeUDP(ctx context.Context, s *net.UDPConn, receive func(netip.AddrPort, []byte)) error {
	stop := context.AfterFunc(ctx, func() { s.Close() })
	defer stop()
	// Leave room for the full UDP datagram so large peer packets aren't truncated.
	buf := make([]byte, 65536)
	for {
		n, from, err := s.ReadFromUDPAddrPort(buf)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		from = netip.AddrPortFrom(from.Addr().Unmap(), from.Port())
		receive(from, buf[:n])
	}
}
