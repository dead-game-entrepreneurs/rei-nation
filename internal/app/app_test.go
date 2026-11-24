package app

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"strconv"
	"testing"

	"github.com/dead-game-entrepreneurs/rei-nation/internal/game"
	"github.com/dead-game-entrepreneurs/rei-nation/internal/games/nascar04"
)

func TestSelect(t *testing.T) {
	address := netip.MustParseAddr("127.0.0.1")
	conf := Config{Games: "nascar04", Bind: address, PublicIP: address, HealthPort: 8086}
	g := nascar04.Definition()
	if got, err := conf.Select([]game.Definition{g}); err != nil || len(got) != 1 {
		t.Fatalf("select NASCAR: %v, %v", got, err)
	}
	for _, titles := range []string{"", "typo", "nascar04,nascar04", "nascar04,"} {
		invalid := conf
		invalid.Games = titles
		if _, err := invalid.Select([]game.Definition{g}); err == nil {
			t.Fatalf("accepted title list %q", titles)
		}
	}
	conf.HealthPort = g.TCPPort
	if _, err := conf.Select([]game.Definition{g}); err == nil {
		t.Fatal("accepted overlapping health and game ports")
	}
}

func TestFailedBindReleasesPreviouslyBoundHealthPort(t *testing.T) {
	held, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	healthProbe, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	healthPort := healthProbe.Addr().(*net.TCPAddr).AddrPort().Port()
	healthProbe.Close()
	address := netip.MustParseAddr("127.0.0.1")
	conf := Config{Games: "nascar04", Bind: address, PublicIP: address, HealthPort: healthPort}
	g := nascar04.Definition()
	g.TCPPort = held.Addr().(*net.TCPAddr).AddrPort().Port()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if err := Run(context.Background(), conf, []game.Definition{g}, nil, log); err == nil {
		t.Fatal("started with an occupied title listener")
	}
	l, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(healthPort))))
	if err != nil {
		t.Fatal("health listener leaked after startup failed", err)
	}
	l.Close()
}
