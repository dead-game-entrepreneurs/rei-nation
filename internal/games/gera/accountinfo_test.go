package gera

import (
	"context"
	"io"
	"log/slog"
	"net/netip"
	"strconv"
	"testing"

	"github.com/dead-game-entrepreneurs/rei-nation/internal/game"
	"github.com/dead-game-entrepreneurs/rei-nation/internal/protocol/aries"
	"github.com/dead-game-entrepreneurs/rei-nation/internal/protocol/fesl"
	"github.com/dead-game-entrepreneurs/rei-nation/internal/storage"
)

// memProfiles adds account profiles and password changes to memStore.
type memProfiles struct {
	*memStore
	profiles map[int64]map[string]string
}

func (s *memProfiles) AccountProfile(_ context.Context, id int64) (map[string]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]string{}
	for k, v := range s.profiles[id] {
		out[k] = v
	}
	return out, nil
}

func (s *memProfiles) UpdateAccountProfile(ctx context.Context, id int64, mail string, details map[string]string) error {
	_, err := s.UpdateAccountSettings(ctx, storage.Account{ID: id}, "", mail, details)
	return err
}

func (s *memProfiles) UpdateAccountSettings(_ context.Context, current storage.Account, hash, mail string,
	details map[string]string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, a := range s.accounts {
		if a.ID != current.ID {
			continue
		}
		if hash != "" {
			if a.Hash != current.Hash {
				return false, nil
			}
			a.Hash = hash
		}
		if mail == conflictMail {
			return false, storage.ErrConflict
		}
		if mail != "" {
			a.Mail = mail
		}
		s.accounts[key] = a
	}
	if s.profiles[current.ID] == nil {
		s.profiles[current.ID] = map[string]string{}
	}
	for k, v := range details {
		s.profiles[current.ID][k] = v
	}
	return true, nil
}

// conflictMail is an address the test store reports as used by another account.
const conflictMail = "taken@example.com"

func TestAccountOptions(t *testing.T) {
	store := &memProfiles{memStore: newMemStore(), profiles: map[int64]map[string]string{}}
	s := Definition(false).New(game.Options{PublicIP: netip.MustParseAddr("192.0.2.10"),
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Store: store}).(*server)
	_, conn, cleanup := dialHandler(t, s)
	defer cleanup()
	seq := uint32(0xc0000000)
	call := func(txn string, fields ...fesl.Field) fesl.Message {
		t.Helper()
		seq++
		if err := fesl.Encode("acct", seq, txn, fields...).Write(conn); err != nil {
			t.Fatal(err)
		}
		f, err := aries.Read(conn)
		if err != nil {
			t.Fatal(err)
		}
		return fesl.Decode(f)
	}
	errorCode := func(m fesl.Message) string { v, _ := m.Get("errorCode"); return v }

	if code := errorCode(call("GetAccount")); code != strconv.Itoa(errNotAuthenticated) {
		t.Fatalf("GetAccount before login errorCode = %q", code)
	}
	call("AddAccount", field("name", "bond"), field("password", "pw-1"), field("email", "bond@example.com"),
		field("countryCode", "US"), field("DOBDay", "13"), field("DOBMonth", "4"), field("DOBYear", "1970"),
		field("eaMailFlag", "1"), field("thirdPartyMailFlag", "0"))
	call("Login", field("name", "bond"), field("password", "pw-1"))

	info := call("GetAccount")
	for k, want := range map[string]string{"name": "bond", "email": "bond@example.com", "countryCode": "US",
		"countryDesc": "United States", "DOBDay": "13", "DOBMonth": "4", "DOBYear": "1970", "eaMailFlag": "1",
		"thirdPartyMailFlag": "0"} {
		if got, _ := info.Get(k); got != want {
			t.Errorf("GetAccount %s = %q, want %q", k, got, want)
		}
	}

	if code := errorCode(call("UpdateAccount", field("email", "007@example.com"), field("eaMailFlag", "0"))); code != "" {
		t.Fatalf("UpdateAccount errorCode %s", code)
	}
	if v, _ := call("GetAccount").Get("email"); v != "007@example.com" {
		t.Fatalf("email after update = %q", v)
	}
	if code := errorCode(call("UpdateAccount", field("password", "wrong"), field("newPassword", "pw-2"))); code != strconv.Itoa(errBadPassword) {
		t.Fatalf("password change with a wrong password errorCode = %q", code)
	}
	if code := errorCode(call("UpdateAccount", field("password", "pw-1"), field("newPassword", "pw-2"))); code != "" {
		t.Fatalf("password change errorCode %s", code)
	}
	if code := errorCode(call("Login", field("name", "bond"), field("password", "pw-2"))); code != "" {
		t.Fatalf("login with the new password errorCode %s", code)
	}

	// A rejected update changes nothing, including a valid password change.
	if code := errorCode(call("UpdateAccount", field("password", "pw-2"), field("newPassword", "pw-3"),
		field("email", "not an address"))); code != strconv.Itoa(errMissingField) {
		t.Fatalf("invalid email errorCode = %q", code)
	}
	if code := errorCode(call("UpdateAccount", field("password", "pw-2"), field("newPassword", "pw-3"),
		field("email", conflictMail))); code != strconv.Itoa(errDuplicate) {
		t.Fatalf("conflicting email errorCode = %q", code)
	}
	if code := errorCode(call("Login", field("name", "bond"), field("password", "pw-2"))); code != "" {
		t.Fatalf("rejected update changed the password: login errorCode %s", code)
	}
}
