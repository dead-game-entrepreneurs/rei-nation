// Package nascar04 implements the NASCAR Thunder 2004 PS2 lobby.
package nascar04

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dead-game-entrepreneurs/rei-nation/internal/connectivity"
	"github.com/dead-game-entrepreneurs/rei-nation/internal/game"
	"github.com/dead-game-entrepreneurs/rei-nation/internal/protocol/aries"
	"github.com/dead-game-entrepreneurs/rei-nation/internal/storage"

	"golang.org/x/crypto/bcrypt"
)

func Definition() game.Definition {
	return game.Definition{Key: gameKey, TCPPort: 10600, AuxTCP: []uint16{buddyPort, guestPort},
		UDP: []uint16{guestPort, hostPort, peerPort}, New: func(opts game.Options) game.Runtime {
			rooms := initialRooms()
			l := &lobby{
				opts: opts, players: make(map[*aries.Client]*player), online: make(map[string]*player),
				rooms: rooms, nextRoom: len(rooms) + 1, races: make(map[int64][]*raceSession),
			}
			l.stats, _ = opts.Store.(storage.RaceStore)
			l.accounts, _ = opts.Store.(storage.AccountManagementStore)
			l.reports, _ = opts.Store.(storage.ReportStore)
			if store, ok := opts.Store.(storage.MessengerStore); ok && opts.AuxTCP[buddyPort] != nil {
				l.buddy = &messenger{lobby: l, store: store, clients: make(map[*aries.Client]*buddyClient),
					online: make(map[string]*buddyClient), lobbyOnline: make(map[string]*player),
					pendingAuth: make(map[*aries.Client]time.Time)}
			}
			if opts.UDP[guestPort] != nil {
				l.demangler = connectivity.NewDemangler(opts.PublicIP, guestPort)
				l.relay = &raceRelay{mode: opts.Relay, sockets: opts.UDP, pairs: make(map[netip.Addr]*relayPair)}
			}
			return l
		}}
}

const gameKey = "nascar04"

func (l *lobby) Services() []func(context.Context) error {
	var services []func(context.Context) error
	if l.buddy != nil {
		services = append(services, func(ctx context.Context) error {
			return aries.ServeWithHeartbeat(ctx, l.opts.AuxTCP[buddyPort], l.buddy, "PING")
		}, l.buddy.monitorConnections)
	}
	if l.demangler != nil {
		services = append(services, func(ctx context.Context) error {
			return connectivity.ServeHTTP(ctx, l.opts.AuxTCP[guestPort], l.demangler)
		})
		for _, port := range []uint16{guestPort, hostPort, peerPort} {
			services = append(services, func(ctx context.Context) error {
				return connectivity.ServeUDP(ctx, l.opts.UDP[port], func(from netip.AddrPort, data []byte) {
					if port != guestPort || !l.demangler.Probe(from, data) {
						l.relay.forward(port, from, data)
					}
				})
			})
		}
	}
	return services
}

type player struct {
	client     *aries.Client
	account    storage.Account
	persona    string
	personaID  int64
	lastMade   string
	reported   netip.Addr
	room       *room
	preview    *room
	proposal   string
	offer      *raceOffer
	pending    challenge
	lkey       string
	race       *raceSession
	stats      storage.Ranked
	reputation int

	messengerLostAt time.Time
}

type lobby struct {
	opts game.Options
	// Room changes and their queued notifications share one order.
	mu         sync.Mutex
	players    map[*aries.Client]*player
	online     map[string]*player
	rooms      map[int]*room
	nextRoom   int
	races      map[int64][]*raceSession // Recent matches by stable persona ID.
	buddy      *messenger
	demangler  *connectivity.Demangler
	relay      *raceRelay
	stats      storage.RaceStore
	identityMu sync.Mutex // Serializes persona selection and deletion.
	accounts   storage.AccountManagementStore
	reports    storage.ReportStore
}

func (l *lobby) Handle(ctx context.Context, c *aries.Client, f aries.Frame) {
	fields := f.Fields()
	switch f.Tag {
	case "~png":
		return
	case "@tic":
		c.Send(aries.Reply("@tic", nil))
		return
	case "@dir":
		c.Send(aries.Reply("@dir", map[string]string{"ADDR": l.opts.PublicIP.String(), "PORT": strconv.Itoa(int(l.opts.TCPPort))}))
		return
	case "skey":
		c.Send(aries.Reply("skey", map[string]string{"SKEY": "$" + aries.SSC2Key}))
		return
	case "news":
		name := fields["NAME"]
		tag := "news"
		if len(name) == 1 && name[0] >= '0' && name[0] <= '9' {
			tag = "newsnew" + name
		}
		// The startup callback (SLUS-20824, 0x206bf0) reads Messenger
		// settings from document 0. Online News requests document 1
		// separately (0x206788) and displays that reply.
		if name == "0" {
			var fields map[string]string
			if l.buddy != nil {
				fields = map[string]string{"BUDDY_URL": l.opts.PublicIP.String(), "BUDDY_PORT": strconv.Itoa(buddyPort)}
				l.opts.Log.Info("NASCAR Messenger bootstrap served", "lobby_connection", c.ID())
			}
			c.Send(aries.Reply(tag, fields))
			return
		}
		news := aries.Reply(tag, nil)
		news.Body = "News=Welcome back to NASCAR Thunder 2004!\nTip=Create or join a room to race with a friend."
		c.Send(news)
		return
	}

	l.mu.Lock()
	p := l.players[c]
	if p == nil {
		p = &player{client: c}
		l.players[c] = p
	}
	l.mu.Unlock()

	switch f.Tag {
	case "addr":
		addr, err := netip.ParseAddr(fields["ADDR"])
		if err == nil && addr.Is4() && !addr.IsUnspecified() && !addr.IsMulticast() && addr != netip.MustParseAddr("255.255.255.255") {
			l.mu.Lock()
			p.reported = addr
			l.mu.Unlock()
		}
		c.Send(aries.Reply("addr", nil))
	case "acct":
		l.createAccount(ctx, c, fields)
	case "auth":
		l.authenticate(ctx, p, fields)
	case "edit":
		l.editAccount(ctx, p, fields)
	case "lost":
		l.recoverAccount(ctx, p, fields)
	case "cper":
		l.createPersona(ctx, p, fields)
	case "dper":
		l.deletePersona(ctx, p, fields["PERS"])
	case "pers":
		l.selectPersona(ctx, p, fields["PERS"])
	case "sele":
		l.subscribeRooms(p, f.Body)
	case "rank", "snap", "cate", "sviw":
		l.raceCommand(ctx, p, f.Tag, fields)
	case "user":
		l.searchPersona(ctx, p, fields)
	case "rept":
		l.reportPlayer(ctx, p, fields["PERS"])
	default:
		l.mu.Lock()
		defer l.mu.Unlock()
		l.handleLobby(p, f.Tag, fields)
	}
}

func (l *lobby) Disconnected(c *aries.Client) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if p := l.players[c]; p != nil {
		if p.persona != "" {
			l.opts.Log.Info("NASCAR lobby socket closed", "lobby_connection", c.ID(), "reason", c.CloseReason())
		}
		l.leave(p)
		// The game closes its lobby socket while racing. Messenger has its own
		// authenticated connection and stays up until that socket ends.
		p.lkey, p.race = "", nil
		if key := strings.ToLower(p.persona); l.online[key] == p {
			delete(l.online, key)
			if l.buddy != nil {
				l.buddy.leaveLobby(p)
			}
		}
		delete(l.players, c)
	}
}

var (
	accountName = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,32}$`)
	personaName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9 ]{2,11}$`)
	locale      = regexp.MustCompile(`^[A-Za-z]{2,8}$`)
)

func nameValue(s string) string {
	return strings.TrimSpace(strings.Trim(s, "\""))
}

func (l *lobby) createAccount(ctx context.Context, c *aries.Client, f map[string]string) {
	name := nameValue(f["NAME"])
	if !accountName.MatchString(name) {
		c.Send(aries.Reply("acctimst", nil))
		return
	}
	password, err := aries.DecodePassword(f["PASS"])
	if err != nil {
		c.Send(aries.Reply("acctpass", nil))
		return
	}
	mail := nameValue(f["MAIL"])
	if len(mail) > 128 || !wireText(mail, 128) {
		c.Send(aries.Reply("acctmail", nil))
		return
	}
	loc := f["LOC"]
	if !locale.MatchString(loc) {
		loc = f["LANG"] + f["FROM"]
		if !locale.MatchString(loc) {
			loc = "enUS"
		}
	}
	hash, err := bcrypt.GenerateFromPassword(password, bcrypt.DefaultCost)
	if err != nil {
		l.storeFailure(c, "acct", err)
		return
	}
	if err := l.opts.Store.CreateAccount(ctx, storage.Account{Name: name, Hash: string(hash), Mail: mail, Loc: loc}); err != nil {
		if errors.Is(err, storage.ErrConflict) {
			c.Send(aries.Reply("acctdupl", nil))
		} else {
			l.storeFailure(c, "acct", err)
		}
		return
	}
	c.Send(aries.Reply("acct", nil))
}

func (l *lobby) authenticate(ctx context.Context, p *player, f map[string]string) {
	name := nameValue(f["NAME"])
	pw, err := aries.DecodePassword(f["PASS"])
	if err != nil || !accountName.MatchString(name) {
		p.client.Send(aries.Reply("authpass", nil))
		return
	}
	a, ok, err := l.opts.Store.Account(ctx, name)
	if err != nil {
		l.storeFailure(p.client, "auth", err)
		return
	}
	if !ok {
		p.client.Send(aries.Reply("authimst", nil))
		return
	}
	if p.account.ID != 0 && p.account.ID != a.ID {
		p.client.Send(aries.Reply("authpass", nil))
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(a.Hash), pw) != nil && !l.useRecoveryPassword(ctx, a, pw) {
		p.client.Send(aries.Reply("authpass", nil))
		return
	}
	personas, err := l.opts.Store.Personas(ctx, a.ID)
	if err != nil {
		l.storeFailure(p.client, "auth", err)
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if p.account.ID != 0 && p.account.ID != a.ID {
		p.client.Send(aries.Reply("authpass", nil))
		return
	}
	p.account = a
	names := make([]string, 0, len(personas))
	for _, persona := range personas {
		n := persona.Name
		if strings.Contains(n, " ") {
			n = "\"" + n + "\""
		}
		names = append(names, n)
	}
	p.client.Send(aries.Reply("auth", map[string]string{
		"NAME": a.Name, "ADDR": p.client.Addr().String(), "PERSONAS": strings.Join(names, ","),
		"LOC": a.Loc, "MAIL": a.Mail, "SPAM": "NN", "TOS": "1",
	}))
}

func (l *lobby) createPersona(ctx context.Context, p *player, f map[string]string) {
	if p.account.ID == 0 {
		p.client.Send(aries.Reply("cpermaut", nil))
		return
	}
	name := nameValue(f["PERS"])
	if !personaName.MatchString(name) {
		p.client.Send(aries.Reply("cperiper", nil))
		return
	}
	if err := l.opts.Store.CreatePersona(ctx, p.account.ID, name); err != nil {
		if errors.Is(err, storage.ErrConflict) {
			p.client.Send(aries.Reply("cperdupl", nil))
		} else {
			l.storeFailure(p.client, "cper", err)
		}
		return
	}
	p.lastMade = name
	p.client.Send(aries.Reply("cper", nil))
}

func (l *lobby) selectPersona(ctx context.Context, p *player, field string) {
	if p.account.ID == 0 {
		p.client.Send(aries.Reply("persmaut", nil))
		return
	}
	name := nameValue(field)
	if name == "" {
		name = p.lastMade
	}
	l.identityMu.Lock()
	defer l.identityMu.Unlock()
	personas, err := l.opts.Store.Personas(ctx, p.account.ID)
	if err != nil {
		l.storeFailure(p.client, "pers", err)
		return
	}
	for _, owned := range personas {
		if strings.EqualFold(owned.Name, name) {
			if ctx.Err() == nil {
				l.enterLobby(ctx, p, owned)
			}
			return
		}
	}
	p.client.Send(aries.Reply("persmaut", nil))
}

func (l *lobby) enterLobby(ctx context.Context, p *player, identity storage.Persona) {
	name := identity.Name
	stats := l.playerStats(ctx, name)
	l.mu.Lock()
	defer l.mu.Unlock()
	reputation := 5
	if l.reports != nil {
		if value, err := l.reports.Reputation(ctx, gameKey, name); err == nil {
			reputation = value
		}
	}
	key := strings.ToLower(name)
	if prior := l.online[key]; prior != nil && prior != p {
		l.leave(prior)
		// Keep the Messenger socket available for resumeLobby to check
		// against the authenticated replacement session.
		prior.lkey, prior.race = "", nil
		prior.client.CloseWithReason("replaced by lobby sign-in")
	}
	// A repeated selection on the same socket keeps the current Messenger session.
	if p.personaID != identity.ID || p.lkey == "" {
		l.closeCompanions(p)
		l.leave(p)
		if oldKey := strings.ToLower(p.persona); l.online[oldKey] == p {
			delete(l.online, oldKey)
		}
		p.messengerLostAt = time.Time{}
		var token [32]byte
		rand.Read(token[:])
		p.lkey = hex.EncodeToString(token[:])
		l.opts.Log.Info("NASCAR lobby signed in", "lobby_connection", p.client.ID())
	}
	p.persona = name
	p.personaID = identity.ID
	p.stats = stats
	p.reputation = reputation
	l.online[key] = p
	if l.buddy != nil {
		l.buddy.resumeLobby(p)
	}
	p.client.Send(aries.Reply("pers", map[string]string{
		"PERS": name, "LKEY": p.lkey, "LOC": p.account.Loc,
		"A": p.client.Addr().String(), "LA": p.client.Addr().String(), "IDLE": "100000",
	}))
	l.listRooms(p)
	sendPlayerState(p)
}

func (l *lobby) closeCompanions(p *player) {
	p.lkey = ""
	if l.buddy != nil {
		l.buddy.revoke(p.persona, p)
	}
	p.race = nil
}

func (l *lobby) searchPersona(ctx context.Context, p *player, fields map[string]string) {
	if p.persona == "" {
		p.client.Send(aries.Reply("usermaut", nil))
		return
	}
	var query string
	for _, key := range []string{"PERS", "NAME", "ALIAS", "MASK"} {
		if query = nameValue(fields[key]); query != "" {
			break
		}
	}
	reply := map[string]string{"NAME": "", "SPAM": "NN", "MASK": "0"}
	if personaName.MatchString(query) {
		reply["PERS"] = query
		if store, ok := l.opts.Store.(storage.MessengerStore); ok {
			if name, found, err := store.FindPersona(ctx, query); err == nil && found {
				reply["PERS"], reply["NAME"], reply["ALIAS"] = name, name, name
				reply["STAT"] = statsCSV(l.playerStats(ctx, name).RaceTotals, false)
			}
		}
	}
	p.client.Send(aries.Reply("user", reply))
}

func (l *lobby) storeFailure(c *aries.Client, tag string, err error) {
	// Database errors can include query parameters. Keep them out of normal logs.
	l.opts.Log.Warn("identity operation failed", "command", tag, "error_type", fmt.Sprintf("%T", err))
	c.Send(aries.Reply(tag+"fail", nil))
}

func wireText(s string, maxLen int) bool {
	return len(s) <= maxLen && !strings.ContainsAny(s, "\x00\r\n\t")
}
