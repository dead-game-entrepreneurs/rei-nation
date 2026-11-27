// Package tsfp implements the TimeSplitters: Future Perfect PlayStation 2 services.
package tsfp

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/dead-game-entrepreneurs/rei-nation/internal/connectivity"
	"github.com/dead-game-entrepreneurs/rei-nation/internal/game"
	"github.com/dead-game-entrepreneurs/rei-nation/internal/protocol/aries"
	"github.com/dead-game-entrepreneurs/rei-nation/internal/protocol/ssl2"
	"github.com/dead-game-entrepreneurs/rei-nation/internal/storage"
)

const (
	gameKey       = "tsfp"
	feslPort      = 18070
	theaterPort   = 18071
	messengerPort = 18072
)

func Definition(certFile, keyFile, relayAddress string) game.Definition {
	var transport *ssl2.Server
	var relayIP netip.Addr
	return game.Definition{
		Key: gameKey, TCPPort: feslPort, AuxTCP: []uint16{theaterPort, messengerPort}, UDP: []uint16{theaterPort},
		Validate: func(publicIP netip.Addr) error {
			var err error
			transport, err = ssl2.Load(certFile, keyFile)
			if err != nil {
				return fmt.Errorf("load TSFP FESL certificate and key: %w", err)
			}
			if relayAddress == "" {
				return nil
			}
			relayIP, err = netip.ParseAddr(relayAddress)
			if err != nil || !relayIP.Is4() || relayIP.IsUnspecified() || relayIP.IsMulticast() ||
				relayIP == netip.MustParseAddr("255.255.255.255") || relayIP == publicIP {
				return fmt.Errorf("REI_NATION_TSFP_RELAY_IP must be a distinct reachable IPv4 address")
			}
			socket, err := net.ListenUDP("udp4", net.UDPAddrFromAddrPort(netip.AddrPortFrom(relayIP, 0)))
			if err != nil {
				return fmt.Errorf("bind REI_NATION_TSFP_RELAY_IP: %w", err)
			}
			return socket.Close()
		},
		New: func(opts game.Options) game.Runtime {
			keys := &loginKeys{keys: make(map[string]sessionKey)}
			f := &fesl{opts: opts, keys: keys, clients: make(map[*aries.Client]*feslSession)}
			f.theater = newTheater(keys, relayIP, opts.Relay)
			f.theater.udp = opts.UDP[theaterPort]
			f.messenger = newMessenger(keys, opts.Store)
			f.content, _ = opts.Store.(storage.TSFPStore)
			f.ranking, _ = opts.Store.(storage.TSFPRankStore)
			return f
		},
		Serve: func(ctx context.Context, ln net.Listener, h game.Runtime) error {
			return aries.ServeTransport(ctx, ln, h, aries.Transport{
				Upgrade: transport.Accept, Heartbeat: packet("fsys", "TXN", "Ping"),
				HeartbeatInterval: time.Minute, ReadTimeout: 300 * time.Second,
			})
		},
	}
}

func (f *fesl) Services() []func(context.Context) error {
	return []func(context.Context) error{
		func(ctx context.Context) error {
			return aries.ServeTransport(ctx, f.opts.AuxTCP[theaterPort], f.theater, aries.Transport{
				Heartbeat: aries.Frame{Tag: "PING", Terminate: true}, HeartbeatInterval: 30 * time.Second,
				ReadTimeout: 100 * time.Second,
			})
		},
		func(ctx context.Context) error {
			return connectivity.ServeUDP(ctx, f.opts.UDP[theaterPort], f.theater.echo)
		},
		func(ctx context.Context) error {
			ln := f.opts.AuxTCP[messengerPort]
			return aries.ServeTransport(ctx, keepaliveListener{ln}, f.messenger, aries.Transport{})
		},
		func(ctx context.Context) error { return f.theater.sweep(ctx, f.opts.UDP[theaterPort]) },
	}
}

type keepaliveListener struct{ net.Listener }

func (l keepaliveListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if tcp, ok := conn.(*net.TCPConn); ok {
		_ = tcp.SetKeepAliveConfig(net.KeepAliveConfig{
			Enable: true, Idle: 30 * time.Second, Interval: 15 * time.Second, Count: 4,
		})
	}
	return conn, err
}

type sessionKey struct {
	persona storage.Persona
	expires time.Time
}

type loginKeys struct {
	mu   sync.Mutex
	keys map[string]sessionKey
}

func (k *loginKeys) issue(p storage.Persona) string {
	// Account-level keys have no authority to enter Theater or Messenger.
	key := token(24)
	if p.ID == 0 {
		return key
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	now := time.Now()
	for id, entry := range k.keys {
		if !now.Before(entry.expires) {
			delete(k.keys, id)
		}
	}
	k.keys[key] = sessionKey{p, now.Add(24 * time.Hour)}
	return key
}

func (k *loginKeys) lookup(key string) (storage.Persona, bool) {
	if len(key) != 48 {
		return storage.Persona{}, false
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	entry, ok := k.keys[key]
	return entry.persona, ok && time.Now().Before(entry.expires)
}
