package storage

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

type AccountManagementStore interface {
	UpdateAccount(context.Context, Account, string) (bool, error)
	DeletePersona(context.Context, int64, string) (string, error)
	AccountByMail(context.Context, string) (Account, bool, error)
	ReserveRecovery(context.Context, Account, string) (bool, error)
	RecoveryHash(context.Context, int64) (string, error)
	UseRecovery(context.Context, Account, string) (bool, error)
}

func (p *Postgres) UpdateAccount(ctx context.Context, current Account, hash string) (bool, error) {
	var changed bool
	err := pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		r, err := tx.Exec(ctx, `UPDATE eanation.accounts SET mail=$1,password_hash=$2
			WHERE id=$3 AND password_hash=$4`, current.Mail, hash, current.ID, current.Hash)
		if err != nil || r.RowsAffected() == 0 {
			return conflict(err)
		}
		changed = true
		_, err = tx.Exec(ctx, `DELETE FROM eanation.account_recovery WHERE account_id=$1`, current.ID)
		return err
	})
	return changed && err == nil, err
}

func (p *Postgres) DeletePersona(ctx context.Context, owner int64, name string) (string, error) {
	var deleted string
	err := pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(1249901215)"); err != nil {
			return err
		}
		err := tx.QueryRow(ctx, `UPDATE eanation.personas SET deleted_at=now()
			WHERE account_id=$1 AND lower(name)=lower($2) AND deleted_at IS NULL
			RETURNING name`, owner, name).Scan(&deleted)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		for _, query := range []string{
			"DELETE FROM eanation.buddies WHERE a=$1 OR b=$1",
			"DELETE FROM eanation.ignored WHERE owner=$1 OR target=$1",
			"DELETE FROM eanation.mailbox WHERE sender=$1 OR recipient=$1",
		} {
			if _, err := tx.Exec(ctx, query, deleted); err != nil {
				return err
			}
		}
		return nil
	})
	return deleted, err
}

func (p *Postgres) AccountByMail(ctx context.Context, address string) (Account, bool, error) {
	var a Account
	err := p.pool.QueryRow(ctx, `SELECT id,name,password_hash,mail,loc FROM eanation.accounts
		WHERE mail<>'' AND lower(mail)=lower($1)`, address).Scan(&a.ID, &a.Name, &a.Hash, &a.Mail, &a.Loc)
	if errors.Is(err, pgx.ErrNoRows) {
		return Account{}, false, nil
	}
	return a, err == nil, err
}

func (p *Postgres) ReserveRecovery(ctx context.Context, a Account, hash string) (bool, error) {
	// Hold the account row until the reservation exists so a concurrent edit
	// can invalidate it before any recovery password is accepted.
	var reserved bool
	err := pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		var id int64
		err := tx.QueryRow(ctx, `SELECT id FROM eanation.accounts
			WHERE id=$1 AND mail=$2 AND password_hash=$3 FOR UPDATE`, a.ID, a.Mail, a.Hash).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		r, err := tx.Exec(ctx, `INSERT INTO eanation.account_recovery (account_id,requested_at,password_hash,expires_at)
			VALUES ($1,now(),nullif($2,''),CASE WHEN $2<>'' THEN now()+interval '30 minutes' END)
			ON CONFLICT (account_id) DO UPDATE SET requested_at=excluded.requested_at,
				password_hash=coalesce(excluded.password_hash,account_recovery.password_hash),
				expires_at=coalesce(excluded.expires_at,account_recovery.expires_at)
			WHERE account_recovery.requested_at < now()-interval '5 minutes'`, id, hash)
		reserved = r.RowsAffected() == 1
		return err
	})
	return reserved && err == nil, err
}

func (p *Postgres) RecoveryHash(ctx context.Context, id int64) (string, error) {
	var hash string
	err := p.pool.QueryRow(ctx, `SELECT password_hash FROM eanation.account_recovery
		WHERE account_id=$1 AND expires_at>now()`, id).Scan(&hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return hash, err
}

func (p *Postgres) UseRecovery(ctx context.Context, a Account, hash string) (bool, error) {
	var used bool
	err := pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		// Account edits and recovery reservations also take the account lock
		// before changing the recovery row.
		var id int64
		err := tx.QueryRow(ctx, `SELECT id FROM eanation.accounts
			WHERE id=$1 AND password_hash=$2 FOR UPDATE`, a.ID, a.Hash).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		r, err := tx.Exec(ctx, `UPDATE eanation.account_recovery SET password_hash=NULL,expires_at=NULL
			WHERE account_id=$1 AND password_hash=$2 AND expires_at>now()`, id, hash)
		if err != nil || r.RowsAffected() == 0 {
			return err
		}
		_, err = tx.Exec(ctx, "UPDATE eanation.accounts SET password_hash=$1 WHERE id=$2", hash, id)
		used = err == nil
		return err
	})
	return used && err == nil, err
}

var _ AccountManagementStore = (*Postgres)(nil)
