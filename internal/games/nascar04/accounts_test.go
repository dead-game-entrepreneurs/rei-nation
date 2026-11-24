package nascar04

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/dead-game-entrepreneurs/rei-nation/internal/game"
	"github.com/dead-game-entrepreneurs/rei-nation/internal/protocol/aries"
	"github.com/dead-game-entrepreneurs/rei-nation/internal/storage"
)

func TestAccountRecovery(t *testing.T) {
	dsn := os.Getenv("REI_NATION_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set REI_NATION_TEST_DATABASE_URL to a test PostgreSQL database")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store, err := storage.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	type email struct{ recipient, subject, body string }
	messages := make(chan email, 4)
	h := Definition().New(game.Options{
		PublicIP: netip.MustParseAddr("127.0.0.1"), Store: store,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		SendRecoveryMail: func(_ context.Context, recipient, subject, body string) error {
			messages <- email{recipient, subject, body}
			return nil
		},
	})
	done := make(chan error, 1)
	go func() { done <- aries.Serve(ctx, ln, h) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	c := dial(t, ln.Addr().String())
	name := "Recovery" + strconv.FormatInt(time.Now().UnixNano(), 36)
	address := name + "@example.test"
	pass := encodeTestPassword(t, "pass")
	edited := encodeTestPassword(t, "changed")
	c.send("acct", map[string]string{"NAME": name, "PASS": pass})
	c.expect("acct", nil)
	c.send("edit", map[string]string{"NAME": name, "PASS": edited, "MAIL": address})
	c.expect("editpass", nil)
	c.send("edit", map[string]string{"NAME": name, "PASS": pass, "CHNG": edited, "MAIL": address})
	c.expect("edit", nil)
	c.send("auth", map[string]string{"NAME": name, "PASS": pass})
	c.expect("authpass", nil)

	c.send("lost", map[string]string{"NAME": name})
	if wire := c.expect("lost", nil); wire.Body != "" {
		t.Fatal("recovery returned data to the requester")
	}
	message := <-messages
	_, secret, ok := strings.Cut(message.body, "recovery password: ")
	secret, _, _ = strings.Cut(secret, "\r\n")
	if !ok || len(secret) != 12 || message.recipient != address {
		t.Fatal("incorrect password recovery email")
	}
	c.send("lost", map[string]string{"NAME": name})
	c.expect("lost", nil)
	if len(messages) != 0 {
		t.Fatal("repeat recovery request bypassed the cooldown")
	}
	c.send("auth", map[string]string{"NAME": name, "PASS": edited})
	c.expect("auth", map[string]string{"MAIL": address})
	recovery := encodeTestPassword(t, secret)
	c.send("auth", map[string]string{"NAME": name, "PASS": recovery})
	c.expect("auth", nil)
	c.send("auth", map[string]string{"NAME": name, "PASS": edited})
	c.expect("authpass", nil)
	c.send("edit", map[string]string{"NAME": name, "PASS": recovery, "CHNG": pass})
	c.expect("edit", nil)
	c.send("auth", map[string]string{"NAME": name, "PASS": recovery})
	c.expect("authpass", nil)
	c.send("lost", map[string]string{"MAIL": strings.ToLower(address)})
	c.expect("lost", nil)
	message = <-messages
	if message.recipient != address || !strings.Contains(message.body, name) {
		t.Fatal("incorrect account-name recovery email")
	}
	c.send("lost", map[string]string{"NAME": name + "None"})
	c.expect("lost", nil)
	if len(messages) != 0 {
		t.Fatal("unknown account produced email")
	}
}

// Each SSC2 byte uses a fixed permutation at its position. Find printable
// encodings for synthetic client credentials using the production decoder.
func encodeTestPassword(t *testing.T, password string) string {
	t.Helper()
	var encoded []byte
	for i := range len(password) {
		for b := 32; b <= 127; b++ {
			candidate := append(encoded, byte(b))
			pw, err := aries.DecodePassword("~" + url.PathEscape(string(candidate)))
			if err == nil && string(pw) == password[:i+1] {
				encoded = candidate
				break
			}
		}
		if len(encoded) != i+1 {
			t.Fatal("could not encode fixture password")
		}
	}
	return "~" + url.PathEscape(string(encoded))
}
