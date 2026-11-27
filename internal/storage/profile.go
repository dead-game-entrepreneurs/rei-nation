package storage

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/jackc/pgx/v5"
)

// ProfileStore keeps the account-screen details (date of birth, mail
// preferences) alongside the shared account.
type ProfileStore interface {
	AccountProfile(context.Context, int64) (map[string]string, error)
	UpdateAccountProfile(context.Context, int64, string, map[string]string) error
	UpdateAccountSettings(context.Context, Account, string, string, map[string]string) (bool, error)
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

// UpdateAccountProfile merges details into the profile and sets a non-empty mail.
// A mail address another account uses returns ErrConflict.
func (p *Postgres) UpdateAccountProfile(ctx context.Context, id int64, mail string, details map[string]string) error {
	_, err := p.UpdateAccountSettings(ctx, Account{ID: id}, "", mail, details)
	return err
}

// UpdateAccountSettings applies a non-empty password hash, mail and profile
// details in one transaction. It returns false when current.Hash is stale.
func (p *Postgres) UpdateAccountSettings(ctx context.Context, current Account, hash, mail string,
	details map[string]string) (bool, error) {
	raw, err := json.Marshal(details)
	if err != nil {
		return false, err
	}
	errStale := errors.New("account changed")
	err = pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		if hash != "" {
			r, err := tx.Exec(ctx, `UPDATE eanation.accounts SET password_hash=$1 WHERE id=$2 AND password_hash=$3`,
				hash, current.ID, current.Hash)
			if err != nil {
				return err
			}
			if r.RowsAffected() == 0 {
				return errStale
			}
			if _, err := tx.Exec(ctx, `DELETE FROM eanation.account_recovery WHERE account_id=$1`, current.ID); err != nil {
				return err
			}
		}
		if mail != "" {
			if _, err := tx.Exec(ctx, "UPDATE eanation.accounts SET mail=$1 WHERE id=$2", mail, current.ID); err != nil {
				return conflict(err)
			}
		}
		_, err := tx.Exec(ctx, `INSERT INTO eanation.account_profile (account_id, details) VALUES ($1, $2)
			ON CONFLICT (account_id) DO UPDATE SET details = eanation.account_profile.details || EXCLUDED.details`, current.ID, raw)
		return err
	})
	if errors.Is(err, errStale) {
		return false, nil
	}
	return err == nil, err
}
