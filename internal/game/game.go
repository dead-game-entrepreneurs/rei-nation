// Package game defines the contract between a title and the server process.
package game

import (
	"context"
	"log/slog"
	"net"
	"net/netip"

	"github.com/dead-game-entrepreneurs/rei-nation/internal/protocol/aries"
	"github.com/dead-game-entrepreneurs/rei-nation/internal/storage"
)

type Definition struct {
	Key     string
	TCPPort uint16
	AuxTCP  []uint16
	UDP     []uint16
	New     func(Options) Runtime
	// Serve runs the main TCP listener. Nil uses aries.Serve. A title sets it
	// when its clients need SSL or a different keepalive or idle limit.
	Serve    func(context.Context, net.Listener, Runtime) error
	Validate func(netip.Addr) error
}

type Runtime interface {
	aries.Handler
	Services() []func(context.Context) error
}

type Options struct {
	PublicIP netip.Addr
	TCPPort  uint16
	Store    storage.IdentityStore
	Log      *slog.Logger
	AuxTCP   map[uint16]net.Listener
	UDP      map[uint16]*net.UDPConn
	Relay    string
	// RelayDir and RelayForce configure the GoldenEye relay hub: the directory
	// with its control certificate and node tokens, and personas (names or
	// IDs, "*" for all) whose joins always go through a relay.
	RelayDir         string
	RelayForce       []string
	SendRecoveryMail func(ctx context.Context, recipient, subject, body string) error
}
