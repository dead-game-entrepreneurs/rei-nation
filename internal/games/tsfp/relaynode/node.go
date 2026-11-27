// Package relaynode runs the remote TSFP UDP relay controlled by Theater.
package relaynode

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const maxDatagram = 2048
const maxRooms = 64
const maxMembers = 16
const idleTime = 3 * time.Minute
const bytesPerSecond = 512 << 10
const burstBytes = 1 << 20

type Config struct {
	Server, CAFile, TokenFile, Name string
	PublicIP, Bind                  netip.Addr
	FirstPort, LastPort             uint16
	AllowPrivate                    bool
	Log                             *slog.Logger
}

type Node struct {
	mu        sync.Mutex
	bind      netip.Addr
	first     uint16
	last      uint16
	rooms     map[string]*room
	usedPorts map[uint16]*member
	private   bool
	log       *slog.Logger
}

type room struct {
	node                    *Node
	id                      string
	members                 map[string]*member
	last                    time.Time
	forwarded, dropped, bad uint64
	closed                  bool
}

type member struct {
	endpoint netip.AddrPort
	socket   *net.UDPConn
	budget   float64
	refilled time.Time
	closed   bool
}

type message struct {
	Op        string         `json:"op"`
	ID        string         `json:"id,omitempty"`
	Room      string         `json:"room,omitempty"`
	Token     string         `json:"token,omitempty"`
	PublicIP  string         `json:"public_ip,omitempty"`
	Name      string         `json:"name,omitempty"`
	Members   members        `json:"members,omitempty"`
	Ports     map[string]int `json:"ports,omitempty"`
	Target    []any          `json:"target,omitempty"`
	Reachable bool           `json:"reachable,omitempty"`
	RTT       *float64       `json:"rtt_ms,omitempty"`
	Error     string         `json:"error,omitempty"`
	Forwarded uint64         `json:"forwarded,omitempty"`
	Dropped   uint64         `json:"dropped,omitempty"`
	Rejected  uint64         `json:"rejected,omitempty"`
	Idle      float64        `json:"idle_s,omitempty"`
}

type members map[string][]any

func (m *members) UnmarshalJSON(data []byte) error {
	var values map[string][]any
	if json.Unmarshal(data, &values) == nil {
		*m = values
		return nil
	}
	var keys []string
	if err := json.Unmarshal(data, &keys); err != nil {
		return err
	}
	values = make(map[string][]any, len(keys))
	for _, key := range keys {
		values[key] = nil
	}
	*m = values
	return nil
}

func New(config Config) (*Node, error) {
	if !config.Bind.IsValid() || !config.Bind.Is4() || config.FirstPort == 0 || config.LastPort < config.FirstPort {
		return nil, errors.New("invalid relay bind address or port range")
	}
	if config.Log == nil {
		config.Log = slog.Default()
	}
	return &Node{bind: config.Bind, first: config.FirstPort, last: config.LastPort,
		rooms: make(map[string]*room), usedPorts: make(map[uint16]*member), private: config.AllowPrivate, log: config.Log}, nil
}

func Run(ctx context.Context, config Config) error {
	if config.Log == nil {
		config.Log = slog.Default()
	}
	node, err := New(config)
	if err != nil {
		return err
	}
	tokenData, err := os.ReadFile(config.TokenFile)
	if err != nil {
		return err
	}
	token := strings.TrimSpace(string(tokenData))
	if token == "" || len(token) > 256 {
		return errors.New("invalid relay token")
	}
	tlsConfig, err := pinnedTLS(config.CAFile)
	if err != nil {
		return err
	}
	go node.sweep(ctx)
	delay := time.Second
	for ctx.Err() == nil {
		started := time.Now()
		err = node.control(ctx, config, tlsConfig, token)
		if ctx.Err() != nil {
			break
		}
		if time.Since(started) >= time.Minute {
			delay = time.Second
		}
		config.Log.Warn("relay control disconnected", "error", err, "retry", delay)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
		delay = min(delay*2, time.Minute)
	}
	node.closeAll()
	return nil
}

func pinnedTLS(path string) (*tls.Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, errors.New("relay CA is not a PEM certificate")
	}
	want := sha256.Sum256(block.Bytes)
	return &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: true,
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			if len(raw) != 0 && sha256.Sum256(raw[0]) == want {
				return nil
			}
			return errors.New("relay control certificate does not match pin")
		}}, nil
}

func (n *Node) control(ctx context.Context, config Config, tlsConfig *tls.Config, token string) error {
	dialer := &tls.Dialer{Config: tlsConfig, NetDialer: &net.Dialer{
		LocalAddr: net.TCPAddrFromAddrPort(netip.AddrPortFrom(config.PublicIP, 0)), KeepAliveConfig: net.KeepAliveConfig{
			Enable: true, Idle: 30 * time.Second, Interval: 15 * time.Second, Count: 4,
		},
	}}
	attempt, cancel := context.WithTimeout(ctx, 10*time.Second)
	conn, err := dialer.DialContext(attempt, "tcp", config.Server)
	cancel()
	if err != nil {
		return err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	writeMu := &sync.Mutex{}
	write := func(out message) error {
		data, err := json.Marshal(out)
		if err != nil || len(data) > 4095 {
			return errors.New("invalid control response")
		}
		writeMu.Lock()
		defer writeMu.Unlock()
		_ = conn.SetWriteDeadline(time.Now().Add(3 * time.Second))
		_, err = conn.Write(append(data, '\n'))
		return err
	}
	if err := write(message{Op: "hello", Token: token, Name: config.Name, PublicIP: config.PublicIP.String()}); err != nil {
		return err
	}
	reader := bufio.NewReaderSize(conn, 4096)
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	line, err := reader.ReadSlice('\n')
	if err != nil {
		return fmt.Errorf("relay hello was not accepted: %w", err)
	}
	var ready message
	if json.Unmarshal(line, &ready) != nil || ready.Op != "ready" {
		return errors.New("relay hello was not accepted")
	}
	_ = conn.SetReadDeadline(time.Time{})
	config.Log.Info("relay control connected", "server", config.Server, "rooms", n.roomCount())
	for {
		line, err := reader.ReadSlice('\n')
		if err != nil {
			if errors.Is(err, bufio.ErrBufferFull) {
				return errors.New("control request too large")
			}
			return err
		}
		if len(line) > 4096 {
			return errors.New("control request too large")
		}
		var request message
		if json.Unmarshal(line, &request) != nil {
			return errors.New("invalid control request")
		}
		if request.Op == "probe" {
			go func() { _ = write(n.handle(request)) }()
			continue
		}
		if reply := n.handle(request); reply.Op != "" {
			if err := write(reply); err != nil {
				return err
			}
		}
	}
}

func (n *Node) handle(request message) message {
	switch request.Op {
	case "add":
		ports, err := n.add(request.Room, request.Members)
		if err != nil {
			return message{Op: "failed", ID: request.ID, Error: err.Error()}
		}
		return message{Op: "added", ID: request.ID, Ports: ports}
	case "remove":
		n.remove(request.Room, request.Members)
	case "close":
		reply := n.close(request.Room)
		reply.ID = request.ID
		return reply
	case "probe":
		target, err := parseTarget(request.Target, n.private)
		if err != nil {
			return message{Op: "failed", ID: request.ID, Error: err.Error()}
		}
		started := time.Now()
		ok := n.probe(target)
		rtt := float64(time.Since(started).Microseconds()) / 1000
		if !ok {
			rtt = 0
		}
		return message{Op: "probed", ID: request.ID, Reachable: ok, RTT: &rtt}
	case "ping":
		return message{Op: "pong"}
	}
	return message{}
}

func parseTarget(raw []any, allowPrivate bool) (netip.AddrPort, error) {
	if len(raw) != 2 {
		return netip.AddrPort{}, errors.New("invalid target")
	}
	ip, err := netip.ParseAddr(fmt.Sprint(raw[0]))
	port, portErr := strconv.ParseUint(fmt.Sprint(raw[1]), 10, 16)
	if err != nil || portErr != nil || port == 0 || !allowedIPv4(ip, allowPrivate) {
		return netip.AddrPort{}, errors.New("invalid public target")
	}
	return netip.AddrPortFrom(ip, uint16(port)), nil
}

func allowedIPv4(ip netip.Addr, allowPrivate bool) bool {
	if !ip.Is4() || ip.IsUnspecified() || ip.IsMulticast() {
		return false
	}
	if allowPrivate {
		return true
	}
	for _, prefix := range nonPublicIPv4 {
		if prefix.Contains(ip) {
			return false
		}
	}
	return ip.IsGlobalUnicast()
}

var nonPublicIPv4 = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"), netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"), netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.168.0.0/16"), netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"), netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
}

func parseMembers(raw members, allowPrivate bool) (map[string]netip.AddrPort, error) {
	if len(raw) == 0 || len(raw) > maxMembers {
		return nil, errors.New("invalid relay members")
	}
	result := make(map[string]netip.AddrPort, len(raw))
	for key, value := range raw {
		target, err := parseTarget(value, allowPrivate)
		if err != nil || len(key) == 0 || len(key) > 16 {
			return nil, errors.New("invalid relay member")
		}
		result[key] = target
	}
	return result, nil
}

func (n *Node) add(id string, raw members) (map[string]int, error) {
	members, err := parseMembers(raw, n.private)
	if err != nil || id == "" || len(id) > 80 {
		return nil, errors.New("invalid relay room")
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	r := n.rooms[id]
	created := r == nil
	if r == nil {
		if len(n.rooms) >= maxRooms {
			return nil, errors.New("relay is full")
		}
		r = &room{node: n, id: id, members: make(map[string]*member), last: time.Now()}
		n.rooms[id] = r
	}
	newMembers := 0
	for key := range members {
		if r.members[key] == nil {
			newMembers++
		}
	}
	if len(r.members)+newMembers > maxMembers {
		return nil, errors.New("relay room is full")
	}
	var addedKeys []string
	updates := make(map[*member]netip.AddrPort)
	for key, endpoint := range members {
		if existing := r.members[key]; existing != nil {
			updates[existing] = endpoint
			continue
		}
		socket, port, err := n.openSocket()
		if err != nil {
			for _, addedKey := range addedKeys {
				added := r.members[addedKey]
				delete(r.members, addedKey)
				port := added.socket.LocalAddr().(*net.UDPAddr).AddrPort().Port()
				delete(n.usedPorts, port)
				added.closed = true
				added.socket.Close()
			}
			if created {
				delete(n.rooms, id)
			}
			return nil, err
		}
		m := &member{endpoint: endpoint, socket: socket,
			budget: burstBytes, refilled: time.Now()}
		r.members[key] = m
		addedKeys = append(addedKeys, key)
		n.usedPorts[port] = m
		go n.receive(r, m, port)
	}
	for existing, endpoint := range updates {
		existing.endpoint = endpoint
	}
	r.last = time.Now()
	ports := make(map[string]int, len(r.members))
	for key, m := range r.members {
		ports[key] = int(m.socket.LocalAddr().(*net.UDPAddr).AddrPort().Port())
	}
	n.log.Info("relay room updated", "room", id, "members", len(r.members))
	return ports, nil
}

func (n *Node) openSocket() (*net.UDPConn, uint16, error) {
	width := int(n.last-n.first) + 1
	start := rand.IntN(width)
	for offset := range width {
		port := n.first + uint16((start+offset)%width)
		if n.usedPorts[port] != nil {
			continue
		}
		socket, err := net.ListenUDP("udp4", net.UDPAddrFromAddrPort(netip.AddrPortFrom(n.bind, port)))
		if err == nil {
			return socket, port, nil
		}
	}
	return nil, 0, errors.New("no relay port is available")
}

func (n *Node) receive(r *room, target *member, port uint16) {
	defer func() {
		n.mu.Lock()
		if n.usedPorts[port] == target {
			delete(n.usedPorts, port)
		}
		n.mu.Unlock()
	}()
	var data [maxDatagram + 1]byte
	for {
		size, from, err := target.socket.ReadFromUDPAddrPort(data[:])
		if err != nil {
			return
		}
		if size > 0 && size <= maxDatagram {
			n.forward(r, target, from, data[:size])
		}
	}
}

func (n *Node) forward(r *room, target *member, from netip.AddrPort, data []byte) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if r.closed || target.closed {
		return
	}
	var sender *member
	for _, candidate := range r.members {
		if candidate != target && candidate.endpoint == from {
			if sender != nil {
				r.bad++
				return
			}
			sender = candidate
		}
	}
	if sender == nil {
		r.bad++
		return
	}
	now := time.Now()
	sender.budget = min(burstBytes, sender.budget+now.Sub(sender.refilled).Seconds()*bytesPerSecond)
	sender.refilled = now
	if sender.budget < float64(len(data)) {
		r.dropped++
		return
	}
	sender.budget -= float64(len(data))
	r.last = now
	_, _ = sender.socket.WriteToUDPAddrPort(data, target.endpoint)
	r.forwarded++
}

func (n *Node) remove(id string, raw members) {
	n.mu.Lock()
	defer n.mu.Unlock()
	r := n.rooms[id]
	if r == nil {
		return
	}
	for key := range raw {
		if m := r.members[key]; m != nil {
			delete(r.members, key)
			m.closed = true
			m.socket.Close()
		}
	}
}

func (n *Node) close(id string) message {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.closeLocked(id)
}

func (n *Node) closeLocked(id string) message {
	r := n.rooms[id]
	if r == nil {
		return message{Op: "closed", Room: id}
	}
	delete(n.rooms, id)
	r.closed = true
	for _, m := range r.members {
		m.closed = true
		m.socket.Close()
	}
	return message{Op: "closed", Room: id, Forwarded: r.forwarded, Dropped: r.dropped,
		Rejected: r.bad, Idle: time.Since(r.last).Seconds()}
}

func (n *Node) closeAll() {
	n.mu.Lock()
	ids := make([]string, 0, len(n.rooms))
	for id := range n.rooms {
		ids = append(ids, id)
	}
	n.mu.Unlock()
	for _, id := range ids {
		n.close(id)
	}
}

func (n *Node) roomCount() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.rooms)
}

func (n *Node) sweep(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			n.mu.Lock()
			for id, r := range n.rooms {
				if now.Sub(r.last) >= idleTime {
					n.closeLocked(id)
				}
			}
			n.mu.Unlock()
		}
	}
}

func (n *Node) probe(target netip.AddrPort) bool {
	n.mu.Lock()
	socket, _, err := n.openSocket()
	n.mu.Unlock()
	if err != nil {
		return false
	}
	defer socket.Close()
	tid := strconv.FormatUint(rand.Uint64N(0x7ffffffe)+1, 10)
	body := []byte("TID=" + tid + "\nTYPE=1\n\x00")
	wire := make([]byte, 14+len(body))
	wire[0], wire[1] = 0xff, 0xff
	copy(wire[2:6], "PROB")
	binary.BigEndian.PutUint32(wire[6:10], 0xc0000000)
	binary.BigEndian.PutUint32(wire[10:14], uint32(12+len(body)))
	copy(wire[14:], body)
	var response [128]byte
	for range 4 {
		if _, err := socket.WriteToUDPAddrPort(wire, target); err != nil {
			return false
		}
		_ = socket.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		for {
			size, from, err := socket.ReadFromUDPAddrPort(response[:])
			if err != nil {
				break
			}
			data := bytes.TrimPrefix(response[:size], []byte{0xff, 0xff})
			if from.Addr() == target.Addr() && len(data) >= 12 && string(data[:4]) == "PROB" &&
				binary.BigEndian.Uint32(data[4:8]) == 0 && int(binary.BigEndian.Uint32(data[8:12])) == len(data) &&
				strings.Contains(string(data[12:]), "TID="+tid+"\n") {
				return true
			}
		}
	}
	return false
}
