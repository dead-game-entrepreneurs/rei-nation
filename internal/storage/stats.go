package storage

import (
	"context"

	"github.com/jackc/pgx/v5"
)

type RaceRow struct {
	Name                             string
	Start, Finish, Laps, Led         int64
	DisconnectTime, DisqualifiedTime int64
}

func (r RaceRow) DNF() bool { return r.DisconnectTime > 0 || r.DisqualifiedTime > 0 }

type RaceReport struct {
	Rows     [2]RaceRow
	Ranked   bool
	Reporter string
	When     string
	Elapsed  *int64
	TrackID  *int32
	Track    string
	Settings map[string]int64
}

type RaceTotals struct {
	Starts, Wins, Losses, DNFs, RaceWins, Top5, Top10, Poles       int64
	Laps, Led, SumStart, SumFinish, Streak, BestFinish             int64
	StartSamples, FinishSamples, Disconnects, Disqualifications    int64
	OpponentRankSum, OpponentRankSamples, Top100Wins, Top100Losses int64
}

type Ranked struct {
	Name string
	Rank int64
	RaceTotals
}

type TrackRank struct {
	ID          int32
	Rank, Races int64
}

type RaceStore interface {
	RecordRace(context.Context, string, string, string, RaceReport) (bool, error)
	RaceStats(context.Context, string, string, string, int, int) ([]Ranked, error)
	PopularTracks(context.Context, string, int, int) ([]TrackRank, error)
}

func (p *Postgres) RecordRace(ctx context.Context, title, id, fingerprint string, report RaceReport) (bool, error) {
	var recorded bool
	err := pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(1249901216)"); err != nil {
			return err
		}
		r, err := tx.Exec(ctx, `INSERT INTO eanation.race_receipts (title,race_id,fingerprint)
			VALUES ($1,$2,$3) ON CONFLICT DO NOTHING`, title, id, fingerprint)
		if err != nil || r.RowsAffected() == 0 {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO eanation.race_details (title,race_id,track_id,report)
			VALUES ($1,$2,$3,$4)`, title, id, report.TrackID, report); err != nil {
			return err
		}
		recorded = true
		if !report.Ranked {
			return nil
		}
		// Read both ranks before either result changes the standings.
		names := []string{report.Rows[0].Name, report.Rows[1].Name}
		rows, err := tx.Query(ctx, raceBoard+`SELECT persona,rank,totals FROM board WHERE persona=ANY($3)`,
			title, "", names)
		if err != nil {
			return err
		}
		prior, err := pgx.CollectRows(rows, readRank)
		if err != nil {
			return err
		}
		var participants [2]Ranked
		for _, entry := range prior {
			if entry.Name == names[0] {
				participants[0] = entry
			} else {
				participants[1] = entry
			}
		}
		for i, row := range report.Rows {
			totals := participants[i].RaceTotals
			totals.Add(row, report.Rows[1-i], participants[1-i].Rank)
			if _, err := tx.Exec(ctx, `INSERT INTO eanation.race_stats(title,persona,totals) VALUES ($1,$2,$3)
				ON CONFLICT (title,persona) DO UPDATE SET totals=excluded.totals`, title, row.Name, totals); err != nil {
				return err
			}
		}
		return nil
	})
	return recorded && err == nil, err
}

func (s RaceTotals) PositionSamples() (starts, finishes int64) {
	starts, finishes = s.StartSamples, s.FinishSamples
	// Totals written before race history had no sample counts.
	if starts == 0 && s.SumStart > 0 {
		starts = s.Starts
	}
	if finishes == 0 && s.SumFinish > 0 {
		finishes = max(1, s.Starts-s.DNFs)
	}
	return starts, finishes
}

func (s *RaceTotals) Add(row, peer RaceRow, opponentRank int64) {
	s.StartSamples, s.FinishSamples = s.PositionSamples()
	s.Starts++
	s.Laps += row.Laps
	s.Led += row.Led
	if opponentRank > 0 {
		s.OpponentRankSamples++
		s.OpponentRankSum += opponentRank
	}
	switch {
	case row.DNF() && !peer.DNF(), !row.DNF() && !peer.DNF() && row.Finish > peer.Finish && peer.Finish > 0:
		s.Losses++
		s.Streak = min(0, s.Streak) - 1
		if opponentRank > 0 && opponentRank <= 100 {
			s.Top100Losses++
		}
	case !row.DNF() && peer.DNF(), !row.DNF() && !peer.DNF() && row.Finish < peer.Finish && row.Finish > 0:
		s.Wins++
		s.Streak = max(0, s.Streak) + 1
		if opponentRank > 0 && opponentRank <= 100 {
			s.Top100Wins++
		}
	}
	if row.DNF() {
		s.DNFs++
	}
	if row.DisconnectTime > 0 {
		s.Disconnects++
	}
	if row.DisqualifiedTime > 0 {
		s.Disqualifications++
	}
	if row.Start > 0 {
		s.SumStart += row.Start
		s.StartSamples++
		if row.Start == 1 {
			s.Poles++
		}
	}
	if row.Finish > 0 && !row.DNF() {
		s.SumFinish += row.Finish
		s.FinishSamples++
		if s.BestFinish == 0 || row.Finish < s.BestFinish {
			s.BestFinish = row.Finish
		}
		if row.Finish == 1 {
			s.RaceWins++
		}
		if row.Finish <= 5 {
			s.Top5++
		}
		if row.Finish <= 10 {
			s.Top10++
		}
	}
}

const raceBoard = `WITH board AS (
	SELECT persona,totals,row_number() OVER (ORDER BY
		coalesce((totals->>$2::text)::bigint,0) DESC,
		CASE $2 WHEN 'Top100Wins' THEN coalesce((totals->>'Top100Losses')::bigint,0)
			WHEN 'RaceWins' THEN -(totals->>'Top5')::bigint ELSE 0 END ASC,
		(totals->>'Wins')::bigint DESC, (totals->>'Losses')::bigint ASC, persona ASC) AS rank
	FROM eanation.race_stats JOIN eanation.personas ON name=persona
	WHERE title=$1 AND deleted_at IS NULL
)`

func readRank(row pgx.CollectableRow) (Ranked, error) {
	var r Ranked
	err := row.Scan(&r.Name, &r.Rank, &r.RaceTotals)
	return r, err
}

func (p *Postgres) RaceStats(ctx context.Context, title, metric, name string, start, limit int) ([]Ranked, error) {
	rows, err := p.pool.Query(ctx, raceBoard+`SELECT persona,rank,totals FROM board
		WHERE $3='' OR lower(persona)=lower($3) ORDER BY rank LIMIT $4 OFFSET $5`, title, metric, name, limit, start)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, readRank)
}

func (p *Postgres) PopularTracks(ctx context.Context, title string, start, limit int) ([]TrackRank, error) {
	rows, err := p.pool.Query(ctx, `SELECT track_id,row_number() OVER (ORDER BY count(*) DESC,track_id),count(*)
		FROM eanation.race_details WHERE title=$1 AND track_id IS NOT NULL GROUP BY track_id
		ORDER BY count(*) DESC,track_id LIMIT $2 OFFSET $3`, title, limit, start)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[TrackRank])
}

var _ RaceStore = (*Postgres)(nil)
