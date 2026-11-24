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
}

type Runtime interface {
	aries.Handler
	Services() []func(context.Context) error
}

type Options struct {
	PublicIP         netip.Addr
	TCPPort          uint16
	Store            storage.IdentityStore
	Log              *slog.Logger
	AuxTCP           map[uint16]net.Listener
	UDP              map[uint16]*net.UDPConn
	Relay            string
	SendRecoveryMail func(ctx context.Context, recipient, subject, body string) error
}
