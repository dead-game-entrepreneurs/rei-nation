package nascar04

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/dead-game-entrepreneurs/rei-nation/internal/protocol/aries"
	"github.com/dead-game-entrepreneurs/rei-nation/internal/storage"
)

type raceSession struct {
	id           string
	names        [2]string
	when         int64
	expires      time.Time
	release      []func()
	releaseTimer *time.Timer
}

const raceReportRelayGrace = 90 * time.Second

// startRace and raceForReport use the lobby lock. A match outlives its lobby sockets.
func (l *lobby) startRace(a, b *player, now time.Time) *raceSession {
	for persona, races := range l.races {
		active := slices.DeleteFunc(races, func(race *raceSession) bool {
			if now.Before(race.expires) {
				return false
			}
			race.stop()
			return true
		})
		if len(active) == 0 {
			delete(l.races, persona)
		} else {
			l.races[persona] = active
		}
	}
	when := now.Unix()
	for _, p := range [2]*player{a, b} {
		if recent := l.races[p.personaID]; len(recent) > 0 {
			previous := recent[len(recent)-1]
			previous.stop()
			// WHEN identifies the match even when the same pair immediately rematches.
			when = max(when, previous.when+1)
		}
	}
	var id [16]byte
	rand.Read(id[:])
	race := &raceSession{id: hex.EncodeToString(id[:]), names: [2]string{a.persona, b.persona},
		when: when, expires: now.Add(2 * time.Hour)}
	for _, p := range [2]*player{a, b} {
		l.races[p.personaID] = append(l.races[p.personaID], race)
		p.race = race
	}
	return race
}

func (l *lobby) raceForReport(p *player, field string, now time.Time) *raceSession {
	if field == "" {
		if p.race != nil && now.Before(p.race.expires) {
			return p.race
		}
		return nil
	}
	when, err := strconv.ParseInt(field, 10, 64)
	if err != nil {
		// The disc formats the +ses Unix time in UTC without zero padding on the date.
		date, err := time.Parse("2006.1.2 15:04:05", field)
		if err != nil {
			return nil
		}
		when = date.Unix()
	}
	for _, race := range l.races[p.personaID] {
		if race.when == when && now.Before(race.expires) {
			return race
		}
	}
	return nil
}

// Called with the lobby lock. The other participant can still report a result.
func (r *raceSession) stop() {
	if r.releaseTimer != nil {
		r.releaseTimer.Stop()
		r.releaseTimer = nil
	}
	for _, release := range r.release {
		release()
	}
	r.release = nil
}

// Called with the lobby lock. The first report can arrive while the other driver still needs UDP.
func (l *lobby) releaseRaceAfterReport(race *raceSession) {
	if len(race.release) == 0 || race.releaseTimer != nil {
		return
	}
	race.releaseTimer = time.AfterFunc(raceReportRelayGrace, func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		race.stop()
	})
}

func (l *lobby) raceCommand(ctx context.Context, p *player, tag string, f map[string]string) {
	l.mu.Lock()
	live := p.persona != "" && l.online[strings.ToLower(p.persona)] == p
	name := p.persona
	var race *raceSession
	if live && tag == "rank" {
		race = l.raceForReport(p, nameValue(f["WHEN"]), time.Now())
	}
	l.mu.Unlock()
	if !live {
		p.client.Send(aries.Reply(tag+"maut", nil))
		return
	}
	switch tag {
	case "rank":
		p.client.Send(aries.Reply("rank", nil))
		if l.stats == nil {
			return
		}
		if race == nil {
			l.opts.Log.Info("NASCAR result ignored", "reason", "no matching race")
			return
		}
		if reporter := nameValue(f["REPT"]); reporter != "" && !strings.EqualFold(reporter, name) {
			l.opts.Log.Info("NASCAR result ignored", "reason", "reporter differs from lobby persona")
			return
		}
		report, fingerprint, ok := parseReport(race, f)
		if !ok {
			l.opts.Log.Info("NASCAR result ignored", "reason", "invalid result fields")
			return
		}
		report.Reporter = name
		recorded, err := l.stats.RecordRace(ctx, gameKey, race.id, fingerprint, report)
		if err != nil {
			l.opts.Log.Warn("could not persist NASCAR race", "error_type", fmt.Sprintf("%T", err))
		}
		if recorded {
			l.mu.Lock()
			l.releaseRaceAfterReport(race)
			l.mu.Unlock()
			l.opts.Log.Info("NASCAR result recorded", "race_id", race.id, "ranked", report.Ranked)
			if report.Ranked {
				l.refreshRaceStats(ctx, race)
			}
		}
	case "cate":
		p.client.Send(aries.Reply("cate", map[string]string{
			"CC": "1", "IC": "1", "VC": "1", "SYMS": "\"TOP 100\"", "SS": "8", "R": "0,1,1,1,1,1,1,1",
		}))
	case "sviw":
		p.client.Send(aries.Reply("sviw", map[string]string{
			"N": "8", "NAMES": "Rank,Wins,Losses,Races,LapsLed,Best,DNF,Points",
			"DESCS":  "Rank,Wins,Losses,Races,\"Laps led\",\"Best finish\",\"DNF %\",Points",
			"PARAMS": "2,2,2,2,2,2,2,2", "WIDTHS": "5,5,5,5,5,5,5,5",
		}))
	case "snap":
		l.snapshot(ctx, p, name, f)
	}
}

func parseReport(race *raceSession, f map[string]string) (storage.RaceReport, string, bool) {
	var report storage.RaceReport
	if f["SET_RANKED"] != "1" && f["SET_RANKED"] != "0" {
		return report, "", false
	}
	report.Ranked = f["SET_RANKED"] == "1"
	for i := range report.Rows {
		suffix := strconv.Itoa(i)
		name := nameValue(f["NAME"+suffix])
		row := &report.Rows[i]
		switch {
		case strings.EqualFold(name, race.names[0]):
			row.Name = race.names[0]
		case strings.EqualFold(name, race.names[1]):
			row.Name = race.names[1]
		default:
			return report, "", false
		}
		row.Start = boundedNumber(f["STARTPOS"+suffix], 1000)
		row.Finish = boundedNumber(f["FINISPOS"+suffix], 1000)
		row.Laps = boundedNumber(f["LAPSCOMP"+suffix], 10000)
		row.Led = boundedNumber(f["LAPSLED"+suffix], 10000)
		row.DisconnectTime = boundedNumber(f["DISCTIME"+suffix], 1<<32-1)
		row.DisqualifiedTime = boundedNumber(f["DISQTIME"+suffix], 1<<32-1)
	}
	a, b := report.Rows[0], report.Rows[1]
	aborted := a.DNF() || b.DNF()
	if a.Name == b.Name || !aborted && (a.Finish == 0 || b.Finish == 0) {
		return report, "", false
	}
	if when := f["WHEN"]; wireText(when, 64) {
		report.When = when
	}
	if elapsed, ok := wireNumber(f["RACETIME"], 1<<32-1); ok {
		report.Elapsed = &elapsed
	}
	if track, ok := wireNumber(f["SET_TRACKID"], 65535); ok {
		id := int32(track)
		report.TrackID = &id
	}
	if track := nameValue(f["SET_TRACK"]); wireText(track, 80) {
		report.Track = strings.ToValidUTF8(track, "�")
	}
	report.Settings = make(map[string]int64)
	for _, key := range []string{"SET_DAMAGE", "SET_SETUPS", "SET_NUMAI", "SET_ASSISTS", "SET_CAUTIONS",
		"SET_RACELEN", "SET_CONSUME", "SET_AIDIFF"} {
		if value, ok := wireNumber(f[key], 255); ok {
			report.Settings[key] = value
		}
	}
	// Retain the original fingerprint for completed races, including old receipts.
	// Abort reports omit RACETIME, but still carry the lobby's shared WHEN.
	fingerprint := race.id
	if report.When != "" && (report.Elapsed != nil || aborted) {
		elapsed := f["RACETIME"]
		if report.Elapsed == nil {
			elapsed = "abort"
		}
		sum := sha256.Sum256([]byte(strings.Join([]string{min(a.Name, b.Name), max(a.Name, b.Name), report.When, elapsed}, "\x00")))
		fingerprint = hex.EncodeToString(sum[:])
	}
	return report, fingerprint, true
}

func boundedNumber(s string, limit int64) int64 {
	n, _ := wireNumber(s, limit)
	return n
}

func wireNumber(s string, limit int64) (int64, bool) {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 0 || n > limit {
		return 0, false
	}
	return n, true
}

func (l *lobby) playerStats(ctx context.Context, name string) storage.Ranked {
	if l.stats != nil {
		if rows, err := l.stats.RaceStats(ctx, gameKey, "", name, 0, 1); err == nil && len(rows) == 1 {
			return rows[0]
		}
	}
	return storage.Ranked{}
}

func (l *lobby) refreshRaceStats(ctx context.Context, race *raceSession) {
	for _, name := range race.names {
		row := l.playerStats(ctx, name)
		l.mu.Lock()
		if p := l.online[strings.ToLower(name)]; p != nil && row.Starts >= p.stats.Starts {
			p.stats = row
			sendPlayerState(p)
			if p.room != nil {
				for viewer := range p.room.members {
					sendRoomMember(viewer, p)
				}
			}
		}
		l.mu.Unlock()
	}
}

// The retail category table at 0x383368 uses two index ranges. Both
// contain the same categories and draw from the same recorded race data.
var statMetrics = map[string]string{
	"1": "Wins", "17": "Wins",
	"2": "Top100Wins", "18": "Top100Wins",
	"4": "RaceWins", "20": "RaceWins",
	"5": "Top10", "21": "Top10",
	"7": "Laps", "23": "Laps",
	"8": "Led", "24": "Led",
	"9": "Starts", "25": "Starts",
}

func (l *lobby) snapshot(ctx context.Context, p *player, name string, f map[string]string) {
	chanID, index, seq := f["CHAN"], f["INDEX"], f["SEQN"]
	if chanID != "4" && chanID != "5" && chanID != "6" && chanID != "7" {
		p.client.Send(aries.Reply("snapunsp", nil))
		return
	}
	if _, ok := wireNumber(index, 255); !ok {
		index = "0"
	}
	if !wireText(seq, 32) {
		seq = ""
	}
	start := int(boundedNumber(f["START"], 8191))
	limit := 100
	if n, ok := wireNumber(f["RANGE"], 100); ok {
		limit = int(n)
	}
	if index == "0" && chanID == "5" {
		limit = min(limit, max(0, 100-start))
	}
	filter := nameValue(f["FIND"])
	if filter == "" && chanID == "4" {
		filter = name
	}
	if chanID != "5" {
		start, limit = 0, 1
		if !personaName.MatchString(filter) {
			limit = 0
		}
	} else {
		filter = ""
	}

	var results []map[string]string
	trackBoard := index == "33" || index == "34"
	const columns = 24
	count := columns
	if trackBoard {
		count = 1
	}
	if l.stats != nil && limit > 0 {
		if trackBoard && chanID == "5" {
			tracks, err := l.stats.PopularTracks(ctx, gameKey, start, limit)
			if err != nil {
				l.storeFailure(p.client, "snap", err)
				return
			}
			for _, track := range tracks {
				results = append(results, map[string]string{
					"N": strconv.FormatInt(int64(track.ID), 10), "R": strconv.FormatInt(track.Rank, 10),
					"P": strconv.FormatInt(track.Races, 10), "O": "0", "S": strconv.FormatInt(track.Races, 16),
				})
			}
		} else if !trackBoard {
			metric := statMetrics[index]
			if chanID == "7" {
				metric = ""
			}
			rows, err := l.stats.RaceStats(ctx, gameKey, metric, filter, start, limit)
			if err != nil {
				l.storeFailure(p.client, "snap", err)
				return
			}
			for _, row := range rows {
				results = append(results, map[string]string{
					"N": row.Name, "R": rankField(row), "P": strconv.FormatInt(points(row.RaceTotals), 10),
					"O": "0", "S": statsCSV(row.RaceTotals, metric == "Top100Wins"),
				})
			}
		}
	}
	fields := map[string]string{
		"CHAN": chanID, "INDEX": index, "START": strconv.Itoa(start), "RANGE": strconv.Itoa(len(results)),
		"SEQN": seq, "CC": strconv.Itoa(count), "FC": "1", "DESC": "", "PARAMS": strings.Repeat("1,", count-1) + "1",
	}
	for i := range count {
		col := strconv.Itoa(i)
		fields["CN"+col], fields["CD"+col] = "C"+col, "C"+col
	}
	p.client.Send(aries.Reply("snap", fields))
	for _, row := range results {
		p.client.Send(aries.Reply("+snp", row))
	}
}

func points(s storage.RaceTotals) int64 { return s.Wins * 10 }

func statsCSV(s storage.RaceTotals, vsTop100 bool) string {
	var values [24]int64
	starts, finishes := s.PositionSamples()
	wins, losses := s.Wins, s.Losses
	if vsTop100 {
		wins, losses = s.Top100Wins, s.Top100Losses
	}
	// SLUS-20824 0x290838 decodes this CSV into the stat card at 0x42c480.
	// Its percentage formula counts disconnected drivers separately from starts.
	values[2], values[4], values[5], values[6], values[7] = points(s), s.RaceWins, s.Top5, s.Top10, s.Poles
	values[8], values[9], values[10] = s.Laps, s.Led, s.Starts-s.Disconnects
	values[11], values[12] = percent(s.SumStart, starts), percent(s.SumFinish, finishes)
	values[13] = percent(s.OpponentRankSum, s.OpponentRankSamples)
	values[14], values[15], values[16] = wins, losses, s.Streak
	values[17], values[18] = s.Disconnects, s.Disqualifications
	values[20], values[21], values[22] = s.BestFinish, s.Top100Wins, s.Top100Losses
	var csv [len(values)]string
	for i, value := range values {
		csv[i] = strconv.FormatInt(value, 16)
	}
	return strings.Join(csv[:], ",")
}

func percent(count, total int64) int64 {
	if total == 0 {
		return 0
	}
	return (count*100 + total/2) / total
}

func rankField(s storage.Ranked) string {
	if s.Rank == 0 {
		return ""
	}
	return strconv.FormatInt(s.Rank, 10)
}

func lobbyRecord(s storage.Ranked) string {
	return "," + strconv.FormatInt(s.Wins, 16) + "," + strconv.FormatInt(s.Losses, 16) +
		",," + strconv.FormatInt(min(100, percent(s.DNFs, s.Starts)), 16) + ",,,,,"
}
