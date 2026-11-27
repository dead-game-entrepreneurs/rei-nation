// Command rei-relay runs a UDP relay node for GoldenEye: Rogue Agent players
// whose NATs block a direct connection, and sets up the server side.
//
// Register a node on the server (writes the relay directory and the node's
// token file):
//
//	rei-relay setup -dir relay -name relay1 -ip 198.51.100.7
//
// Then run the node on the machine with that public IP, with the control
// certificate and token file copied over:
//
//	rei-relay -server game.example.com:18043 -cert control-cert.pem \
//	    -token-file relay1-token.txt -ports 20000-20999
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/dead-game-entrepreneurs/rei-nation/internal/games/gera/relay"
)

func main() {
	var err error
	if len(os.Args) > 1 && os.Args[1] == "setup" {
		err = setup(os.Args[2:])
	} else {
		err = run(os.Args[1:])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "rei-relay:", err)
		os.Exit(1)
	}
}

func setup(args []string) error {
	fs := flag.NewFlagSet("setup", flag.ExitOnError)
	dir := fs.String("dir", "relay", "relay directory on the server (REI_NATION_GERA_RELAY_DIR)")
	name := fs.String("name", "relay1", "node name")
	ip := fs.String("ip", "", "the node's public IPv4 address; players are sent here")
	fs.Parse(args)
	addr, err := netip.ParseAddr(*ip)
	if err != nil {
		return errors.New("-ip must be the node's public IPv4 address")
	}
	token, err := relay.Setup(*dir, *name, addr)
	if err != nil {
		return err
	}
	tokenFile := filepath.Join(*dir, *name+"-token.txt")
	if err := os.WriteFile(tokenFile, []byte(token+"\n"), 0o600); err != nil {
		return err
	}
	fmt.Printf("Registered relay node %s at %s.\n", *name, addr)
	fmt.Printf("Copy %s and %s to the node, then delete the token file here.\n",
		filepath.Join(*dir, relay.CertFile), tokenFile)
	fmt.Printf("Set REI_NATION_GERA_RELAY_DIR=%s on the server and restart it.\n", *dir)
	return nil
}

func run(args []string) error {
	fs := flag.NewFlagSet("rei-relay", flag.ExitOnError)
	server := fs.String("server", "", "the Rei Nation server's relay control address, host:18043")
	certFile := fs.String("cert", relay.CertFile, "the server's control certificate (pinned)")
	tokenFile := fs.String("token-file", "", "this node's token file")
	bind := fs.String("bind", "0.0.0.0", "local address for relay ports")
	ports := fs.String("ports", "20000-20999", "UDP port range to open and forward; one port per relayed player")
	name := fs.String("name", "relay1", "node name, as registered")
	publicIP := fs.String("public-ip", "", "informational; the server uses the IP registered for this node")
	allowPrivate := fs.Bool("allow-private", false, "allow private player addresses (LAN tests only)")
	fs.Parse(args)
	if *server == "" || *tokenFile == "" {
		return errors.New("-server and -token-file are required")
	}
	low, high, ok := strings.Cut(*ports, "-")
	lo, err1 := strconv.Atoi(low)
	hi, err2 := strconv.Atoi(high)
	if !ok || err1 != nil || err2 != nil || lo < 1024 || hi > 65535 || lo > hi {
		return errors.New("-ports must be a range such as 20000-20999")
	}
	bindAddr, err := netip.ParseAddr(*bind)
	if err != nil || !bindAddr.Is4() {
		return errors.New("-bind must be an IPv4 address")
	}
	cert, err := os.ReadFile(*certFile)
	if err != nil {
		return err
	}
	token, err := os.ReadFile(*tokenFile)
	if err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	node := &relay.Node{Bind: bindAddr, PortLow: lo, PortHigh: hi, AllowPrivate: *allowPrivate, Log: log}
	log.Info("relay node starting", "server", *server, "ports", *ports, "name", *name)
	return node.Run(ctx, relay.ControlConfig{Server: *server, CertPEM: cert, Token: strings.TrimSpace(string(token)),
		PublicIP: *publicIP, Name: *name})
}
