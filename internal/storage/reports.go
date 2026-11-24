package storage

import (
	"context"

	"github.com/jackc/pgx/v5"
)

type ReportStore interface {
	ReportPlayer(context.Context, string, int64, string, string) (string, error)
	Reputation(context.Context, string, string) (int, error)
}

func (p *Postgres) ReportPlayer(ctx context.Context, title string, account int64, source, target string) (string, error) {
	var canonical string
	err := p.pool.QueryRow(ctx, `SELECT name FROM eanation.personas
		WHERE lower(name)=lower($1) AND account_id<>$2 AND deleted_at IS NULL`, target, account).Scan(&canonical)
	if err != nil {
		// Unknown, deleted, and same-account targets all produce an empty response.
		if err == pgx.ErrNoRows {
			return "", nil
		}
		return "", err
	}
	_, err = p.pool.Exec(ctx, `INSERT INTO eanation.player_reports (title,reporter,source,target)
		SELECT $1,$2,source.name,target.name FROM eanation.personas AS source CROSS JOIN eanation.personas AS target
		WHERE source.name=$3 AND source.account_id=$2 AND source.deleted_at IS NULL
		AND target.name=$4 AND target.deleted_at IS NULL ON CONFLICT DO NOTHING`, title, account, source, canonical)
	return canonical, err
}

func (p *Postgres) Reputation(ctx context.Context, title string, name string) (int, error) {
	var result int
	err := p.pool.QueryRow(ctx, `SELECT greatest(0,5-count(*))
		FROM eanation.player_reports WHERE title=$1 AND target=$2`, title, name).Scan(&result)
	return result, err
}

var _ ReportStore = (*Postgres)(nil)
