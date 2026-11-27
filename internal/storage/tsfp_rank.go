package storage

import (
	"context"

	"github.com/jackc/pgx/v5"
)

type TSFPStat struct {
	Key, Value, Text string
	HasText          bool
}

type TSFPRank struct {
	TSFPStat
	Persona Persona
	Rank    int
}

type TSFPRankStore interface {
	ApplyTSFPStats(context.Context, int64, string, uint32, [32]byte, []TSFPStat) error
	ClearTSFPStatReceipts(context.Context, string) error
	TSFPStats(context.Context, int64, []string, bool, bool) ([]TSFPRank, error)
	TSFPTop(context.Context, string, int, int) ([]TSFPRank, error)
}

func (p *Postgres) ApplyTSFPStats(ctx context.Context, owner int64, connection string, code uint32, digest [32]byte,
	updates []TSFPStat) error {
	return pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		result, err := tx.Exec(ctx, `INSERT INTO eanation.tsfp_stat_receipts(connection,code,digest) VALUES ($1,$2,$3)
			ON CONFLICT DO NOTHING`, connection, int64(code), digest[:])
		if err != nil || result.RowsAffected() == 0 {
			return err
		}
		for _, update := range updates {
			if _, err := tx.Exec(ctx, `INSERT INTO eanation.tsfp_stats AS s(persona,key,value,detail)
				VALUES ($1,$2,$3::text::numeric,$4) ON CONFLICT (persona,key) DO UPDATE SET
				value=s.value+excluded.value, detail=CASE WHEN $5 THEN excluded.detail ELSE s.detail END`,
				owner, update.Key, update.Value, update.Text, update.HasText); err != nil {
				return err
			}
		}
		return nil
	})
}

func (p *Postgres) ClearTSFPStatReceipts(ctx context.Context, connection string) error {
	_, err := p.pool.Exec(ctx, "DELETE FROM eanation.tsfp_stat_receipts WHERE connection=$1", connection)
	return err
}

func (p *Postgres) TSFPStats(ctx context.Context, owner int64, keys []string, allTime, ranks bool) ([]TSFPRank, error) {
	rows, err := p.pool.Query(ctx, `SELECT wanted.key,coalesce(s.value::text,'0.0000'),coalesce(s.detail,''),p.id,p.name,
		CASE WHEN NOT $4 OR s.value IS NULL THEN 0 ELSE 1 + (
			SELECT count(*) FROM eanation.tsfp_stats higher JOIN eanation.personas hp ON hp.id=higher.persona
			WHERE higher.key=s.key AND higher.value>s.value AND hp.deleted_at IS NULL
		) END
		FROM eanation.personas p CROSS JOIN unnest($2::text[]) WITH ORDINALITY wanted(key,position)
		LEFT JOIN eanation.tsfp_stats s ON $3 AND s.key=wanted.key AND s.persona=p.id
		WHERE p.id=$1 AND p.deleted_at IS NULL ORDER BY wanted.position`, owner, keys, allTime, ranks)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, readTSFPRank)
}

func (p *Postgres) TSFPTop(ctx context.Context, key string, offset, limit int) ([]TSFPRank, error) {
	rows, err := p.pool.Query(ctx, `SELECT s.key,s.value::text,s.detail,p.id,p.name,RANK() OVER (ORDER BY s.value DESC)
		FROM eanation.tsfp_stats s JOIN eanation.personas p ON p.id=s.persona
		WHERE s.key=$1 AND p.deleted_at IS NULL ORDER BY s.value DESC,p.id OFFSET $2 LIMIT $3`, key, offset, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, readTSFPRank)
}

func readTSFPRank(row pgx.CollectableRow) (TSFPRank, error) {
	var r TSFPRank
	err := row.Scan(&r.Key, &r.Value, &r.Text, &r.Persona.ID, &r.Persona.Name, &r.Rank)
	return r, err
}

var _ TSFPRankStore = (*Postgres)(nil)
