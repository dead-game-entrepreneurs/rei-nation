package storage

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

const BuddyAddedMail = "buddy-added"

var errBuddyUnavailable = errors.New("buddy add unavailable")

type Mail struct {
	ID                   int64
	From, To, Kind, Body string
	Sent                 time.Time
	Delivered            bool
}

type MessengerStore interface {
	FindPersona(context.Context, string) (string, bool, error)
	SearchPersonas(context.Context, string, int) ([]string, error)
	Roster(context.Context, string, string, string) ([]string, error)
	AddBuddy(context.Context, string, string, string, string) (Mail, bool, error)
	ChangeBuddy(context.Context, string, string, string, string) (bool, error)
	CanContact(context.Context, string, string, string) (bool, error)
	QueueMail(context.Context, string, Mail) (Mail, bool, error)
	Mailbox(context.Context, string, string) ([]Mail, error)
	AckMail(context.Context, string, string, int64) error
}

func (p *Postgres) SearchPersonas(ctx context.Context, prefix string, limit int) ([]string, error) {
	rows, err := p.pool.Query(ctx, `SELECT name FROM eanation.personas
		WHERE deleted_at IS NULL AND starts_with(lower(name),lower($1)) ORDER BY lower(name) LIMIT $2`, prefix, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

func (p *Postgres) Roster(ctx context.Context, title, name, list string) ([]string, error) {
	var rows pgx.Rows
	var err error
	if list == "I" {
		rows, err = p.pool.Query(ctx, `SELECT target FROM eanation.ignored
			WHERE title=$1 AND owner=$2 ORDER BY target LIMIT 100`, title, name)
	} else {
		rows, err = p.pool.Query(ctx, `SELECT CASE WHEN a=$2 THEN b ELSE a END
			FROM eanation.buddies WHERE title=$1 AND (a=$2 OR b=$2) ORDER BY 1 LIMIT 100`, title, name)
	}
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

func (p *Postgres) AddBuddy(ctx context.Context, title, self, other, notice string) (Mail, bool, error) {
	mail := Mail{From: self, To: other, Kind: BuddyAddedMail, Body: notice}
	a, b := min(self, other), max(self, other)
	var added bool
	err := pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(1249901215)"); err != nil {
			return err
		}
		ok, err := contact(ctx, tx, title, self, other)
		if err != nil {
			return err
		}
		if !ok {
			return errBuddyUnavailable
		}
		result, err := tx.Exec(ctx, `INSERT INTO eanation.buddies(title,a,b,inviter,accepted)
			SELECT $1,$2,$3,$4,true WHERE NOT EXISTS (
				SELECT 1 FROM (SELECT count(*) n FROM eanation.buddies WHERE title=$1 AND (a=$2 OR b=$2)
					UNION ALL SELECT count(*) FROM eanation.buddies WHERE title=$1 AND (a=$3 OR b=$3)) counts WHERE n>=100
			) ON CONFLICT DO NOTHING`, title, a, b, self)
		if err != nil {
			return err
		}
		if result.RowsAffected() == 0 {
			var exists bool
			err := tx.QueryRow(ctx, `SELECT EXISTS (
				SELECT 1 FROM eanation.buddies WHERE title=$1 AND a=$2 AND b=$3
			)`, title, a, b).Scan(&exists)
			if err == nil && !exists {
				return errBuddyUnavailable
			}
			return err
		}
		added = true
		return queueMail(ctx, tx, title, &mail)
	})
	return mail, added && err == nil, err
}

func (p *Postgres) ChangeBuddy(ctx context.Context, title, self, other, op string) (bool, error) {
	if self == other {
		return false, nil
	}
	a, b := min(self, other), max(self, other)
	var changed bool
	err := pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		// Roster and mailbox admission share the lock so a block applies atomically.
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(1249901215)"); err != nil {
			return err
		}
		switch op {
		case "remove":
			r, err := tx.Exec(ctx, "DELETE FROM eanation.buddies WHERE title=$1 AND a=$2 AND b=$3", title, a, b)
			if err != nil || r.RowsAffected() == 0 {
				return err
			}
			changed = true
			// A recipient who has not signed in yet should not get a stale add notice.
			_, err = tx.Exec(ctx, `DELETE FROM eanation.mailbox WHERE title=$1 AND kind=$4 AND
				((sender=$2 AND recipient=$3) OR (sender=$3 AND recipient=$2))`, title, a, b, BuddyAddedMail)
			return err
		case "block":
			r, err := tx.Exec(ctx, `INSERT INTO eanation.ignored(title,owner,target)
				SELECT $1,$2,$3 WHERE (SELECT count(*) FROM eanation.ignored WHERE title=$1 AND owner=$2)<100
				AND (SELECT count(*) FROM eanation.personas WHERE name IN ($2,$3) AND deleted_at IS NULL)=2
				ON CONFLICT DO NOTHING`, title, self, other)
			if err != nil || r.RowsAffected() == 0 {
				return err
			}
			changed = true
			if _, err := tx.Exec(ctx, "DELETE FROM eanation.buddies WHERE title=$1 AND a=$2 AND b=$3", title, a, b); err != nil {
				return err
			}
			_, err = tx.Exec(ctx, `DELETE FROM eanation.mailbox WHERE title=$1 AND
				((sender=$2 AND recipient=$3) OR (sender=$3 AND recipient=$2))`, title, self, other)
			return err
		case "unblock":
			r, err := tx.Exec(ctx, "DELETE FROM eanation.ignored WHERE title=$1 AND owner=$2 AND target=$3", title, self, other)
			changed = r.RowsAffected() != 0
			return err
		}
		return nil
	})
	return changed, err
}

type queryRower interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func contact(ctx context.Context, q queryRower, title, a, b string) (bool, error) {
	var ok bool
	err := q.QueryRow(ctx, `SELECT NOT EXISTS (SELECT 1 FROM eanation.ignored WHERE title=$1
		AND ((owner=$2 AND target=$3) OR (owner=$3 AND target=$2)))
		AND (SELECT count(*) FROM eanation.personas WHERE name IN ($2,$3) AND deleted_at IS NULL)=2`, title, a, b).Scan(&ok)
	return ok, err
}

func (p *Postgres) CanContact(ctx context.Context, title, a, b string) (bool, error) {
	return contact(ctx, p.pool, title, a, b)
}

func (p *Postgres) QueueMail(ctx context.Context, title string, m Mail) (Mail, bool, error) {
	err := pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(1249901215)"); err != nil {
			return err
		}
		if ok, err := contact(ctx, tx, title, m.From, m.To); !ok || err != nil {
			return err
		}
		return queueMail(ctx, tx, title, &m)
	})
	return m, m.ID != 0, err
}

func queueMail(ctx context.Context, tx pgx.Tx, title string, m *Mail) error {
	if _, err := tx.Exec(ctx, `DELETE FROM eanation.mailbox WHERE title=$1 AND recipient=$2
		AND delivered_at IS NOT NULL AND sent_at <= now() - interval '3 days'`, title, m.To); err != nil {
		return err
	}
	// Keep a 100-message mailbox. Pending deliveries take precedence over history.
	if _, err := tx.Exec(ctx, `DELETE FROM eanation.mailbox WHERE id=(
		SELECT min(id) FROM eanation.mailbox WHERE title=$1 AND recipient=$2 AND delivered_at IS NOT NULL
	) AND (SELECT count(*) FROM eanation.mailbox WHERE title=$1 AND recipient=$2)>=100`, title, m.To); err != nil {
		return err
	}
	err := tx.QueryRow(ctx, `INSERT INTO eanation.mailbox(title,sender,recipient,kind,body)
		SELECT $1,$2,$3,$4,$5 WHERE
		(SELECT count(*) FROM eanation.mailbox WHERE title=$1 AND recipient=$3)<100
		RETURNING id,sent_at`, title, m.From, m.To, m.Kind, []byte(m.Body)).Scan(&m.ID, &m.Sent)
	if err == pgx.ErrNoRows {
		return nil
	}
	return err
}

func (p *Postgres) Mailbox(ctx context.Context, title, name string) ([]Mail, error) {
	rows, err := p.pool.Query(ctx, `SELECT id,sender,recipient,kind,body,sent_at,delivered_at IS NOT NULL
		FROM eanation.mailbox
		WHERE title=$1 AND recipient=$2 AND (delivered_at IS NULL OR sent_at > now() - interval '3 days')
		ORDER BY id LIMIT 100`, title, name)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Mail, error) {
		var m Mail
		var body []byte
		err := row.Scan(&m.ID, &m.From, &m.To, &m.Kind, &body, &m.Sent, &m.Delivered)
		m.Body = string(body)
		return m, err
	})
}

func (p *Postgres) AckMail(ctx context.Context, title, name string, id int64) error {
	// The PS2 clears its inbox on sign-out and receives history after its next roster sync.
	// One-time notices are consumed as soon as their frame is sent.
	_, err := p.pool.Exec(ctx, `WITH transient AS (
		DELETE FROM eanation.mailbox WHERE title=$1 AND recipient=$2 AND id=$3 AND kind<>'C'
	) UPDATE eanation.mailbox SET delivered_at=now()
	WHERE title=$1 AND recipient=$2 AND id=$3 AND kind='C' AND delivered_at IS NULL`, title, name, id)
	return err
}
