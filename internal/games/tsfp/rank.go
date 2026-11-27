package tsfp

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/dead-game-entrepreneurs/rei-nation/internal/protocol/aries"
	"github.com/dead-game-entrepreneurs/rei-nation/internal/storage"
)

var statKey = regexp.MustCompile(`^S_[0-9]{1,9}(_C)?$`)
var statValue = regexp.MustCompile(`^[+-]?([0-9]{1,24}(\.[0-9]{0,4})?|\.[0-9]{1,4})$`)

func (f *fesl) rank(ctx context.Context, s *feslSession, in aries.Frame, command string, v map[string]string) []string {
	switch command {
	case "getsessionid":
		if s.rankID == "" {
			n, _ := strconv.ParseUint(token(4), 16, 32)
			s.rankID = strconv.FormatUint(n%0x7ffffffe+1, 10)
		}
		return []string{"sessionId", s.rankID}
	case "startreport", "endreport", "addgameinfo":
		return []string{}
	case "updatestats", "getstats", "getrankedstats", "gettopn", "gettopnandstats":
		if s.persona.ID == 0 {
			return failure("101")
		}
		if f.ranking == nil {
			return failure("99")
		}
	default:
		return failure("21")
	}
	if command == "updatestats" {
		return f.updateStats(ctx, s, in, v)
	}
	kind, kindOK := decimal(fallback(v, "OWNERTYPE", "1"), 65535)
	period, periodOK := decimal(fallback(v, "PERIODID", "0"), maxRecordID)
	past, pastOK := decimal(fallback(v, "PERIODPAST", "0"), maxRecordID)
	if !kindOK || !periodOK || !pastOK {
		return failure("21")
	}
	if kind != 1 {
		return []string{"stats.[]", "0"}
	}
	allTime := period == 0 && past == 0
	if command == "getstats" || command == "getrankedstats" {
		return f.playerStats(ctx, s.persona.ID, v, allTime)
	}
	return f.topStats(ctx, v, allTime, command == "gettopnandstats")
}

func (f *fesl) updateStats(ctx context.Context, s *feslSession, in aries.Frame, v map[string]string) []string {
	owners, ok := decimal(fallback(v, "U.[]", "0"), 4)
	if !ok {
		return failure("21")
	}
	var updates []storage.TSFPStat
	for i := range owners {
		root := "U." + strconv.Itoa(i)
		owner, ownerOK := decimal(v[root+".O"], maxRecordID)
		if !ownerOK {
			return failure("21")
		}
		if int64(owner) != s.persona.ID || v[root+".OT"] != "1" {
			return failure("101")
		}
		n, ok := decimal(fallback(v, root+".S.[]", "0"), 64-len(updates))
		if !ok {
			return failure("21")
		}
		for j := range n {
			prefix := root + ".S." + strconv.Itoa(j)
			key := strings.ToUpper(v[prefix+".K"])
			value, text := v[prefix+".V"], v[prefix+".T"]
			if !statKey.MatchString(key) || !statValue.MatchString(value) || v[prefix+".UT"] != "3" ||
				len(text) > 512 || strings.ContainsRune(text, 0) {
				return failure("21")
			}
			_, hasText := v[prefix+".T"]
			updates = append(updates, storage.TSFPStat{Key: key, Value: value, Text: text, HasText: hasText})
		}
	}
	// A consistent row lock order prevents cross-connection update deadlocks.
	sort.SliceStable(updates, func(i, j int) bool { return updates[i].Key < updates[j].Key })
	if err := f.ranking.ApplyTSFPStats(ctx, s.persona.ID, s.rankConnection, binary.BigEndian.Uint32(in.Code[:]),
		sha256.Sum256([]byte(in.Body)), updates); err != nil {
		return failure("21")
	}
	return []string{}
}

func readKeys(v map[string]string, prefix string) ([]string, bool) {
	n, ok := decimal(fallback(v, prefix+".[]", "0"), 64)
	if !ok {
		return nil, false
	}
	keys := make([]string, n)
	for i := range n {
		key := strings.ToUpper(v[prefix+"."+strconv.Itoa(i)])
		if !statKey.MatchString(key) {
			return nil, false
		}
		keys[i] = key
	}
	return keys, true
}

func rankFields(rows []storage.TSFPRank) []string {
	pairs := []string{"stats.[]", strconv.Itoa(len(rows))}
	for i, rank := range rows {
		prefix := "stats." + strconv.Itoa(i) + "."
		pairs = append(pairs, prefix+"key", rank.Key, prefix+"value", rank.Value, prefix+"rank", strconv.Itoa(rank.Rank),
			prefix+"text", rank.Text, prefix+"owner", strconv.FormatInt(rank.Persona.ID, 10), prefix+"name", rank.Persona.Name)
	}
	return pairs
}

func (f *fesl) playerStats(ctx context.Context, self int64, v map[string]string, allTime bool) []string {
	owner, ownerOK := decimal(fallback(v, "OWNER", strconv.FormatInt(self, 10)), maxRecordID)
	keys, keysOK := readKeys(v, "KEYS")
	if !ownerOK || owner == 0 || !keysOK {
		return failure("21")
	}
	if len(keys) == 0 && v["KEY"] != "" {
		key := strings.ToUpper(v["KEY"])
		if !statKey.MatchString(key) {
			return failure("21")
		}
		keys = []string{key}
	}
	rows, err := f.ranking.TSFPStats(ctx, int64(owner), keys, allTime, true)
	if err != nil {
		return failure("99")
	}
	return rankFields(rows)
}

func (f *fesl) topStats(ctx context.Context, v map[string]string, allTime, extras bool) []string {
	key := strings.ToUpper(v["KEY"])
	first, firstOK := decimal(fallback(v, "MINRANK", "1"), maxRecordID)
	last, lastOK := decimal(fallback(v, "MAXRANK", "16"), maxRecordID)
	if !statKey.MatchString(key) || !firstOK || !lastOK {
		return failure("21")
	}
	first = max(1, first)
	if !allTime || last < first {
		return []string{"stats.[]", "0"}
	}
	var keys []string
	if extras {
		for _, name := range []string{"KEYS", "ADDSTATS", "STATS"} {
			if _, exists := v[name+".[]"]; exists {
				var ok bool
				if keys, ok = readKeys(v, name); !ok {
					return failure("21")
				}
				break
			}
		}
	}
	rows, err := f.ranking.TSFPTop(ctx, key, first-1, min(16, last-first+1))
	if err != nil {
		return failure("99")
	}
	pairs := rankFields(rows)
	if len(keys) == 0 {
		return pairs
	}
	for i, row := range rows {
		additional, err := f.ranking.TSFPStats(ctx, row.Persona.ID, keys, true, false)
		if err != nil {
			return failure("99")
		}
		for j, stat := range additional {
			prefix := "stats." + strconv.Itoa(i) + ".addStats." + strconv.Itoa(j) + "."
			pairs = append(pairs, prefix+"key", stat.Key, prefix+"value", stat.Value, prefix+"text", stat.Text)
		}
	}
	return pairs
}
