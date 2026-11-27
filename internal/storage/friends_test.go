package storage

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"
)

// openTest opens the database named by REI_NATION_TEST_DATABASE_URL and returns
// a unique suffix for names and titles so runs do not collide.
func openTest(t *testing.T) (*Postgres, string) {
	t.Helper()
	dsn := os.Getenv("REI_NATION_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set REI_NATION_TEST_DATABASE_URL to a test PostgreSQL database")
	}
	p, err := Open(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	var b [4]byte
	rand.Read(b[:])
	return p, hex.EncodeToString(b[:])
}

func testPersonas(t *testing.T, p *Postgres, suffix string, names ...string) []Persona {
	t.Helper()
	ctx := context.Background()
	if err := p.CreateAccount(ctx, Account{Name: "acct" + suffix, Hash: "x"}); err != nil {
		t.Fatal(err)
	}
	acct, _, err := p.Account(ctx, "acct"+suffix)
	if err != nil {
		t.Fatal(err)
	}
	var out []Persona
	for _, n := range names {
		if err := p.CreatePersona(ctx, acct.ID, n+suffix); err != nil {
			t.Fatal(err)
		}
		persona, ok, err := p.PersonaByName(ctx, n+suffix)
		if err != nil || !ok {
			t.Fatalf("persona %s: %v", n, err)
		}
		out = append(out, persona)
	}
	return out
}

func TestFriendRequests(t *testing.T) {
	p, suffix := openTest(t)
	ctx := context.Background()
	title := "test-" + suffix
	ps := testPersonas(t, p, suffix, "a", "b", "c")
	a, b, c := ps[0].Name, ps[1].Name, ps[2].Name

	if s, err := p.RequestFriend(ctx, title, a, b); err != nil || s != FriendOutgoing {
		t.Fatalf("request = %v, %v", s, err)
	}
	if s, err := p.RequestFriend(ctx, title, a, b); err != nil || s != FriendOutgoing {
		t.Fatalf("repeat request = %v, %v", s, err)
	}
	friends, err := p.Friends(ctx, title, b)
	if err != nil || len(friends) != 1 || friends[0].State != FriendIncoming || friends[0].ID != ps[0].ID {
		t.Fatalf("b's roster = %+v, %v", friends, err)
	}
	// Only the recipient can answer.
	if ok, _ := p.AnswerFriend(ctx, title, a, b, true); ok {
		t.Fatal("sender answered its own request")
	}
	if ok, err := p.AnswerFriend(ctx, title, b, a, true); err != nil || !ok {
		t.Fatalf("answer = %v, %v", ok, err)
	}
	if friends, _ := p.Friends(ctx, title, a); len(friends) != 1 || friends[0].State != FriendAccepted {
		t.Fatalf("a's roster = %+v", friends)
	}

	// Crossing requests accept each other.
	p.RequestFriend(ctx, title, a, c)
	if s, err := p.RequestFriend(ctx, title, c, a); err != nil || s != FriendAccepted {
		t.Fatalf("crossing request = %v, %v", s, err)
	}

	// A declined request is gone; a block refuses new requests.
	p.RequestFriend(ctx, title, b, c)
	if ok, _ := p.AnswerFriend(ctx, title, c, b, false); !ok {
		t.Fatal("decline failed")
	}
	if _, err := p.ChangeBuddy(ctx, title, c, b, "block"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.RequestFriend(ctx, title, b, c); err != ErrFriendUnavailable {
		t.Fatalf("request to a blocker = %v", err)
	}
	if ok, err := p.ChangeBuddy(ctx, title, a, b, "remove"); err != nil || !ok {
		t.Fatalf("remove = %v, %v", ok, err)
	}
	if friends, _ := p.Friends(ctx, title, b); len(friends) != 0 {
		t.Fatalf("b's roster after remove = %+v", friends)
	}
}

func TestPlayerStats(t *testing.T) {
	p, suffix := openTest(t)
	ctx := context.Background()
	title := "test-" + suffix
	ps := testPersonas(t, p, suffix, "x", "y", "z")
	text := "note"
	for _, u := range []struct {
		persona int64
		value   string
	}{{ps[0].ID, "5"}, {ps[0].ID, "2.5"}, {ps[1].ID, "9"}, {ps[2].ID, "7.5"}} {
		if err := p.UpdateStats(ctx, title, u.persona, []StatUpdate{{Key: "kills", Value: u.value, Add: true}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.UpdateStats(ctx, title, ps[0].ID, []StatUpdate{{Key: "best", Value: "3", Text: &text}, {Key: "best", Value: "1"}}); err != nil {
		t.Fatal(err)
	}

	name, values, err := p.PersonaStats(ctx, title, ps[0].ID, []string{"kills", "best", "none"})
	if err != nil || name != ps[0].Name {
		t.Fatalf("PersonaStats name %q, %v", name, err)
	}
	want := []StatValue{{Key: "kills", Value: "7.5000", Rank: 2}, {Key: "best", Value: "1.0000", Text: "note", Rank: 1}, {Key: "none", Value: "0.0000"}}
	for i := range want {
		if values[i] != want[i] {
			t.Fatalf("value %d = %+v, want %+v", i, values[i], want[i])
		}
	}

	board, err := p.TopStats(ctx, title, "kills", 2, 5, []string{"best"})
	if err != nil || len(board) != 2 {
		t.Fatalf("board = %+v, %v", board, err)
	}
	// x and z tie at 7.5 for second place; the lower persona ID comes first.
	if board[0].Rank != 2 || board[1].Rank != 2 || board[0].PersonaID != ps[0].ID {
		t.Fatalf("board = %+v", board)
	}
	if board[0].Extra[0].Value != "1.0000" || board[1].Extra[0].Value != "0.0000" {
		t.Fatalf("extra = %+v / %+v", board[0].Extra, board[1].Extra)
	}
}
