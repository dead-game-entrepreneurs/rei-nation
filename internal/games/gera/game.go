// Package gera implements EA FESL for GoldenEye: Rogue Agent on PS2.
//
// The US disc (SLUS-21064) registers its FESL hub as clientString "gera" and
// connects to TCP 18040. See docs/games/gera/online-map.md for the evidence.
package gera

import (
	"context"
	"crypto/rsa"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"time"

	"github.com/dead-game-entrepreneurs/rei-nation/internal/connectivity"
	"github.com/dead-game-entrepreneurs/rei-nation/internal/game"
	"github.com/dead-game-entrepreneurs/rei-nation/internal/protocol/aries"
	"github.com/dead-game-entrepreneurs/rei-nation/internal/protocol/fesl"
	"github.com/dead-game-entrepreneurs/rei-nation/internal/protocol/ssl2"
	"github.com/dead-game-entrepreneurs/rei-nation/internal/storage"
)

const (
	gameKey  = "gera"
	feslPort = 18040
	// sslName is the certificate name the client passes to its SSL module. DNS
	// redirection changes where the name resolves, not the name itself.
	sslName = "fesl.ea.com"
	// The client takes the theater and messenger endpoints from the Hello
	// reply. Rei Nation does not serve either yet; these are the addresses it
	// will use once it does.
	theaterPort   = 18041
	messengerPort = 18042
	// No application read deadline on any of the three connections: the client
	// goes silent for minutes while loading a level and expects its session to
	// survive. aries enables TCP keepalive, which drops a genuinely dead peer.
	idleTimeout   = noIdle
	messengerIdle = noIdle
	theaterIdle   = noIdle
	// noIdle disables the aries read deadline (a negative duration).
	noIdle = -1 * time.Second
)

func Definition() game.Definition {
	return game.Definition{Key: gameKey, TCPPort: feslPort, AuxTCP: []uint16{messengerPort, theaterPort},
		UDP: []uint16{theaterPort},
		New: func(opts game.Options) game.Runtime {
			s := &server{opts: opts, sessions: make(map[*aries.Client]*account), keys: make(map[string]identity)}
			s.store, _ = opts.Store.(storage.IdentityStore)
			s.messenger = &messenger{srv: s, sessions: make(map[*aries.Client]*presence)}
			s.theater = &theater{srv: s, sessions: make(map[*aries.Client]*theaterSession),
				games: make(map[int]*hostedGame), connIP: make(map[*aries.Client]string),
				observed: make(map[*aries.Client]addr)}
			// The client negotiates SSL before any FESL traffic. The certificate
			// is generated once per process and is never written to disk.
			s.cert, s.key, s.certErr = ssl2.GenerateCertificate(sslName)
			if s.certErr != nil {
				opts.Log.Error("FESL certificate generation failed", "error", s.certErr)
			}
			return s
		},
		Serve: func(ctx context.Context, ln net.Listener, h aries.Handler) error {
			s := h.(*server)
			if s.certErr != nil {
				return s.certErr
			}
			// FESL has no server heartbeat command; fsys Ping is a transaction.
			return aries.ServeWithOptions(ctx, ssl2.NewListener(ln, s.cert, s.key, s.opts.Log), s,
				aries.ServeOptions{IdleTimeout: idleTimeout})
		}}
}

type server struct {
	opts      game.Options
	cert      []byte
	key       *rsa.PrivateKey
	certErr   error
	store     storage.IdentityStore
	messenger *messenger
	theater   *theater

	mu       sync.Mutex
	sessions map[*aries.Client]*account

	keysMu sync.Mutex
	keys   map[string]identity
}

// identity is the logged-in player a session key resolves to. The messenger
// authenticates with the key the acct Login or LoginSubAccount issued.
type identity struct {
	accountID   int64
	personaID   int64
	personaName string
}

// issueKey records a session key for id and returns it.
func (s *server) issueKey(id identity) string {
	key := newLkey()
	s.keysMu.Lock()
	s.keys[key] = id
	s.keysMu.Unlock()
	return key
}

func (s *server) lookupKey(key string) (identity, bool) {
	s.keysMu.Lock()
	defer s.keysMu.Unlock()
	id, ok := s.keys[key]
	return id, ok
}

func (s *server) Services() []func(context.Context) error {
	var services []func(context.Context) error
	// Both aux services are plaintext; only the FESL login connection uses SSL.
	if ln := s.opts.AuxTCP[messengerPort]; ln != nil {
		services = append(services, func(ctx context.Context) error {
			return aries.ServeWithOptions(ctx, ln, s.messenger, aries.ServeOptions{IdleTimeout: messengerIdle})
		})
	}
	if ln := s.opts.AuxTCP[theaterPort]; ln != nil {
		services = append(services, func(ctx context.Context) error {
			return aries.ServeWithOptions(ctx, ln, s.theater,
				aries.ServeOptions{Heartbeat: "PING", IdleTimeout: theaterIdle})
		})
	}
	if udp := s.opts.UDP[theaterPort]; udp != nil {
		services = append(services, func(ctx context.Context) error {
			return connectivity.ServeUDP(ctx, udp, func(from netip.AddrPort, data []byte) {
				s.theater.handleEcho(udp, from, data)
			})
		}, func(ctx context.Context) error {
			return s.theater.udpKeepAlive(ctx, udp)
		})
	}
	return services
}

func (s *server) Disconnected(c *aries.Client) {
	s.mu.Lock()
	delete(s.sessions, c)
	s.mu.Unlock()
	s.opts.Log.Info("FESL client disconnected", "conn", c.ID(), "reason", c.CloseReason())
}

// session returns the per-connection account state, creating it on first use.
func (s *server) session(c *aries.Client) *account {
	s.mu.Lock()
	defer s.mu.Unlock()
	a := s.sessions[c]
	if a == nil {
		a = &account{}
		s.sessions[c] = a
	}
	return a
}

func (s *server) Handle(ctx context.Context, c *aries.Client, f aries.Frame) {
	m := fesl.Decode(f)
	switch m.Service {
	case "fsys":
		s.handleFsys(c, m)
	case "acct":
		s.handleAcct(ctx, c, m)
	case "rank":
		s.handleRank(c, m)
	default:
		s.opts.Log.Info("unhandled FESL service", "conn", c.ID(), "service", m.Service, "txn", m.TXN)
	}
}

func (s *server) handleFsys(c *aries.Client, m fesl.Message) {
	switch m.TXN {
	case "Hello":
		s.hello(c, m)
	case "Ping":
		c.Send(fesl.Reply(m, "Ping"))
	case "Goodbye":
		s.opts.Log.Info("FESL client said goodbye", "conn", c.ID())
		c.CloseWithReason("client goodbye")
	default:
		s.opts.Log.Info("unhandled fsys transaction", "conn", c.ID(), "txn", m.TXN)
	}
}

func (s *server) hello(c *aries.Client, m fesl.Message) {
	client, _ := m.Get("clientString")
	version, _ := m.Get("clientVersion")
	protocol, _ := m.Get("protocolVersion")
	s.opts.Log.Info("FESL hello", "conn", c.ID(), "client", client, "client_version", version, "protocol", protocol)
	ip := s.opts.PublicIP.String()
	c.Send(fesl.Reply(m, "Hello",
		fesl.Field{Key: "theaterIp", Value: ip},
		fesl.Field{Key: "theaterPort", Value: strconv.Itoa(theaterPort)},
		fesl.Field{Key: "messengerIp", Value: ip},
		fesl.Field{Key: "messengerPort", Value: strconv.Itoa(messengerPort)},
	))
}
