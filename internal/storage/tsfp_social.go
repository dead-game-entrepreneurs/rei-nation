package storage

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

const tsfpTitle = "tsfp"

type TSFPBuddy struct {
	Persona
	State string
}

type TSFPSocialStore interface {
	MessengerStore
	TSFPPersona(context.Context, string) (Persona, bool, error)
	TSFPSearch(context.Context, string, int) ([]Persona, error)
	TSFPRoster(context.Context, string) ([]TSFPBuddy, error)
	TSFPFriendship(context.Context, string, string, string) (previous, current string, err error)
}

func (p *Postgres) TSFPPersona(ctx context.Context, name string) (Persona, bool, error) {
	var persona Persona
	err := p.pool.QueryRow(ctx, `SELECT id,name FROM eanation.personas WHERE lower(name)=lower($1) AND deleted_at IS NULL`,
		name).Scan(&persona.ID, &persona.Name)
	if errors.Is(err, pgx.ErrNoRows) {
		return Persona{}, false, nil
	}
	return persona, err == nil, err
}

func (p *Postgres) TSFPSearch(ctx context.Context, pattern string, limit int) ([]Persona, error) {
	rows, err := p.pool.Query(ctx, `SELECT id,name FROM eanation.personas
		WHERE deleted_at IS NULL AND lower(name) LIKE lower($1) ORDER BY lower(name) LIMIT $2`, pattern, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[Persona])
}

func (p *Postgres) TSFPRoster(ctx context.Context, self string) ([]TSFPBuddy, error) {
	rows, err := p.pool.Query(ctx, `SELECT p.id,p.name,CASE
		WHEN b.accepted THEN 'S' WHEN b.inviter=$2 THEN 'T' ELSE 'R' END
		FROM eanation.buddies b JOIN eanation.personas p ON p.name=CASE WHEN b.a=$2 THEN b.b ELSE b.a END
		WHERE b.title=$1 AND (b.a=$2 OR b.b=$2) AND p.deleted_at IS NULL ORDER BY lower(p.name) LIMIT 100`, tsfpTitle, self)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (TSFPBuddy, error) {
		var b TSFPBuddy
		err := row.Scan(&b.ID, &b.Name, &b.State)
		return b, err
	})
}

func (p *Postgres) TSFPFriendship(ctx context.Context, self, other, operation string) (previous, current string, err error) {
	if self == other {
		return "", "", nil
	}
	a, b := min(self, other), max(self, other)
	err = pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(1249901215)"); err != nil {
			return err
		}
		var inviter string
		var accepted bool
		err := tx.QueryRow(ctx, "SELECT inviter,accepted FROM eanation.buddies WHERE title=$1 AND a=$2 AND b=$3",
			tsfpTitle, a, b).Scan(&inviter, &accepted)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			err = nil
		case err != nil:
			return err
		case accepted:
			previous = "S"
		case inviter == self:
			previous = "T"
		default:
			previous = "R"
		}
		current = previous
		switch {
		case operation == "request" && previous == "":
			allowed, err := contact(ctx, tx, tsfpTitle, self, other)
			if !allowed || err != nil {
				return err
			}
			result, err := tx.Exec(ctx, `INSERT INTO eanation.buddies(title,a,b,inviter)
				SELECT $1,$2,$3,$4 WHERE NOT EXISTS (
				SELECT 1 FROM (SELECT count(*) n FROM eanation.buddies WHERE title=$1 AND (a=$2 OR b=$2)
				UNION ALL SELECT count(*) FROM eanation.buddies WHERE title=$1 AND (a=$3 OR b=$3)) counts WHERE n>=100)`,
				tsfpTitle, a, b, self)
			if err == nil && result.RowsAffected() == 1 {
				current = "T"
			}
			return err
		case previous == "R" && (operation == "accept" || operation == "request"):
			_, err = tx.Exec(ctx, "UPDATE eanation.buddies SET accepted=true WHERE title=$1 AND a=$2 AND b=$3", tsfpTitle, a, b)
			current = "S"
		case previous != "" && (operation == "remove" || operation == "decline" && previous == "R"):
			_, err = tx.Exec(ctx, "DELETE FROM eanation.buddies WHERE title=$1 AND a=$2 AND b=$3", tsfpTitle, a, b)
			current = ""
		}
		return err
	})
	return
}

var _ TSFPSocialStore = (*Postgres)(nil)
