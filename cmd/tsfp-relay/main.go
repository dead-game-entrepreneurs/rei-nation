package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/netip"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/dead-game-entrepreneurs/rei-nation/internal/games/tsfp/relaynode"
)

func main() {
	server := flag.String("server", "", "Rei Nation relay control address")
	ca := flag.String("ca", "", "pinned relay control certificate")
	tokenFile := flag.String("token-file", "", "relay authentication token file")
	publicIP := flag.String("public-ip", "", "public IPv4 advertised to players")
	bind := flag.String("bind", "0.0.0.0", "local IPv4 bind address")
	ports := flag.String("ports", "20000-20255", "UDP relay port range")
	name := flag.String("name", "relay", "relay node name")
	flag.Parse()
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := run(*server, *ca, *tokenFile, *publicIP, *bind, *ports, *name, log); err != nil {
		log.Error("TSFP relay stopped", "error", err)
		os.Exit(1)
	}
}

func run(server, ca, tokenFile, public, bind, ports, name string, log *slog.Logger) error {
	publicIP, err := netip.ParseAddr(public)
	if err != nil || !publicIP.Is4() {
		return errors.New("invalid public IPv4 address")
	}
	bindIP, err := netip.ParseAddr(bind)
	if err != nil || !bindIP.Is4() {
		return errors.New("invalid bind IPv4 address")
	}
	low, high, ok := strings.Cut(ports, "-")
	first, firstErr := strconv.ParseUint(low, 10, 16)
	last, lastErr := strconv.ParseUint(high, 10, 16)
	if !ok || firstErr != nil || lastErr != nil || first == 0 || last < first {
		return errors.New("invalid relay port range")
	}
	if server == "" || ca == "" || tokenFile == "" {
		return errors.New("set server, ca, and token-file")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return relaynode.Run(ctx, relaynode.Config{Server: server, CAFile: ca, TokenFile: tokenFile,
		Name: name, PublicIP: publicIP, Bind: bindIP, FirstPort: uint16(first), LastPort: uint16(last), Log: log})
}
