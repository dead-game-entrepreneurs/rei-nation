package nascar04

import (
	"strconv"
	"testing"
	"time"

	"github.com/dead-game-entrepreneurs/rei-nation/internal/storage"
)

func TestRaceLookupAfterRematch(t *testing.T) {
	l := lobby{races: make(map[int64][]*raceSession)}
	host, guest := &player{persona: "Alice", personaID: 1}, &player{persona: "Bob", personaID: 2}
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	first := l.startRace(host, guest, now)
	second := l.startRace(host, guest, now)
	if first.when == second.when {
		t.Fatal("rematch reused an admission timestamp")
	}
	rejoined := &player{personaID: host.personaID}
	if got := l.raceForReport(rejoined, "2026.1.2 3:04:05", now); got != first {
		t.Fatal("after reconnect the retail timestamp did not select the first race")
	}
	if got := l.raceForReport(rejoined, strconv.FormatInt(second.when, 10), now); got != second {
		t.Fatal("the rematch timestamp did not select the second race")
	}
	if got := l.raceForReport(rejoined, "2026.1.2 3:04:05", first.expires); got != nil {
		t.Fatal("expired race remained eligible")
	}
}

func TestReportBindsToRaceAndNormalizesReplay(t *testing.T) {
	race := &raceSession{id: "receipt", names: [2]string{"Alice", "Bob"}}
	f := map[string]string{
		"NAME0": "bob", "NAME1": "ALICE", "SET_RANKED": "1",
		"FINISPOS0": "42", "FINISPOS1": "1", "STARTPOS1": "1", "LAPSLED1": "5", "LAPSCOMP1": "5",
		"WHEN": "123", "RACETIME": "2660",
	}
	report, key, ok := parseReport(race, f)
	rows := report.Rows
	if !ok || rows[0].Name != "Bob" || rows[1].Name != "Alice" {
		t.Fatalf("rows=%+v valid=%v", rows, ok)
	}
	var total storage.RaceTotals
	total.Add(rows[1], rows[0], 10)
	total.Add(storage.RaceRow{DisconnectTime: 200}, rows[0], 101)
	if total.Starts != 2 || total.Wins != 1 || total.Losses != 1 || total.DNFs != 1 || total.Streak != -1 ||
		total.Poles != 1 || total.Led != 5 || total.BestFinish != 1 || total.RaceWins != 1 {
		t.Fatalf("totals=%+v", total)
	}
	f["NAME0"], f["NAME1"] = "Alice", "Bob"
	if _, reversed, ok := parseReport(race, f); !ok || reversed != key {
		t.Fatal("reordered player report got a new fingerprint")
	}
	f["NAME0"] = "Bob"
	if _, _, ok := parseReport(race, f); ok {
		t.Fatal("duplicate persona in malformed guest report accepted")
	}
	f["NAME0"] = "Other"
	if _, _, ok := parseReport(race, f); ok {
		t.Fatal("report awarded a result to a nonparticipant")
	}
}

func TestRetailAbortReport(t *testing.T) {
	for _, key := range []string{"DISCTIME", "DISQTIME"} {
		t.Run(key, func(t *testing.T) {
			race := &raceSession{id: "receipt", names: [2]string{"Alice", "Bob"}}
			f := map[string]string{
				"NAME0": "Alice", "NAME1": "Bob", "SET_RANKED": "1", "WHEN": "2026.11.24 10:15:02",
				"SET_TRACKID": "13", "SET_TRACK": "TALLADEGA%b1", "SET_CAUTIONS": "0", "SET_NUMAI": "41",
				"STARTPOS0": "24", "STARTPOS1": "22", key + "0": "0", key + "1": "2345",
			}
			report, fingerprint, ok := parseReport(race, f)
			if !ok || fingerprint == race.id || report.Elapsed != nil || report.TrackID == nil ||
				*report.TrackID != 13 || report.Track != f["SET_TRACK"] || report.Settings["SET_NUMAI"] != 41 {
				t.Fatalf("abort report: %+v, valid=%v", report, ok)
			}
			var winner, loser storage.RaceTotals
			winner.Add(report.Rows[0], report.Rows[1], 11)
			loser.Add(report.Rows[1], report.Rows[0], 2)
			if winner.Wins != 1 || winner.Top100Wins != 1 || winner.RaceWins != 0 || winner.FinishSamples != 0 ||
				loser.Losses != 1 || loser.DNFs != 1 || loser.Disconnects+loser.Disqualifications != 1 {
				t.Fatalf("abort totals: winner=%+v loser=%+v", winner, loser)
			}
			delete(f, key+"1")
			if _, _, ok := parseReport(race, f); ok {
				t.Fatal("report with neither finish nor abort accepted")
			}
		})
	}
}
