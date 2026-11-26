package gera

import (
	"crypto/rand"
	"encoding/binary"
	"strconv"
	"strings"

	"github.com/dead-game-entrepreneurs/rei-nation/internal/protocol/aries"
	"github.com/dead-game-entrepreneurs/rei-nation/internal/protocol/fesl"
)

// handleRank answers the FESL rank service. The client reads ranked stats when
// it enters a match and reports results when one ends; an unanswered request
// leaves a screen waiting on its timeout. Stats are not stored yet, so reads
// return empty lists and reports are accepted and discarded. This follows the
// TimeSplitters server, which uses the same FESL library.
func (s *server) handleRank(c *aries.Client, m fesl.Message) {
	a := s.session(c)
	txn := strings.ToLower(m.TXN)
	s.opts.Log.Info("rank request", "conn", c.ID(), "txn", m.TXN, "persona_id", a.personaID)
	switch txn {
	case "getsessionid":
		if a.rankSession == 0 {
			a.rankSession = newRankSession()
		}
		c.Send(fesl.Reply(m, m.TXN, fesl.Field{Key: "sessionId", Value: strconv.FormatUint(uint64(a.rankSession), 10)}))
	case "startreport", "endreport", "addgameinfo", "updatestats":
		c.Send(fesl.Reply(m, m.TXN))
	case "getstats", "getrankedstats", "gettopn", "gettopnandstats":
		if a.personaID == 0 {
			s.acctError(c, m, errNoSuchUser)
			return
		}
		c.Send(fesl.Reply(m, m.TXN, fesl.Field{Key: "stats.[]", Value: "0"}))
	default:
		// Answer everything so a leaderboard screen does not wait out its timeout.
		s.acctError(c, m, errMissingField)
	}
}

// newRankSession returns a positive 31-bit session identifier.
func newRankSession() uint32 {
	var b [4]byte
	rand.Read(b[:])
	return binary.BigEndian.Uint32(b[:])%0x7ffffffe + 1
}
