package storage

import (
	"context"
	"crypto/sha256"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

const tsfpSchema = `
CREATE TABLE IF NOT EXISTS eanation.tsfp_maps (
	id integer GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
	owner integer NOT NULL REFERENCES eanation.personas(id),
	name_key text NOT NULL UNIQUE,
	digest bytea NOT NULL UNIQUE,
	content bytea NOT NULL,
	metadata jsonb NOT NULL,
	created_at timestamptz NOT NULL DEFAULT now(),
	downloads integer NOT NULL DEFAULT 0,
	rating_sum integer NOT NULL DEFAULT 0,
	reviews integer NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS eanation.tsfp_map_reviews (
	map_id integer NOT NULL REFERENCES eanation.tsfp_maps(id) ON DELETE CASCADE,
	voter integer NOT NULL REFERENCES eanation.personas(id),
	rating integer NOT NULL CHECK (rating BETWEEN 0 AND 5),
	PRIMARY KEY (map_id,voter)
);
CREATE TABLE IF NOT EXISTS eanation.tsfp_feedback (
	id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
	reporter integer NOT NULL REFERENCES eanation.personas(id),
	target_type integer NOT NULL CHECK (target_type IN (1,3)),
	target integer NOT NULL,
	reason integer NOT NULL,
	detail text NOT NULL,
	message text NOT NULL,
	created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS eanation.tsfp_stats (
	persona integer NOT NULL REFERENCES eanation.personas(id),
	key text NOT NULL,
	value numeric(28,4) NOT NULL,
	detail text NOT NULL DEFAULT '',
	PRIMARY KEY (persona,key)
);
CREATE INDEX IF NOT EXISTS tsfp_stats_rank ON eanation.tsfp_stats (key,value DESC);
CREATE TABLE IF NOT EXISTS eanation.tsfp_stat_receipts (
	connection text NOT NULL,
	code bigint NOT NULL,
	digest bytea NOT NULL,
	received_at timestamptz NOT NULL DEFAULT now(),
	PRIMARY KEY (connection,code,digest)
);
CREATE INDEX IF NOT EXISTS tsfp_stat_receipts_expiry ON eanation.tsfp_stat_receipts (received_at);
`

type TSFPMapMeta struct {
	Name, Creator, Version, ShortDescription, LongDescription, Locale, FormatType, IconID string
	Attributes                                                                            [][2]string
}

type TSFPMap struct {
	ID, Owner                int64
	Meta                     TSFPMapMeta
	Created                  time.Time
	Size, Downloads, Reviews int
	Rating                   float64
}

type TSFPFeedback struct {
	Reporter, Target   int64
	TargetType, Reason int
	Detail, Message    string
}

type TSFPStore interface {
	CreateTSFPMap(context.Context, int64, string, TSFPMapMeta, []byte) (int64, error)
	TSFPMap(context.Context, int64) (TSFPMap, bool, error)
	TSFPMaps(context.Context) ([]TSFPMap, error)
	DownloadTSFPMap(context.Context, int64) ([]byte, bool, error)
	DeleteTSFPMap(context.Context, int64, int64) (bool, error)
	RateTSFPMap(context.Context, int64, int64, int) (bool, error)
	RecordTSFPFeedback(context.Context, TSFPFeedback) (bool, error)
}

const tsfpMapFields = `id,owner,metadata,created_at,octet_length(content),downloads,reviews,
	CASE WHEN reviews=0 THEN 0 ELSE rating_sum::double precision/reviews END`

func (p *Postgres) CreateTSFPMap(ctx context.Context, owner int64, nameKey string, meta TSFPMapMeta, content []byte) (int64, error) {
	digest := sha256.Sum256(content)
	var id int64
	err := p.pool.QueryRow(ctx, `INSERT INTO eanation.tsfp_maps(owner,name_key,digest,content,metadata)
		VALUES ($1,$2,$3,$4,$5) RETURNING id`, owner, nameKey, digest[:], content, meta).Scan(&id)
	return id, conflict(err)
}

func scanTSFPMap(row pgx.Row) (TSFPMap, error) {
	var m TSFPMap
	err := row.Scan(&m.ID, &m.Owner, &m.Meta, &m.Created, &m.Size, &m.Downloads, &m.Reviews, &m.Rating)
	return m, err
}

func (p *Postgres) TSFPMap(ctx context.Context, id int64) (TSFPMap, bool, error) {
	m, err := scanTSFPMap(p.pool.QueryRow(ctx, `SELECT `+tsfpMapFields+` FROM eanation.tsfp_maps WHERE id=$1`, id))
	return m, err == nil, ignoreMissing(err)
}

func (p *Postgres) TSFPMaps(ctx context.Context) ([]TSFPMap, error) {
	rows, err := p.pool.Query(ctx, `SELECT `+tsfpMapFields+` FROM eanation.tsfp_maps ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (TSFPMap, error) { return scanTSFPMap(row) })
}

func (p *Postgres) DownloadTSFPMap(ctx context.Context, id int64) ([]byte, bool, error) {
	var data []byte
	err := p.pool.QueryRow(ctx, `UPDATE eanation.tsfp_maps SET downloads=downloads+1 WHERE id=$1 RETURNING content`,
		id).Scan(&data)
	return data, err == nil, ignoreMissing(err)
}

func (p *Postgres) DeleteTSFPMap(ctx context.Context, owner, id int64) (bool, error) {
	result, err := p.pool.Exec(ctx, "DELETE FROM eanation.tsfp_maps WHERE id=$1 AND owner=$2", id, owner)
	return err == nil && result.RowsAffected() == 1, err
}

func (p *Postgres) RateTSFPMap(ctx context.Context, voter, id int64, rating int) (bool, error) {
	err := pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		var mapID int64
		if err := tx.QueryRow(ctx, "SELECT id FROM eanation.tsfp_maps WHERE id=$1 FOR UPDATE", id).Scan(&mapID); err != nil {
			return err
		}
		var previous int
		err := tx.QueryRow(ctx, "SELECT rating FROM eanation.tsfp_map_reviews WHERE map_id=$1 AND voter=$2", id, voter).Scan(&previous)
		reviews := 0
		if errors.Is(err, pgx.ErrNoRows) {
			reviews = 1
		} else if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO eanation.tsfp_map_reviews(map_id,voter,rating) VALUES ($1,$2,$3)
			ON CONFLICT (map_id,voter) DO UPDATE SET rating=excluded.rating`, id, voter, rating); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE eanation.tsfp_maps SET rating_sum=rating_sum+$2,reviews=reviews+$3 WHERE id=$1`,
			id, rating-previous, reviews)
		return err
	})
	return err == nil, ignoreMissing(err)
}

func (p *Postgres) RecordTSFPFeedback(ctx context.Context, r TSFPFeedback) (bool, error) {
	result, err := p.pool.Exec(ctx, `INSERT INTO eanation.tsfp_feedback(reporter,target_type,target,reason,detail,message)
		SELECT $1,$2,$3,$4,$5,$6 WHERE
		($2=1 AND EXISTS (SELECT 1 FROM eanation.personas WHERE id=$3 AND deleted_at IS NULL)) OR
		($2=3 AND EXISTS (SELECT 1 FROM eanation.tsfp_maps WHERE id=$3))`,
		r.Reporter, r.TargetType, r.Target, r.Reason, r.Detail, r.Message)
	return err == nil && result.RowsAffected() == 1, err
}

func ignoreMissing(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	return err
}

var _ TSFPStore = (*Postgres)(nil)
