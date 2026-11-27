package gera

import (
	"context"
	"errors"
	"net/mail"
	"sort"
	"strconv"

	"github.com/dead-game-entrepreneurs/rei-nation/internal/protocol/aries"
	"github.com/dead-game-entrepreneurs/rei-nation/internal/protocol/fesl"
	"github.com/dead-game-entrepreneurs/rei-nation/internal/storage"

	"golang.org/x/crypto/bcrypt"
)

// errNotAuthenticated is the code the client shows as $notAuthenicated
// (0x14 in its account request callback, FUN_00327908).
const errNotAuthenticated = 20

// profileKeys are the account-screen fields named in the client's acct strings.
// Email and country live on the account itself.
var profileKeys = []string{"DOBDay", "DOBMonth", "DOBYear", "eaMailFlag", "thirdPartyMailFlag", "parentalEmail"}

var countryNames = map[string]string{"US": "United States"}

// profileStore is what the account screen needs beyond IdentityStore.
type profileStore interface {
	storage.ProfileStore
}

// requestFieldNames lists a request's field names for the log, without values.
func requestFieldNames(m fesl.Message) []string {
	var names []string
	for k := range m.Fields() {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// profileFields reads the profile fields present in a request.
func profileFields(m fesl.Message) map[string]string {
	out := map[string]string{}
	for _, k := range profileKeys {
		if v, ok := m.GetFold(k); ok && len(v) <= 128 {
			out[k] = v
		}
	}
	return out
}

// saveRegistration keeps the profile fields sent with AddAccount.
func (s *server) saveRegistration(ctx context.Context, name string, m fesl.Message) {
	ps, ok := s.store.(profileStore)
	details := profileFields(m)
	if !ok || len(details) == 0 {
		return
	}
	acc, found, err := s.store.Account(ctx, name)
	if err == nil && found {
		err = ps.UpdateAccountProfile(ctx, acc.ID, "", details)
	}
	if err != nil {
		s.opts.Log.Error("save account profile failed", "error", err)
	}
}

// getAccount answers the Account Options screen. Without a reply the screen
// waits on its request.
func (s *server) getAccount(ctx context.Context, c *aries.Client, m fesl.Message) {
	a := s.session(c)
	s.opts.Log.Info("acct GetAccount", "conn", c.ID(), "fields", requestFieldNames(m))
	if a.id == 0 {
		s.acctError(c, m, errNotAuthenticated)
		return
	}
	acc, found, err := s.store.Account(ctx, a.name)
	if err != nil || !found {
		s.acctError(c, m, errServer)
		return
	}
	details := map[string]string{}
	if ps, ok := s.store.(profileStore); ok {
		if details, err = ps.AccountProfile(ctx, acc.ID); err != nil {
			s.opts.Log.Error("account profile read failed", "conn", c.ID(), "error", err)
			details = map[string]string{}
		}
	}
	country := acc.Loc
	if country == "" {
		country = "US"
	}
	desc := countryNames[country]
	if desc == "" {
		desc = country
	}
	fields := []fesl.Field{
		{Key: "name", Value: acc.Name},
		{Key: "email", Value: acc.Mail},
		{Key: "countryCode", Value: country},
		{Key: "countryDesc", Value: desc},
		{Key: "userId", Value: strconv.FormatInt(acc.ID, 10)},
	}
	defaults := map[string]string{"eaMailFlag": "0", "thirdPartyMailFlag": "0"}
	for _, k := range profileKeys {
		v, ok := details[k]
		if !ok {
			v = defaults[k]
		}
		fields = append(fields, fesl.Field{Key: k, Value: v})
	}
	c.Send(fesl.Reply(m, m.TXN, fields...))
}

// updateAccount saves the Account Options screen: email, date of birth and
// mail preferences, and a new password when the current one is given.
func (s *server) updateAccount(ctx context.Context, c *aries.Client, m fesl.Message) {
	a := s.session(c)
	s.opts.Log.Info("acct "+m.TXN, "conn", c.ID(), "fields", requestFieldNames(m))
	if a.id == 0 {
		s.acctError(c, m, errNotAuthenticated)
		return
	}
	ps, ok := s.store.(profileStore)
	if !ok {
		s.acctError(c, m, errServer)
		return
	}
	acc, found, err := s.store.Account(ctx, a.name)
	if err != nil || !found {
		s.acctError(c, m, errServer)
		return
	}
	// Validate every field first, then apply all changes in one transaction.
	address, _ := m.GetFold("email")
	if address != "" {
		if _, err := mail.ParseAddress(address); err != nil || len(address) > 128 {
			s.acctError(c, m, errMissingField)
			return
		}
	}
	var hash string
	if newPassword, ok := m.GetFold("newPassword"); ok && newPassword != "" {
		current, _ := m.GetFold("password")
		if bcrypt.CompareHashAndPassword([]byte(acc.Hash), []byte(current)) != nil {
			s.acctError(c, m, errBadPassword)
			return
		}
		h, err := bcrypt.GenerateFromPassword([]byte(newPassword), bcrypt.DefaultCost)
		if err != nil {
			s.acctError(c, m, errServer)
			return
		}
		hash = string(h)
	}
	changed, err := ps.UpdateAccountSettings(ctx, acc, hash, address, profileFields(m))
	switch {
	case errors.Is(err, storage.ErrConflict):
		s.acctError(c, m, errDuplicate)
	case err != nil || !changed:
		s.opts.Log.Error("account update failed", "conn", c.ID(), "error", err)
		s.acctError(c, m, errServer)
	default:
		s.opts.Log.Info("account updated", "conn", c.ID(), "account_id", acc.ID, "password_changed", hash != "")
		c.Send(fesl.Reply(m, m.TXN))
	}
}
