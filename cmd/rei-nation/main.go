package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/dead-game-entrepreneurs/rei-nation/internal/app"
	"github.com/dead-game-entrepreneurs/rei-nation/internal/game"
	"github.com/dead-game-entrepreneurs/rei-nation/internal/games/gera"
	"github.com/dead-game-entrepreneurs/rei-nation/internal/games/nascar04"
	"github.com/dead-game-entrepreneurs/rei-nation/internal/games/tsfp"
	"github.com/dead-game-entrepreneurs/rei-nation/internal/storage"
)

func main() {
	health := flag.Bool("healthcheck", false, "check the local readiness endpoint")
	flag.Parse()
	var dest io.Writer = os.Stderr
	// REI_NATION_LOG_FILE additionally writes logs to a file, which is useful on
	// Windows where piping a program's stderr mangles it.
	if path := os.Getenv("REI_NATION_LOG_FILE"); path != "" && !*health {
		if f, err := os.Create(path); err == nil {
			defer f.Close()
			dest = io.MultiWriter(os.Stderr, f)
		}
	}
	log := slog.New(slog.NewJSONHandler(dest, nil))
	if err := run(*health, log); err != nil {
		log.Error("Rei Nation stopped", "error", err)
		os.Exit(1)
	}
}

func run(health bool, log *slog.Logger) error {
	port := env("REI_NATION_HEALTH_PORT", "8086")
	healthPort, err := strconv.ParseUint(port, 10, 16)
	if err != nil || healthPort == 0 {
		return errors.New("invalid REI_NATION_HEALTH_PORT")
	}
	if health {
		client := http.Client{Timeout: 2 * time.Second}
		res, err := client.Get("http://127.0.0.1:" + port + "/healthz")
		if err != nil {
			return err
		}
		defer res.Body.Close()
		if res.StatusCode != http.StatusOK {
			return errors.New("server unhealthy: " + res.Status)
		}
		return nil
	}

	bind, err := netip.ParseAddr(env("REI_NATION_BIND", "127.0.0.1"))
	if err != nil {
		return errors.New("invalid REI_NATION_BIND")
	}
	publicIP, err := netip.ParseAddr(env("REI_NATION_PUBLIC_IP", bind.String()))
	if err != nil {
		return errors.New("invalid REI_NATION_PUBLIC_IP")
	}
	conf := app.Config{Games: env("REI_NATION_GAMES", "nascar04,tsfp"), Bind: bind, PublicIP: publicIP,
		HealthPort: uint16(healthPort), Relay: env("REI_NATION_RELAY", "auto"),
		RelayDir: os.Getenv("REI_NATION_GERA_RELAY_DIR")}
	if force := os.Getenv("REI_NATION_GERA_RELAY_FORCE"); force != "" {
		conf.RelayForce = strings.Split(force, ",")
	}
	conf.SendRecoveryMail, err = app.NewRecoverySender(os.Getenv("REI_NATION_SMTP_ADDR"), os.Getenv("REI_NATION_SMTP_FROM"),
		os.Getenv("REI_NATION_SMTP_USER"), os.Getenv("REI_NATION_SMTP_PASSWORD"))
	if err != nil {
		return err
	}
	selected, err := conf.Select([]game.Definition{nascar04.Definition(), gera.Definition(),
		tsfp.Definition(env("REI_NATION_TSFP_CERT", "/tsfp-certs/fesl-cert.pem"),
			env("REI_NATION_TSFP_KEY", "/tsfp-certs/fesl-key.pem"),
			os.Getenv("REI_NATION_TSFP_RELAY_IP"), tsfp.RemoteRelayConfig{
				TokensFile: os.Getenv("REI_NATION_TSFP_RELAY_TOKENS"),
				CertFile:   os.Getenv("REI_NATION_TSFP_RELAY_CONTROL_CERT"),
				KeyFile:    os.Getenv("REI_NATION_TSFP_RELAY_CONTROL_KEY"),
			})})
	if err != nil {
		return err
	}
	for _, title := range selected {
		if title.Validate != nil {
			if err := title.Validate(conf.PublicIP); err != nil {
				return err
			}
		}
	}
	dsn := os.Getenv("REI_NATION_DATABASE_URL")
	if dsn == "" {
		password := os.Getenv("POSTGRES_PASSWORD")
		if password == "" {
			return errors.New("set REI_NATION_DATABASE_URL or POSTGRES_PASSWORD")
		}
		u := url.URL{
			Scheme: "postgres", User: url.UserPassword("eanation", password), Path: "/eanation",
			Host: net.JoinHostPort("127.0.0.1", env("REI_NATION_PG_PORT", "55434")), RawQuery: "sslmode=disable",
		}
		dsn = u.String()
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	initCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	db, err := storage.Open(initCtx, dsn)
	cancel()
	if err != nil {
		return err
	}
	defer db.Close()
	return app.Run(ctx, conf, selected, db, log)
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
