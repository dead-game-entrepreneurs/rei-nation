package tsfp

import (
	"bufio"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const relayControlPort = 18073
const relayControlLimit = 4096

var errRelayRoomLost = errors.New("remote relay room was lost")

type relayControlMessage struct {
	Op        string           `json:"op"`
	ID        string           `json:"id,omitempty"`
	Room      string           `json:"room,omitempty"`
	Token     string           `json:"token,omitempty"`
	PublicIP  string           `json:"public_ip,omitempty"`
	Name      string           `json:"name,omitempty"`
	Members   map[string][]any `json:"members,omitempty"`
	Ports     map[string]int   `json:"ports,omitempty"`
	Target    []any            `json:"target,omitempty"`
	Reachable bool             `json:"reachable,omitempty"`
	RTT       *float64         `json:"rtt_ms,omitempty"`
	Error     string           `json:"error,omitempty"`
}

type remoteRelayHub struct {
	ip     netip.Addr
	name   string
	digest [sha256.Size]byte
	tls    *tls.Config
	log    *slog.Logger
	next   atomic.Uint64
	mu     sync.Mutex
	link   *relayControlLink
	prefix string
}

type relayControlLink struct {
	conn    net.Conn
	done    chan struct{}
	once    sync.Once
	writeMu sync.Mutex
	mu      sync.Mutex
	pending map[string]chan relayControlMessage
}

type remoteRelayRoom struct {
	hub       *remoteRelayHub
	id        string
	mu        sync.Mutex
	ports     map[int]uint16
	endpoints map[int]netip.AddrPort
	dead      bool
}

func newRemoteRelayHub(ip netip.Addr, tokensFile, certFile, keyFile string) (*remoteRelayHub, error) {
	certificate, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, err
	}
	name, digest, err := loadRelayCredential(tokensFile, ip)
	if err != nil {
		return nil, err
	}
	return &remoteRelayHub{ip: ip, name: name, digest: digest, tls: &tls.Config{
		Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12,
	}, log: slog.Default(), prefix: token(8)}, nil
}

func loadRelayCredential(path string, ip netip.Addr) (string, [sha256.Size]byte, error) {
	var digest [sha256.Size]byte
	data, err := os.ReadFile(path)
	if err != nil {
		return "", digest, err
	}
	var found string
	for number, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Fields(line)
		registered, parseErr := netip.ParseAddr(field(parts, 2))
		raw, hexErr := hex.DecodeString(strings.TrimPrefix(field(parts, 1), "sha256:"))
		if len(parts) != 3 || !strings.HasPrefix(parts[1], "sha256:") || parseErr != nil ||
			!registered.Is4() || hexErr != nil || len(raw) != sha256.Size || len(parts[0]) > 32 {
			return "", digest, fmt.Errorf("%s:%d: invalid relay credential", path, number+1)
		}
		if registered != ip {
			continue
		}
		if found != "" {
			return "", digest, fmt.Errorf("multiple relay credentials use %s", ip)
		}
		found = parts[0]
		copy(digest[:], raw)
	}
	if found == "" {
		return "", digest, fmt.Errorf("no relay credential is registered for %s", ip)
	}
	return found, digest, nil
}

func field(fields []string, index int) string {
	if index < len(fields) {
		return fields[index]
	}
	return ""
}

func (h *remoteRelayHub) available() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.link != nil
}

func (h *remoteRelayHub) room(label string) relaySession {
	id := h.prefix + "-" + label + "-" + strconv.FormatUint(h.next.Add(1), 36)
	return &remoteRelayRoom{hub: h, id: id, ports: make(map[int]uint16), endpoints: make(map[int]netip.AddrPort)}
}

func (h *remoteRelayHub) serve(ctx context.Context, raw net.Listener) error {
	ln := tls.NewListener(raw, h.tls)
	var activeMu sync.Mutex
	var handlers sync.WaitGroup
	active := make(map[net.Conn]struct{})
	closeActive := func() {
		activeMu.Lock()
		defer activeMu.Unlock()
		for conn := range active {
			_ = conn.Close()
		}
	}
	stop := context.AfterFunc(ctx, func() {
		_ = ln.Close()
		closeActive()
	})
	defer func() {
		stop()
		_ = ln.Close()
		closeActive()
		handlers.Wait()
	}()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		activeMu.Lock()
		if ctx.Err() != nil {
			activeMu.Unlock()
			_ = conn.Close()
			continue
		}
		active[conn] = struct{}{}
		handlers.Add(1)
		activeMu.Unlock()
		go func() {
			defer handlers.Done()
			defer func() {
				activeMu.Lock()
				delete(active, conn)
				activeMu.Unlock()
			}()
			h.accept(ctx, conn)
		}()
	}
}

func (h *remoteRelayHub) accept(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	if secure, ok := conn.(*tls.Conn); ok {
		if tcp, ok := secure.NetConn().(*net.TCPConn); ok {
			_ = tcp.SetKeepAliveConfig(net.KeepAliveConfig{
				Enable: true, Idle: 30 * time.Second, Interval: 15 * time.Second, Count: 4,
			})
		}
	}
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	reader := bufio.NewReaderSize(conn, relayControlLimit)
	line, err := readRelayLine(reader)
	if err != nil {
		return
	}
	var hello relayControlMessage
	if json.Unmarshal(line, &hello) != nil {
		return
	}
	presented := sha256.Sum256([]byte(hello.Token))
	if hello.Op != "hello" || hello.Name != h.name ||
		hello.PublicIP != h.ip.String() || subtle.ConstantTimeCompare(presented[:], h.digest[:]) != 1 {
		return
	}
	peer, parseErr := netip.ParseAddrPort(conn.RemoteAddr().String())
	if parseErr != nil || peer.Addr().Unmap() != h.ip {
		return
	}
	_ = conn.SetDeadline(time.Time{})
	link := &relayControlLink{conn: conn, done: make(chan struct{}), pending: make(map[string]chan relayControlMessage)}
	h.mu.Lock()
	old := h.link
	h.link = link
	h.mu.Unlock()
	if old != nil {
		old.close()
	}
	if err := link.send(relayControlMessage{Op: "ready"}); err != nil {
		return
	}
	h.log.Info("TSFP relay connected", "name", short(hello.Name, 32), "ip", h.ip)
	defer func() {
		link.close()
		h.mu.Lock()
		if h.link == link {
			h.link = nil
		}
		h.mu.Unlock()
		h.log.Info("TSFP relay disconnected", "ip", h.ip)
	}()
	for {
		line, err := readRelayLine(reader)
		if err != nil {
			return
		}
		var message relayControlMessage
		if json.Unmarshal(line, &message) != nil {
			return
		}
		if message.Op == "closed" {
			h.log.Info("TSFP relay room closed", "room", short(message.Room, 64))
			if message.ID == "" {
				continue
			}
		}
		link.mu.Lock()
		response := link.pending[message.ID]
		link.mu.Unlock()
		if response != nil {
			select {
			case response <- message:
			default:
			}
		}
	}
}

func readRelayLine(reader *bufio.Reader) ([]byte, error) {
	line, err := reader.ReadSlice('\n')
	if err != nil {
		if errors.Is(err, bufio.ErrBufferFull) {
			return nil, errors.New("relay control message is too large")
		}
		return nil, err
	}
	if len(line) > relayControlLimit {
		return nil, errors.New("relay control message is too large")
	}
	return line, nil
}

func (l *relayControlLink) close() {
	l.once.Do(func() {
		close(l.done)
		l.conn.Close()
	})
}

func (l *relayControlLink) send(message relayControlMessage) error {
	data, err := json.Marshal(message)
	if err != nil || len(data)+1 > relayControlLimit {
		return errors.New("invalid relay control message")
	}
	l.writeMu.Lock()
	defer l.writeMu.Unlock()
	_ = l.conn.SetWriteDeadline(time.Now().Add(3 * time.Second))
	_, err = l.conn.Write(append(data, '\n'))
	return err
}

func (h *remoteRelayHub) request(message relayControlMessage, timeout time.Duration) (relayControlMessage, error) {
	h.mu.Lock()
	link := h.link
	h.mu.Unlock()
	return h.requestLink(link, message, timeout)
}

func (h *remoteRelayHub) requestLink(link *relayControlLink, message relayControlMessage, timeout time.Duration) (relayControlMessage, error) {
	if link == nil {
		return relayControlMessage{}, errors.New("relay is offline")
	}
	message.ID = strconv.FormatUint(h.next.Add(1), 10)
	response := make(chan relayControlMessage, 1)
	link.mu.Lock()
	link.pending[message.ID] = response
	link.mu.Unlock()
	defer func() {
		link.mu.Lock()
		delete(link.pending, message.ID)
		link.mu.Unlock()
	}()
	if err := link.send(message); err != nil {
		link.close()
		return relayControlMessage{}, err
	}
	select {
	case reply := <-response:
		if reply.Op == "failed" {
			return reply, errors.New(short(reply.Error, 200))
		}
		return reply, nil
	case <-link.done:
		return relayControlMessage{}, errors.New("relay disconnected")
	case <-time.After(timeout):
		return relayControlMessage{}, errors.New("relay request timed out")
	}
}

func (h *remoteRelayHub) probe(target netip.AddrPort) (bool, *relayControlLink, error) {
	h.mu.Lock()
	link := h.link
	h.mu.Unlock()
	reply, err := h.requestLink(link,
		relayControlMessage{Op: "probe", Target: []any{target.Addr().String(), target.Port()}}, 4*time.Second)
	if err != nil {
		return false, link, err
	}
	if reply.Op != "probed" {
		return false, link, errors.New("relay returned an invalid probe response")
	}
	return reply.Reachable, link, nil
}

func (h *remoteRelayHub) current(link *relayControlLink) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.link == link
}

func (r *remoteRelayRoom) add(pid int, endpoint netip.AddrPort) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.dead {
		return errors.New("relay room is closed")
	}
	reply, err := r.hub.request(relayControlMessage{Op: "add", Room: r.id, Members: map[string][]any{
		strconv.Itoa(pid): {endpoint.Addr().String(), endpoint.Port()},
	}}, 3*time.Second)
	if err != nil {
		return fmt.Errorf("add remote relay member: %w", err)
	}
	if reply.Op != "added" {
		return errors.New("relay rejected member")
	}
	ports := make(map[int]uint16, len(r.endpoints)+1)
	for key, port := range reply.Ports {
		id, parseErr := strconv.Atoi(key)
		if parseErr == nil && port > 0 && port <= 65535 {
			ports[id] = uint16(port)
		}
	}
	for id := range r.endpoints {
		if ports[id] == 0 {
			r.dead = true
			_, _ = r.hub.request(relayControlMessage{Op: "close", Room: r.id}, 3*time.Second)
			return errRelayRoomLost
		}
	}
	if ports[pid] == 0 {
		return errors.New("relay did not assign a member port")
	}
	r.endpoints[pid] = endpoint
	r.ports = ports
	return nil
}

func (r *remoteRelayRoom) address(pid int) netip.AddrPort {
	r.mu.Lock()
	defer r.mu.Unlock()
	return netip.AddrPortFrom(r.hub.ip, r.ports[pid])
}

func (r *remoteRelayRoom) remove(pid int) {
	r.mu.Lock()
	if r.dead || r.ports[pid] == 0 {
		r.mu.Unlock()
		return
	}
	delete(r.ports, pid)
	delete(r.endpoints, pid)
	r.mu.Unlock()
	_ = r.hub.send(relayControlMessage{Op: "remove", Room: r.id, Members: map[string][]any{strconv.Itoa(pid): {}}})
}

func (r *remoteRelayRoom) close() {
	r.mu.Lock()
	if r.dead {
		r.mu.Unlock()
		return
	}
	r.dead = true
	r.mu.Unlock()
	_, _ = r.hub.request(relayControlMessage{Op: "close", Room: r.id}, 3*time.Second)
}

func (h *remoteRelayHub) send(message relayControlMessage) error {
	h.mu.Lock()
	link := h.link
	h.mu.Unlock()
	if link == nil {
		return errors.New("relay is offline")
	}
	return link.send(message)
}
