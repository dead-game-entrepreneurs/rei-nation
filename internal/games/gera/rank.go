package gera

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"regexp"
	"strconv"
	"strings"

	"github.com/dead-game-entrepreneurs/rei-nation/internal/protocol/aries"
	"github.com/dead-game-entrepreneurs/rei-nation/internal/protocol/fesl"
	"github.com/dead-game-entrepreneurs/rei-nation/internal/storage"
)

// The rank service stores keyed persona stats and serves them back as ranked
// stats and leaderboards. GoldenEye uses the same FESL rank library and field
// names as TimeSplitters: Future Perfect (u.N.s.M.k/v/ut/t, stats.N.*,
// keys.N, minRank/maxRank), so this follows that server's handling. Which keys
// GoldenEye reports and which update types it uses are logged on each report
// until a live capture settles them.
const (
	// ownerPersona is the rank owner type for a persona.
	ownerPersona = 1
	// updateAdd accumulates the reported value. GoldenEye sends it for most
	// match totals and updateReplace for current loss streaks, which reset, so
	// 5 is taken to replace the value. updateSet (0) is assumed to replace too.
	updateSet     = 0
	updateAdd     = 3
	updateReplace = 5
	maxStatKeys   = 64
	// The client keeps 16 leaderboard rows (0x780 bytes of 0x78-byte entries in
	// the shared library).
	maxBoardRows = 16
)

var (
	statKey   = regexp.MustCompile(`^[A-Za-z0-9_]{1,32}$`)
	statValue = regexp.MustCompile(`^-?[0-9]{1,18}(\.[0-9]{1,4})?$`)
)

func (s *server) handleRank(ctx context.Context, c *aries.Client, m fesl.Message) {
	a := s.session(c)
	txn := strings.ToLower(m.TXN)
	switch txn {
	case "getsessionid":
		if a.rankSession == 0 {
			a.rankSession = newRankSession()
		}
		c.Send(fesl.Reply(m, m.TXN, fesl.Field{Key: "sessionId", Value: strconv.FormatUint(uint64(a.rankSession), 10)}))
		return
	case "startreport", "endreport", "addgameinfo":
		s.opts.Log.Info("rank request", "conn", c.ID(), "txn", m.TXN, "persona_id", a.personaID)
		c.Send(fesl.Reply(m, m.TXN))
		return
	case "updatestats", "getstats", "getrankedstats", "gettopn", "gettopnandstats":
	default:
		// Answer everything (GetDateRange, for one) so a leaderboard screen does
		// not wait out its timeout.
		s.opts.Log.Info("unhandled rank transaction", "conn", c.ID(), "txn", m.TXN)
		s.acctError(c, m, errMissingField)
		return
	}
	if a.personaID == 0 {
		s.acctError(c, m, errNoSuchUser)
		return
	}
	if s.stats == nil {
		// Without a database reports are discarded and reads are empty.
		if txn == "updatestats" {
			c.Send(fesl.Reply(m, m.TXN))
		} else {
			c.Send(fesl.Reply(m, m.TXN, fesl.Field{Key: "stats.[]", Value: "0"}))
		}
		return
	}
	switch txn {
	case "updatestats":
		s.updateStats(ctx, c, m, a)
	case "getstats", "getrankedstats":
		s.personaStats(ctx, c, m, a)
	default:
		s.topStats(ctx, c, m, txn == "gettopnandstats")
	}
}

func (s *server) updateStats(ctx context.Context, c *aries.Client, m fesl.Message, a *account) {
	owners, ok := intField(m, "u.[]", 0, 0, 4)
	if !ok {
		s.acctError(c, m, errMissingField)
		return
	}
	type report struct {
		owner   int64
		updates []storage.StatUpdate
	}
	var reports []report
	total := 0
	for i := 0; i < owners; i++ {
		prefix := "u." + strconv.Itoa(i)
		kind, _ := intField(m, prefix+".ot", 0, 0, 65535)
		owner, _ := intField(m, prefix+".o", 0, 0, 1<<31-1)
		count, ok := intField(m, prefix+".s.[]", 0, 0, maxStatKeys)
		total += count
		if !ok || total > maxStatKeys {
			s.acctError(c, m, errMissingField)
			return
		}
		// The host reports its guests' results along with its own, so a guest
		// admitted to one of the reporter's games may be credited too.
		if kind != ownerPersona || (int64(owner) != a.personaID && !s.theater.hostedGuest(a.personaID, int64(owner))) {
			s.opts.Log.Info("rank update skipped owner", "conn", c.ID(), "owner_type", kind, "owner", owner, "stats", count)
			continue
		}
		r := report{owner: int64(owner)}
		var logged []string
		for j := 0; j < count; j++ {
			sp := prefix + ".s." + strconv.Itoa(j)
			key, _ := m.Get(sp + ".k")
			value, _ := m.Get(sp + ".v")
			ut, _ := intField(m, sp+".ut", -1, 0, 255)
			logged = append(logged, key+"/"+strconv.Itoa(ut)+"="+value)
			if !statKey.MatchString(key) || !statValue.MatchString(value) || (ut != updateAdd && ut != updateSet && ut != updateReplace) {
				s.opts.Log.Info("rank update skipped stat", "conn", c.ID(), "key", key, "update_type", ut)
				continue
			}
			u := storage.StatUpdate{Key: key, Value: value, Add: ut == updateAdd}
			if text, ok := m.Get(sp + ".t"); ok {
				if len(text) > 256 {
					text = text[:256]
				}
				u.Text = &text
			}
			r.updates = append(r.updates, u)
		}
		s.opts.Log.Info("rank update", "conn", c.ID(), "reporter", a.personaID, "owner", owner, "stats", logged)
		reports = append(reports, r)
	}
	for _, r := range reports {
		if len(r.updates) == 0 {
			continue
		}
		if err := s.stats.UpdateStats(ctx, gameKey, r.owner, r.updates); err != nil {
			s.opts.Log.Error("rank update failed", "conn", c.ID(), "error", err)
			s.acctError(c, m, errServer)
			return
		}
		if err := s.deriveStats(ctx, r.owner, r.updates); err != nil {
			s.opts.Log.Error("rank derived stats failed", "conn", c.ID(), "error", err)
		}
	}
	c.Send(fesl.Reply(m, m.TXN))
}

// deriveStats keeps the stats the client reads but never reports. For each
// mode prefix P with reported wins (P_W) or games (P_T_G), P_W_PERC is the
// win fraction from 0 to 1 (the leaderboard shows it times 100) and P_RANK,
// which orders the leaderboards, is the win count. The original service's ranking formula is unknown.
func (s *server) deriveStats(ctx context.Context, owner int64, updates []storage.StatUpdate) error {
	prefixes := map[string]bool{}
	for _, u := range updates {
		for _, suffix := range []string{"_W", "_T_G"} {
			if p, ok := strings.CutSuffix(u.Key, suffix); ok && p != "" {
				prefixes[p] = true
			}
		}
	}
	var derived []storage.StatUpdate
	for p := range prefixes {
		_, values, err := s.stats.PersonaStats(ctx, gameKey, owner, []string{p + "_W", p + "_T_G"})
		if err != nil || len(values) != 2 {
			return err
		}
		wins, _ := strconv.ParseFloat(values[0].Value, 64)
		games, _ := strconv.ParseFloat(values[1].Value, 64)
		perc := 0.0
		if games > 0 {
			perc = min(1, wins/games)
		}
		derived = append(derived,
			storage.StatUpdate{Key: p + "_W_PERC", Value: strconv.FormatFloat(perc, 'f', 4, 64)},
			storage.StatUpdate{Key: p + "_RANK", Value: values[0].Value})
	}
	if len(derived) == 0 {
		return nil
	}
	return s.stats.UpdateStats(ctx, gameKey, owner, derived)
}

func (s *server) personaStats(ctx context.Context, c *aries.Client, m fesl.Message, a *account) {
	kind, ok1 := intField(m, "ownerType", ownerPersona, 0, 65535)
	owner, ok2 := intField(m, "owner", int(a.personaID), 0, 1<<31-1)
	keys, ok3 := statKeys(m, "keys")
	if key, ok := m.Get("key"); ok && len(keys) == 0 {
		keys, ok3 = []string{key}, statKey.MatchString(key)
	}
	if !ok1 || !ok2 || !ok3 {
		s.acctError(c, m, errMissingField)
		return
	}
	if owner == 0 {
		owner = int(a.personaID)
	}
	s.opts.Log.Info("rank read", "conn", c.ID(), "txn", m.TXN, "owner_type", kind, "keys", keys)
	if kind != ownerPersona {
		c.Send(fesl.Reply(m, m.TXN, fesl.Field{Key: "stats.[]", Value: "0"}))
		return
	}
	name, values, err := s.stats.PersonaStats(ctx, gameKey, int64(owner), keys)
	if err != nil {
		s.opts.Log.Error("rank read failed", "conn", c.ID(), "error", err)
		s.acctError(c, m, errServer)
		return
	}
	fields := []fesl.Field{{Key: "stats.[]", Value: strconv.Itoa(len(values))}}
	for i, v := range values {
		fields = append(fields, statFields("stats."+strconv.Itoa(i)+".", v, int64(owner), name)...)
	}
	c.Send(fesl.Reply(m, m.TXN, fields...))
}

func (s *server) topStats(ctx context.Context, c *aries.Client, m fesl.Message, withExtra bool) {
	key, _ := m.Get("key")
	kind, ok1 := intField(m, "ownerType", ownerPersona, 0, 65535)
	first, ok2 := intField(m, "minRank", 1, 0, 1<<31-1)
	last, ok3 := intField(m, "maxRank", maxBoardRows, 0, 1<<31-1)
	var extra []string
	ok4 := true
	if withExtra {
		extra, ok4 = statKeys(m, "keys")
	}
	if !statKey.MatchString(key) || !ok1 || !ok2 || !ok3 || !ok4 {
		s.acctError(c, m, errMissingField)
		return
	}
	first = max(first, 1)
	s.opts.Log.Info("rank board", "conn", c.ID(), "txn", m.TXN, "key", key, "min_rank", first, "max_rank", last, "extra", extra)
	if kind != ownerPersona || last < first {
		c.Send(fesl.Reply(m, m.TXN, fesl.Field{Key: "stats.[]", Value: "0"}))
		return
	}
	rows, err := s.stats.TopStats(ctx, gameKey, key, first, min(last-first+1, maxBoardRows), extra)
	if err != nil {
		s.opts.Log.Error("rank board failed", "conn", c.ID(), "error", err)
		s.acctError(c, m, errServer)
		return
	}
	fields := []fesl.Field{{Key: "stats.[]", Value: strconv.Itoa(len(rows))}}
	for i, r := range rows {
		prefix := "stats." + strconv.Itoa(i) + "."
		fields = append(fields, statFields(prefix, r.StatValue, r.PersonaID, r.Name)...)
		for j, e := range r.Extra {
			ep := prefix + "addStats." + strconv.Itoa(j) + "."
			fields = append(fields, fesl.Field{Key: ep + "key", Value: e.Key},
				fesl.Field{Key: ep + "value", Value: e.Value}, fesl.Field{Key: ep + "text", Value: e.Text})
		}
	}
	c.Send(fesl.Reply(m, m.TXN, fields...))
}

func statFields(prefix string, v storage.StatValue, owner int64, name string) []fesl.Field {
	return []fesl.Field{
		{Key: prefix + "key", Value: v.Key},
		{Key: prefix + "value", Value: v.Value},
		{Key: prefix + "rank", Value: strconv.FormatInt(v.Rank, 10)},
		{Key: prefix + "text", Value: v.Text},
		{Key: prefix + "owner", Value: strconv.FormatInt(owner, 10)},
		{Key: prefix + "name", Value: name},
	}
}

// statKeys reads a prefix.[] counted key list.
func statKeys(m fesl.Message, prefix string) ([]string, bool) {
	n, ok := intField(m, prefix+".[]", 0, 0, maxStatKeys)
	if !ok {
		return nil, false
	}
	keys := make([]string, 0, n)
	for i := 0; i < n; i++ {
		k, _ := m.Get(prefix + "." + strconv.Itoa(i))
		if !statKey.MatchString(k) {
			return nil, false
		}
		keys = append(keys, k)
	}
	return keys, true
}

// intField reads an optional decimal field, using def when it is absent. Field
// names are matched without regard to case, as the client's spelling varies.
func intField(m fesl.Message, key string, def, lo, hi int) (int, bool) {
	v, ok := m.GetFold(key)
	if !ok || v == "" {
		return def, true
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < lo || n > hi {
		return def, false
	}
	return n, true
}

// newRankSession returns a positive 31-bit session identifier.
func newRankSession() uint32 {
	var b [4]byte
	rand.Read(b[:])
	return binary.BigEndian.Uint32(b[:])%0x7ffffffe + 1
}
