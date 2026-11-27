package storage

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// FriendState is one side's view of a buddies row in a title that asks before
// adding a friend. NASCAR 04 adds buddies at once and does not use these.
type FriendState int

const (
	FriendAccepted FriendState = iota + 1
	// FriendIncoming is a request the other persona sent and this one has not answered.
	FriendIncoming
	// FriendOutgoing is a request this persona sent.
	FriendOutgoing
)

type Friend struct {
	ID    int64
	Name  string
	State FriendState
}

// ErrFriendUnavailable reports an unknown, deleted or blocked persona, or a
// full roster.
var ErrFriendUnavailable = errors.New("friend unavailable")

// FriendStore keeps request-and-accept friendships in the shared buddies
// table. Removing and blocking use MessengerStore.ChangeBuddy.
type FriendStore interface {
	PersonaByName(context.Context, string) (Persona, bool, error)
	Friends(context.Context, string, string) ([]Friend, error)
	RequestFriend(context.Context, string, string, string) (FriendState, error)
	AnswerFriend(context.Context, string, string, string, bool) (bool, error)
}

func (p *Postgres) PersonaByName(ctx context.Context, name string) (Persona, bool, error) {
	var persona Persona
	err := p.pool.QueryRow(ctx, `SELECT id, name FROM eanation.personas
		WHERE lower(name)=lower($1) AND deleted_at IS NULL`, name).Scan(&persona.ID, &persona.Name)
	if errors.Is(err, pgx.ErrNoRows) {
		return Persona{}, false, nil
	}
	return persona, err == nil, err
}

func (p *Postgres) Friends(ctx context.Context, title, name string) ([]Friend, error) {
	rows, err := p.pool.Query(ctx, `SELECT o.id, o.name, f.accepted, f.inviter=$2
		FROM eanation.buddies f JOIN eanation.personas o
			ON o.name = CASE WHEN f.a=$2 THEN f.b ELSE f.a END AND o.deleted_at IS NULL
		WHERE f.title=$1 AND (f.a=$2 OR f.b=$2) ORDER BY lower(o.name) LIMIT 100`, title, name)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Friend, error) {
		var f Friend
		var accepted, sent bool
		err := row.Scan(&f.ID, &f.Name, &accepted, &sent)
		switch {
		case accepted:
			f.State = FriendAccepted
		case sent:
			f.State = FriendOutgoing
		default:
			f.State = FriendIncoming
		}
		return f, err
	})
}

// RequestFriend records a request from self to other. A request to someone who
// already asked self accepts theirs. The result is self's view afterwards.
func (p *Postgres) RequestFriend(ctx context.Context, title, self, other string) (FriendState, error) {
	if self == other {
		return 0, ErrFriendUnavailable
	}
	a, b := min(self, other), max(self, other)
	var state FriendState
	err := pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(1249901215)"); err != nil {
			return err
		}
		var inviter string
		var accepted bool
		err := tx.QueryRow(ctx, `SELECT inviter, accepted FROM eanation.buddies
			WHERE title=$1 AND a=$2 AND b=$3`, title, a, b).Scan(&inviter, &accepted)
		switch {
		case err == nil && accepted:
			state = FriendAccepted
			return nil
		case err == nil && inviter == self:
			state = FriendOutgoing
			return nil
		case err == nil:
			state = FriendAccepted
			_, err = tx.Exec(ctx, "UPDATE eanation.buddies SET accepted=true WHERE title=$1 AND a=$2 AND b=$3", title, a, b)
			return err
		case !errors.Is(err, pgx.ErrNoRows):
			return err
		}
		if ok, err := contact(ctx, tx, title, self, other); err != nil || !ok {
			if err == nil {
				err = ErrFriendUnavailable
			}
			return err
		}
		r, err := tx.Exec(ctx, `INSERT INTO eanation.buddies(title,a,b,inviter,accepted)
			SELECT $1,$2,$3,$4,false WHERE NOT EXISTS (
				SELECT 1 FROM (SELECT count(*) n FROM eanation.buddies WHERE title=$1 AND (a=$2 OR b=$2)
					UNION ALL SELECT count(*) FROM eanation.buddies WHERE title=$1 AND (a=$3 OR b=$3)) counts WHERE n>=100
			)`, title, a, b, self)
		if err != nil {
			return err
		}
		if r.RowsAffected() == 0 {
			return ErrFriendUnavailable
		}
		state = FriendOutgoing
		return nil
	})
	return state, err
}

// AnswerFriend accepts or declines a pending request that requester sent to
// self. It reports false when there was no such request.
func (p *Postgres) AnswerFriend(ctx context.Context, title, self, requester string, accept bool) (bool, error) {
	if self == requester {
		return false, nil
	}
	a, b := min(self, requester), max(self, requester)
	stmt := "DELETE FROM eanation.buddies WHERE title=$1 AND a=$2 AND b=$3 AND inviter=$4 AND NOT accepted"
	if accept {
		stmt = "UPDATE eanation.buddies SET accepted=true WHERE title=$1 AND a=$2 AND b=$3 AND inviter=$4 AND NOT accepted"
	}
	r, err := p.pool.Exec(ctx, stmt, title, a, b, requester)
	return err == nil && r.RowsAffected() == 1, err
}
