package storage

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
)

// ProfileStore keeps the registration details a title shows on its account
// screen (date of birth, mail preferences) alongside the shared account.
type ProfileStore interface {
	AccountProfile(context.Context, int64) (map[string]string, error)
	UpdateAccountProfile(context.Context, int64, string, map[string]string) error
}

func (p *Postgres) AccountProfile(ctx context.Context, id int64) (map[string]string, error) {
	var raw []byte
	err := p.pool.QueryRow(ctx, "SELECT details FROM eanation.account_profile WHERE account_id=$1", id).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	details := map[string]string{}
	return details, json.Unmarshal(raw, &details)
}

// UpdateAccountProfile merges details into the account's profile and, when
// mail is not empty, changes the account's email. A mail address another
// account uses returns ErrConflict.
func (p *Postgres) UpdateAccountProfile(ctx context.Context, id int64, mail string, details map[string]string) error {
	raw, err := json.Marshal(details)
	if err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		if mail != "" {
			if _, err := tx.Exec(ctx, "UPDATE eanation.accounts SET mail=$1 WHERE id=$2", mail, id); err != nil {
				return conflict(err)
			}
		}
		_, err := tx.Exec(ctx, `INSERT INTO eanation.account_profile (account_id, details) VALUES ($1, $2)
			ON CONFLICT (account_id) DO UPDATE SET details = eanation.account_profile.details || EXCLUDED.details`, id, raw)
		return err
	})
}
