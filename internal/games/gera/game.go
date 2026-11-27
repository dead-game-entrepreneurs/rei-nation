// Package gera implements EA FESL for GoldenEye: Rogue Agent (US PS2, SLUS-21064).
// See docs/games/gera/online-map.md for the client evidence.
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
	"github.com/dead-game-entrepreneurs/rei-nation/internal/games/gera/relay"
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
	// The Hello reply hands the client these theater and messenger ports.
	theaterPort   = 18041
	messengerPort = 18042
	// Signed-in connections have no read deadline because the client goes silent
	// while loading a level. See docs/games/gera/online-map.md#connection-lifetime.
	idleTimeout   = noIdle
	messengerIdle = noIdle
	theaterIdle   = noIdle
	// noIdle disables the aries read deadline (a negative duration).
	noIdle = -1 * time.Second
	// Connections that have not signed in are closed after these limits. FESL
	// allows time for account registration on the console keyboard.
	feslAuthTimeout = 10 * time.Minute
	auxAuthTimeout  = time.Minute
)

// Definition reserves the relay control port only when relay control is configured.
func Definition(relayControl bool) game.Definition {
	aux := []uint16{messengerPort, theaterPort}
	if relayControl {
		aux = append(aux, relayControlPort)
	}
	return game.Definition{Key: gameKey, TCPPort: feslPort, AuxTCP: aux,
		UDP: []uint16{theaterPort},
		New: func(opts game.Options) game.Runtime {
			s := &server{opts: opts, sessions: make(map[*aries.Client]*account), keys: make(map[string]keyEntry),
				clientKeys: make(map[*aries.Client][]string)}
			s.store, _ = opts.Store.(storage.IdentityStore)
			s.stats, _ = opts.Store.(storage.StatsStore)
			s.social, _ = opts.Store.(socialStore)
			s.relayPolicy = newRelayPolicy(opts.RelayForce)
			if opts.RelayDir != "" {
				hub, err := relay.LoadHub(opts.RelayDir, opts.Log.With("service", "relay"))
				if err != nil {
					opts.Log.Error("relay hub not started", "dir", opts.RelayDir, "error", err)
				} else {
					s.relays = hub
				}
			}
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
		Serve: func(ctx context.Context, ln net.Listener, h game.Runtime) error {
			s := h.(*server)
			if s.certErr != nil {
				return s.certErr
			}
			// FESL has no server heartbeat command; fsys Ping is a transaction.
			return aries.ServeWithOptions(ctx, ssl2.NewListener(ln, s.cert, s.key, s.opts.Log), s,
				aries.ServeOptions{IdleTimeout: idleTimeout, AuthTimeout: feslAuthTimeout})
		}}
}

type server struct {
	opts      game.Options
	cert      []byte
	key       *rsa.PrivateKey
	certErr   error
	store     storage.IdentityStore
	stats     storage.StatsStore
	social    socialStore
	messenger *messenger
	theater   *theater
	// relays is nil when no relay directory is configured.
	relays      *relay.Hub
	relayPolicy *relayPolicy

	mu       sync.Mutex
	sessions map[*aries.Client]*account

	keysMu     sync.Mutex
	keys       map[string]keyEntry
	clientKeys map[*aries.Client][]string
}

// keyEntry is an issued session key. A zero expiry means the FESL session is live.
type keyEntry struct {
	id      identity
	expires time.Time
}

// keyGrace keeps a key usable after its FESL connection drops so Messenger and
// Theater can reconnect during a brief FESL reconnect. Logout revokes at once.
const keyGrace = 10 * time.Minute

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
	s.keys[key] = keyEntry{id: id}
	s.keysMu.Unlock()
	return key
}

// issueSessionKey issues a key owned by c, revoked when c logs out or disconnects.
func (s *server) issueSessionKey(c *aries.Client, id identity) string {
	key := s.issueKey(id)
	s.keysMu.Lock()
	s.clientKeys[c] = append(s.clientKeys[c], key)
	s.keysMu.Unlock()
	return key
}

// revokeKeys removes c's keys now (grace 0) or after grace, and prunes expired keys.
func (s *server) revokeKeys(c *aries.Client, grace time.Duration) {
	now := time.Now()
	s.keysMu.Lock()
	defer s.keysMu.Unlock()
	for _, key := range s.clientKeys[c] {
		if grace <= 0 {
			delete(s.keys, key)
		} else if e, ok := s.keys[key]; ok {
			e.expires = now.Add(grace)
			s.keys[key] = e
		}
	}
	delete(s.clientKeys, c)
	for key, e := range s.keys {
		if !e.expires.IsZero() && now.After(e.expires) {
			delete(s.keys, key)
		}
	}
}

func (s *server) lookupKey(key string) (identity, bool) {
	s.keysMu.Lock()
	defer s.keysMu.Unlock()
	e, ok := s.keys[key]
	if !ok || (!e.expires.IsZero() && time.Now().After(e.expires)) {
		return identity{}, false
	}
	return e.id, true
}

func (s *server) Services() []func(context.Context) error {
	var services []func(context.Context) error
	// Both aux services are plaintext; only the FESL login connection uses SSL.
	if ln := s.opts.AuxTCP[messengerPort]; ln != nil {
		services = append(services, func(ctx context.Context) error {
			return aries.ServeWithOptions(ctx, ln, s.messenger, aries.ServeOptions{IdleTimeout: messengerIdle, AuthTimeout: auxAuthTimeout})
		})
	}
	if ln := s.opts.AuxTCP[theaterPort]; ln != nil {
		services = append(services, func(ctx context.Context) error {
			return aries.ServeWithOptions(ctx, ln, s.theater,
				aries.ServeOptions{Heartbeat: "PING", IdleTimeout: theaterIdle, AuthTimeout: auxAuthTimeout})
		})
	}
	if ln := s.opts.AuxTCP[relayControlPort]; ln != nil {
		if s.relays != nil {
			services = append(services, func(ctx context.Context) error { return s.relays.Serve(ctx, ln) })
		} else {
			ln.Close()
		}
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
	s.revokeKeys(c, keyGrace)
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
		s.handleRank(ctx, c, m)
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
	ip := s.advertisedIP(c).String()
	c.Send(fesl.Reply(m, "Hello",
		fesl.Field{Key: "theaterIp", Value: ip},
		fesl.Field{Key: "theaterPort", Value: strconv.Itoa(theaterPort)},
		fesl.Field{Key: "messengerIp", Value: ip},
		fesl.Field{Key: "messengerPort", Value: strconv.Itoa(messengerPort)},
	))
}

// advertisedIP is the theater and messenger address for c: its own LAN address
// when it is on the server's network, so no router hairpin is needed.
func (s *server) advertisedIP(c *aries.Client) netip.Addr {
	if lanAddr(c.Addr()) && !lanAddr(s.opts.PublicIP) {
		if local := c.LocalAddr(); local.IsValid() && !local.IsUnspecified() {
			return local
		}
	}
	return s.opts.PublicIP
}

// lanAddr reports an address that only exists on a local network.
func lanAddr(a netip.Addr) bool {
	return a.IsPrivate() || a.IsLoopback() || a.IsLinkLocalUnicast()
}
