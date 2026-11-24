// Package app validates configuration and owns the process listeners.
package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"time"

	"github.com/dead-game-entrepreneurs/rei-nation/internal/game"
	"github.com/dead-game-entrepreneurs/rei-nation/internal/protocol/aries"
	"github.com/dead-game-entrepreneurs/rei-nation/internal/storage"
)

type Config struct {
	Games            string
	Bind             netip.Addr
	PublicIP         netip.Addr
	HealthPort       uint16
	Relay            string
	SendRecoveryMail func(ctx context.Context, recipient, subject, body string) error
}

func (c Config) Select(registry []game.Definition) ([]game.Definition, error) {
	if !c.Bind.Is4() || !c.PublicIP.Is4() || c.PublicIP.IsUnspecified() ||
		c.PublicIP.IsMulticast() || c.PublicIP == netip.MustParseAddr("255.255.255.255") || c.HealthPort == 0 {
		return nil, errors.New("bind and advertised addresses must be IPv4, public IP must be reachable, and health port must be set")
	}
	definitions := make(map[string]game.Definition, len(registry))
	for _, d := range registry {
		if _, exists := definitions[d.Key]; exists {
			return nil, fmt.Errorf("duplicate title registration %q", d.Key)
		}
		definitions[d.Key] = d
	}
	used := map[uint16]string{c.HealthPort: "health"}
	usedUDP := make(map[uint16]string)
	if c.Relay != "" && c.Relay != "auto" && c.Relay != "always" && c.Relay != "off" {
		return nil, errors.New("REI_NATION_RELAY must be auto, always, or off")
	}
	var selected []game.Definition
	for key := range strings.SplitSeq(c.Games, ",") {
		key = strings.TrimSpace(key)
		d, ok := definitions[key]
		if !ok || d.TCPPort == 0 || d.New == nil {
			return nil, fmt.Errorf("unknown or invalid title %q", key)
		}
		for _, ep := range []struct {
			protocol string
			ports    []uint16
			used     map[uint16]string
		}{
			{"TCP", append([]uint16{d.TCPPort}, d.AuxTCP...), used},
			{"UDP", d.UDP, usedUDP},
		} {
			for _, port := range ep.ports {
				if prior, ok := ep.used[port]; ok {
					return nil, fmt.Errorf("%s port %d is shared by %s and %s", ep.protocol, port, prior, key)
				}
				if port == 0 {
					return nil, fmt.Errorf("invalid %s port for %s", ep.protocol, key)
				}
				ep.used[port] = key
			}
		}
		selected = append(selected, d)
	}
	return selected, nil
}

type Store interface {
	storage.IdentityStore
	Ping(context.Context) error
}

func Run(ctx context.Context, c Config, selected []game.Definition, store Store, log *slog.Logger) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var listeners []net.Listener
	var packets []*net.UDPConn
	defer func() {
		for _, ln := range listeners {
			ln.Close()
		}
		for _, socket := range packets {
			socket.Close()
		}
	}()
	bind := func(host netip.Addr, port uint16) (net.Listener, error) {
		ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp4", netip.AddrPortFrom(host, port).String())
		if err != nil {
			return nil, err
		}
		listeners = append(listeners, ln)
		return ln, nil
	}
	health, err := bind(netip.MustParseAddr("127.0.0.1"), c.HealthPort)
	if err != nil {
		return fmt.Errorf("bind health: %w", err)
	}
	type title struct {
		ln   net.Listener
		opts game.Options
		def  game.Definition
	}
	var runtimes []title
	for _, d := range selected {
		ln, err := bind(c.Bind, d.TCPPort)
		if err != nil {
			return fmt.Errorf("bind %s: %w", d.Key, err)
		}
		options := game.Options{PublicIP: c.PublicIP, TCPPort: d.TCPPort, Store: store,
			Log: log.With("game", d.Key), Relay: c.Relay, SendRecoveryMail: c.SendRecoveryMail,
			AuxTCP: make(map[uint16]net.Listener), UDP: make(map[uint16]*net.UDPConn)}
		for _, port := range d.AuxTCP {
			if options.AuxTCP[port], err = bind(c.Bind, port); err != nil {
				return fmt.Errorf("bind %s TCP %d: %w", d.Key, port, err)
			}
		}
		for _, port := range d.UDP {
			s, err := net.ListenUDP("udp4", net.UDPAddrFromAddrPort(netip.AddrPortFrom(c.Bind, port)))
			if err != nil {
				return fmt.Errorf("bind %s UDP %d: %w", d.Key, port, err)
			}
			packets = append(packets, s)
			options.UDP[port] = s
		}
		runtimes = append(runtimes, title{ln, options, d})
	}

	titles := make(map[string]int, len(selected))
	for _, d := range selected {
		titles[d.Key] = int(d.TCPPort)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		check, cancel := context.WithTimeout(r.Context(), time.Second)
		defer cancel()
		if store.Ping(check) != nil {
			http.Error(w, "database unavailable", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(struct {
			Games map[string]int `json:"games"`
		}{titles})
	})
	web := &http.Server{Handler: mux, ReadHeaderTimeout: 2 * time.Second}

	services := []func(context.Context) error{func(context.Context) error {
		err := web.Serve(health)
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}}
	for _, t := range runtimes {
		handler := t.def.New(t.opts)
		services = append(services, func(ctx context.Context) error { return aries.Serve(ctx, t.ln, handler) })
		services = append(services, handler.Services()...)
		log.Info("title listeners ready", "game", t.def.Key, "bind", t.ln.Addr(), "public_ip", c.PublicIP)
	}
	results := make(chan error, len(services))
	for _, serve := range services {
		go func() { results <- serve(ctx) }()
	}
	var first error
	remaining := len(services)
	select {
	case <-ctx.Done():
	case first = <-results:
		remaining--
	}
	cancel()
	web.Close()
	for range remaining {
		if err := <-results; first == nil {
			first = err
		}
	}
	return first
}
