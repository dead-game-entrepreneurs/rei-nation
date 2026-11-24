// Package storage owns durable state shared by the enabled game adapters.
package storage

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrConflict = errors.New("identity already exists")

type Account struct {
	ID   int64
	Name string
	Hash string
	Mail string
	Loc  string
}

type Persona struct {
	ID   int64
	Name string
}

type IdentityStore interface {
	CreateAccount(context.Context, Account) error
	Account(context.Context, string) (Account, bool, error)
	CreatePersona(context.Context, int64, string) error
	Personas(context.Context, int64) ([]Persona, error)
}

type Postgres struct {
	pool *pgxpool.Pool
}

func Open(ctx context.Context, dsn string) (*Postgres, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, errors.New("invalid PostgreSQL configuration")
	}
	p := &Postgres{pool: pool}
	if err := p.initialize(ctx); err != nil {
		p.Close()
		return nil, fmt.Errorf("initialize Rei Nation database: %w", err)
	}
	return p, nil
}

func (p *Postgres) Close() { p.pool.Close() }

func (p *Postgres) Ping(ctx context.Context) error { return p.pool.Ping(ctx) }

func (p *Postgres) initialize(ctx context.Context) error {
	return pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		for _, stmt := range []string{
			"SELECT pg_advisory_xact_lock(1249901214)",
			"CREATE SCHEMA IF NOT EXISTS eanation",
			`CREATE TABLE IF NOT EXISTS eanation.accounts (
				id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
				name text NOT NULL,
				password_hash text NOT NULL,
				mail text NOT NULL,
				loc text NOT NULL
			)`,
			"CREATE UNIQUE INDEX IF NOT EXISTS accounts_name_unique ON eanation.accounts (lower(name))",
			`CREATE UNIQUE INDEX IF NOT EXISTS accounts_mail_unique ON eanation.accounts (lower(mail)) WHERE mail <> ''`,
			`CREATE TABLE IF NOT EXISTS eanation.personas (
				account_id bigint NOT NULL REFERENCES eanation.accounts (id),
				name text PRIMARY KEY
			)`,
			"ALTER TABLE eanation.personas ADD COLUMN IF NOT EXISTS id integer GENERATED ALWAYS AS IDENTITY",
			"ALTER TABLE eanation.personas ADD COLUMN IF NOT EXISTS deleted_at timestamptz",
			"CREATE UNIQUE INDEX IF NOT EXISTS personas_id_unique ON eanation.personas (id)",
			"CREATE UNIQUE INDEX IF NOT EXISTS personas_name_unique ON eanation.personas (lower(name))",
			"CREATE INDEX IF NOT EXISTS personas_account ON eanation.personas (account_id)",
			`CREATE TABLE IF NOT EXISTS eanation.buddies (
				title text NOT NULL, a text NOT NULL REFERENCES eanation.personas(name),
				b text NOT NULL REFERENCES eanation.personas(name), inviter text NOT NULL,
				accepted boolean NOT NULL DEFAULT false, PRIMARY KEY (title, a, b)
			)`,
			"UPDATE eanation.buddies SET accepted=true WHERE title='nascar04' AND NOT accepted",
			`CREATE TABLE IF NOT EXISTS eanation.ignored (
				title text NOT NULL, owner text NOT NULL REFERENCES eanation.personas(name),
				target text NOT NULL REFERENCES eanation.personas(name), PRIMARY KEY (title, owner, target)
			)`,
			`CREATE TABLE IF NOT EXISTS eanation.mailbox (
				id bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
				title text NOT NULL, sender text NOT NULL REFERENCES eanation.personas(name),
				recipient text NOT NULL REFERENCES eanation.personas(name),
				kind text NOT NULL, body bytea NOT NULL, sent_at timestamptz NOT NULL DEFAULT now()
			)`,
			"ALTER TABLE eanation.mailbox ADD COLUMN IF NOT EXISTS delivered_at timestamptz",
			"CREATE INDEX IF NOT EXISTS mailbox_recipient ON eanation.mailbox (title, recipient, id)",
			`CREATE TABLE IF NOT EXISTS eanation.race_receipts (
				title text NOT NULL, race_id text NOT NULL, fingerprint text NOT NULL,
				PRIMARY KEY (title, race_id), UNIQUE (title, fingerprint)
			)`,
			`CREATE TABLE IF NOT EXISTS eanation.race_stats (
				title text NOT NULL, persona text NOT NULL REFERENCES eanation.personas(name),
				totals jsonb NOT NULL, PRIMARY KEY (title, persona)
			)`,
			`CREATE TABLE IF NOT EXISTS eanation.race_details (
				title text NOT NULL, race_id text NOT NULL, track_id integer, report jsonb NOT NULL,
				PRIMARY KEY (title,race_id), FOREIGN KEY (title,race_id) REFERENCES eanation.race_receipts(title,race_id)
			)`,
			"CREATE INDEX IF NOT EXISTS race_details_track ON eanation.race_details (title,track_id) WHERE track_id IS NOT NULL",
			`CREATE TABLE IF NOT EXISTS eanation.player_reports (
				title text NOT NULL, reporter bigint NOT NULL REFERENCES eanation.accounts(id),
				source text NOT NULL REFERENCES eanation.personas(name), target text NOT NULL REFERENCES eanation.personas(name),
				reported_at timestamptz NOT NULL DEFAULT now(), PRIMARY KEY (title,target,reporter)
			)`,
			`CREATE TABLE IF NOT EXISTS eanation.account_recovery (
				account_id bigint PRIMARY KEY REFERENCES eanation.accounts(id), requested_at timestamptz NOT NULL,
				password_hash text, expires_at timestamptz
			)`,
		} {
			if _, err := tx.Exec(ctx, stmt); err != nil {
				return err
			}
		}
		return nil
	})
}

func (p *Postgres) FindPersona(ctx context.Context, name string) (string, bool, error) {
	var canonical string
	err := p.pool.QueryRow(ctx, "SELECT name FROM eanation.personas WHERE lower(name) = lower($1) AND deleted_at IS NULL", name).Scan(&canonical)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	return canonical, err == nil, err
}

func (p *Postgres) CreateAccount(ctx context.Context, a Account) error {
	_, err := p.pool.Exec(ctx, `INSERT INTO eanation.accounts (name, password_hash, mail, loc)
		VALUES ($1, $2, $3, $4)`, a.Name, a.Hash, a.Mail, a.Loc)
	return conflict(err)
}

func (p *Postgres) Account(ctx context.Context, name string) (Account, bool, error) {
	var a Account
	err := p.pool.QueryRow(ctx, `SELECT id, name, password_hash, mail, loc FROM eanation.accounts
		WHERE lower(name) = lower($1)`, name).Scan(&a.ID, &a.Name, &a.Hash, &a.Mail, &a.Loc)
	if errors.Is(err, pgx.ErrNoRows) {
		return Account{}, false, nil
	}
	return a, err == nil, err
}

func (p *Postgres) CreatePersona(ctx context.Context, id int64, name string) error {
	_, err := p.pool.Exec(ctx, "INSERT INTO eanation.personas (account_id, name) VALUES ($1, $2)", id, name)
	return conflict(err)
}

func (p *Postgres) Personas(ctx context.Context, id int64) ([]Persona, error) {
	rows, err := p.pool.Query(ctx, "SELECT id, name FROM eanation.personas WHERE account_id = $1 AND deleted_at IS NULL ORDER BY name", id)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowToStructByPos[Persona])
}

func conflict(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return ErrConflict
	}
	return err
}
