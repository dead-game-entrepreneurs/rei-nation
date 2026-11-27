package gera

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"regexp"
	"strconv"
	"strings"

	"github.com/dead-game-entrepreneurs/rei-nation/internal/protocol/aries"
	"github.com/dead-game-entrepreneurs/rei-nation/internal/protocol/fesl"
	"github.com/dead-game-entrepreneurs/rei-nation/internal/storage"

	"golang.org/x/crypto/bcrypt"
)

// tosText is returned for GetTos. The client only needs a non-empty body to
// show the terms screen; this is a private preservation server with no terms.
const tosText = "Rei Nation is a private server for game preservation. " +
	"It is not affiliated with Electronic Arts. No commercial terms apply."

// FESL account error codes observed from EA clients.
const (
	errMissingField = 21  // a required field was empty
	errNoSuchUser   = 101 // account or persona not found
	errBadPassword  = 122 // password did not match
	errDuplicate    = 160 // name already taken
	errServer       = 99  // server-side failure
)

var sanitizeName = regexp.MustCompile(`[^A-Za-z0-9]`)

// account is the per-connection login state.
type account struct {
	id          int64
	name        string
	personaID   int64
	personaName string
	lkey        string
	rankSession uint32 // rank GetSessionId value, kept for this connection
}

func (s *server) handleAcct(ctx context.Context, c *aries.Client, m fesl.Message) {
	if s.store == nil {
		s.acctError(c, m, errServer)
		return
	}
	switch strings.ToLower(m.TXN) {
	case "gettos":
		c.Send(fesl.Reply(m, m.TXN, fesl.Field{Key: "tos", Value: tosText}))
	case "getcountrylist":
		s.getCountryList(c, m)
	case "suggestscreennames", "suggestsubscreennames":
		s.suggestNames(c, m)
	case "addaccount":
		s.addAccount(ctx, c, m)
	case "login":
		s.login(ctx, c, m)
	case "getsubaccounts":
		s.getSubAccounts(ctx, c, m)
	case "addsubaccount":
		s.addSubAccount(ctx, c, m)
	case "loginsubaccount":
		s.loginSubAccount(ctx, c, m)
	case "logout":
		a := s.session(c)
		*a = account{}
		s.revokeKeys(c, 0)
		c.Send(fesl.Reply(m, m.TXN))
	case "getaccount":
		s.getAccount(ctx, c, m)
	case "updateaccount", "updatepassword":
		s.updateAccount(ctx, c, m)
	default:
		// Answer so a screen does not wait on the request.
		s.opts.Log.Info("unhandled acct transaction", "conn", c.ID(), "txn", m.TXN, "fields", requestFieldNames(m))
		s.acctError(c, m, errMissingField)
	}
}

func (s *server) acctError(c *aries.Client, m fesl.Message, code int) {
	c.Send(fesl.Reply(m, m.TXN, fesl.Field{Key: "errorCode", Value: strconv.Itoa(code)}))
}

func (s *server) getCountryList(c *aries.Client, m fesl.Message) {
	c.Send(fesl.Reply(m, m.TXN,
		fesl.Field{Key: "countryList.[]", Value: "1"},
		fesl.Field{Key: "countryList.0.description", Value: "United States"},
		fesl.Field{Key: "countryList.0.ISOCode", Value: "US"},
		fesl.Field{Key: "countryList.0.parentalControlAgeLimit", Value: "13"},
		fesl.Field{Key: "countryList.0.registrationAgeLimit", Value: "18"},
	))
}

func (s *server) suggestNames(c *aries.Client, m fesl.Message) {
	count := 5
	if v, ok := m.GetFold("maxSuggestions"); ok {
		if n, err := strconv.Atoi(v); err == nil {
			count = n
		}
	}
	count = max(0, min(count, 10))
	base, _ := m.GetFold("name")
	base = sanitizeName.ReplaceAllString(base, "")
	if base == "" {
		base = "Player"
	}
	fields := make([]fesl.Field, 0, count+1)
	fields = append(fields, fesl.Field{Key: "names.[]", Value: strconv.Itoa(count)})
	for i := range count {
		suffix := strconv.Itoa(i + 1)
		stem := base
		if len(stem) > 11-len(suffix) {
			stem = stem[:11-len(suffix)]
		}
		fields = append(fields, fesl.Field{Key: "names." + strconv.Itoa(i), Value: stem + suffix})
	}
	c.Send(fesl.Reply(m, m.TXN, fields...))
}

func (s *server) addAccount(ctx context.Context, c *aries.Client, m fesl.Message) {
	name, _ := m.GetFold("name")
	password, _ := m.GetFold("password")
	if name == "" || password == "" {
		s.acctError(c, m, errMissingField)
		return
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		s.acctError(c, m, errServer)
		return
	}
	mail, _ := m.GetFold("email")
	loc, _ := m.GetFold("countryCode")
	err = s.store.CreateAccount(ctx, storage.Account{Name: name, Hash: string(hash), Mail: mail, Loc: loc})
	switch {
	case errors.Is(err, storage.ErrConflict):
		s.acctError(c, m, errDuplicate)
	case err != nil:
		s.opts.Log.Error("create account failed", "conn", c.ID(), "error", err)
		s.acctError(c, m, errServer)
	default:
		s.opts.Log.Info("account created", "conn", c.ID(), "name", name)
		s.saveRegistration(ctx, name, m)
		c.Send(fesl.Reply(m, m.TXN))
	}
}

func (s *server) login(ctx context.Context, c *aries.Client, m fesl.Message) {
	name, _ := m.GetFold("name")
	password, _ := m.GetFold("password")
	if name == "" {
		// The client may try to reuse a saved token; we never issue one, so it
		// should fall back to prompting for the name and password.
		s.acctError(c, m, errNoSuchUser)
		return
	}
	acc, found, err := s.store.Account(ctx, name)
	if err != nil {
		s.opts.Log.Error("account lookup failed", "conn", c.ID(), "error", err)
		s.acctError(c, m, errServer)
		return
	}
	if !found {
		s.acctError(c, m, errNoSuchUser)
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(acc.Hash), []byte(password)) != nil {
		s.acctError(c, m, errBadPassword)
		return
	}
	a := s.session(c)
	*a = account{id: acc.ID, name: acc.Name}
	a.lkey = s.issueSessionKey(c, identity{accountID: acc.ID, personaName: acc.Name})
	c.ClearInitialReadDeadline()
	s.opts.Log.Info("account login", "conn", c.ID(), "name", acc.Name, "account_id", acc.ID)
	c.Send(fesl.Reply(m, m.TXN,
		fesl.Field{Key: "lkey", Value: a.lkey},
		fesl.Field{Key: "userId", Value: strconv.FormatInt(acc.ID, 10)},
	))
}

func (s *server) getSubAccounts(ctx context.Context, c *aries.Client, m fesl.Message) {
	a := s.session(c)
	if a.id == 0 {
		s.acctError(c, m, errNoSuchUser)
		return
	}
	personas, err := s.store.Personas(ctx, a.id)
	if err != nil {
		s.opts.Log.Error("list personas failed", "conn", c.ID(), "error", err)
		s.acctError(c, m, errServer)
		return
	}
	fields := make([]fesl.Field, 0, len(personas)+1)
	fields = append(fields, fesl.Field{Key: "subAccounts.[]", Value: strconv.Itoa(len(personas))})
	for i, p := range personas {
		fields = append(fields, fesl.Field{Key: "subAccounts." + strconv.Itoa(i), Value: p.Name})
	}
	c.Send(fesl.Reply(m, m.TXN, fields...))
}

func (s *server) addSubAccount(ctx context.Context, c *aries.Client, m fesl.Message) {
	a := s.session(c)
	if a.id == 0 {
		s.acctError(c, m, errNoSuchUser)
		return
	}
	name, _ := m.GetFold("name")
	if name == "" {
		s.acctError(c, m, errMissingField)
		return
	}
	err := s.store.CreatePersona(ctx, a.id, name)
	switch {
	case errors.Is(err, storage.ErrConflict):
		s.acctError(c, m, errDuplicate)
	case err != nil:
		s.opts.Log.Error("create persona failed", "conn", c.ID(), "error", err)
		s.acctError(c, m, errServer)
	default:
		s.opts.Log.Info("persona created", "conn", c.ID(), "account_id", a.id, "name", name)
		c.Send(fesl.Reply(m, m.TXN))
	}
}

func (s *server) loginSubAccount(ctx context.Context, c *aries.Client, m fesl.Message) {
	a := s.session(c)
	if a.id == 0 {
		s.acctError(c, m, errNoSuchUser)
		return
	}
	name, _ := m.GetFold("name")
	personas, err := s.store.Personas(ctx, a.id)
	if err != nil {
		s.opts.Log.Error("list personas failed", "conn", c.ID(), "error", err)
		s.acctError(c, m, errServer)
		return
	}
	for _, p := range personas {
		if strings.EqualFold(p.Name, name) {
			a.personaID = p.ID
			a.personaName = p.Name
			a.lkey = s.issueSessionKey(c, identity{accountID: a.id, personaID: p.ID, personaName: p.Name})
			s.opts.Log.Info("persona login", "conn", c.ID(), "persona", p.Name, "persona_id", p.ID)
			c.Send(fesl.Reply(m, m.TXN,
				fesl.Field{Key: "lkey", Value: a.lkey},
				fesl.Field{Key: "userId", Value: strconv.FormatInt(p.ID, 10)},
			))
			return
		}
	}
	s.acctError(c, m, errNoSuchUser)
}

// newLkey returns a random session key. The messenger authenticates with the
// key issued here; the theater will reuse the same mechanism.
func newLkey() string {
	var b [24]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
