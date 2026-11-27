package storage

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// StatUpdate changes one keyed persona stat for FESL rank titles. Value is a
// decimal string with at most four fractional digits; the caller validates it.
type StatUpdate struct {
	Key   string
	Value string
	// Add accumulates Value into the stored value; otherwise Value replaces it.
	Add bool
	// Text replaces the stat's text when set.
	Text *string
}

// StatValue is one stat for one persona. Rank is the competition rank among
// personas holding the key, highest value first, and 0 when the persona holds
// no value for it.
type StatValue struct {
	Key, Value, Text string
	Rank             int64
}

// StatRow is one leaderboard entry with any extra stats requested alongside.
type StatRow struct {
	PersonaID int64
	Name      string
	StatValue
	Extra []StatValue
}

type StatsStore interface {
	UpdateStats(context.Context, string, int64, []StatUpdate) error
	PersonaStats(context.Context, string, int64, []string) (string, []StatValue, error)
	TopStats(context.Context, string, string, int, int, []string) ([]StatRow, error)
}

const zeroStat = "0.0000"

func (p *Postgres) UpdateStats(ctx context.Context, title string, persona int64, updates []StatUpdate) error {
	return pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
		for _, u := range updates {
			_, err := tx.Exec(ctx, `INSERT INTO eanation.player_stats AS s (title,persona_id,stat_key,value,text)
				VALUES ($1,$2,$3,$4::numeric,coalesce($6,''))
				ON CONFLICT (title,persona_id,stat_key) DO UPDATE SET
					value = CASE WHEN $5 THEN s.value + EXCLUDED.value ELSE EXCLUDED.value END,
					text = coalesce($6, s.text)`, title, persona, u.Key, u.Value, u.Add, u.Text)
			if err != nil {
				return err
			}
		}
		return nil
	})
}

// PersonaStats returns the persona's name and one value per key, in order. An
// unknown persona returns an empty name and no values.
func (p *Postgres) PersonaStats(ctx context.Context, title string, persona int64, keys []string) (string, []StatValue, error) {
	var name string
	err := p.pool.QueryRow(ctx, "SELECT name FROM eanation.personas WHERE id=$1 AND deleted_at IS NULL", persona).Scan(&name)
	if err == pgx.ErrNoRows {
		return "", nil, nil
	}
	if err != nil {
		return "", nil, err
	}
	values := make([]StatValue, len(keys))
	for i, key := range keys {
		v := StatValue{Key: key, Value: zeroStat}
		err := p.pool.QueryRow(ctx, `SELECT s.value::text, s.text, 1 + (SELECT count(*) FROM eanation.player_stats o
				JOIN eanation.personas op ON op.id=o.persona_id AND op.deleted_at IS NULL
				WHERE o.title=s.title AND o.stat_key=s.stat_key AND o.value > s.value)
			FROM eanation.player_stats s WHERE s.title=$1 AND s.persona_id=$2 AND s.stat_key=$3`,
			title, persona, key).Scan(&v.Value, &v.Text, &v.Rank)
		if err != nil && err != pgx.ErrNoRows {
			return "", nil, err
		}
		values[i] = v
	}
	return name, values, nil
}

// TopStats returns up to count entries for key starting at the 1-based rank
// position first, each with the extra keys.
func (p *Postgres) TopStats(ctx context.Context, title, key string, first, count int, extra []string) ([]StatRow, error) {
	// The inner column is v so ORDER BY sorts the number, not the text output.
	rows, err := p.pool.Query(ctx, `SELECT persona_id, name, v::text, text, r FROM (
			SELECT s.persona_id, op.name, s.value v, s.text, rank() OVER (ORDER BY s.value DESC) r
			FROM eanation.player_stats s JOIN eanation.personas op ON op.id=s.persona_id AND op.deleted_at IS NULL
			WHERE s.title=$1 AND s.stat_key=$2) ranked
		ORDER BY v DESC, persona_id OFFSET $3 LIMIT $4`, title, key, first-1, count)
	if err != nil {
		return nil, err
	}
	board, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (StatRow, error) {
		r := StatRow{StatValue: StatValue{Key: key}}
		err := row.Scan(&r.PersonaID, &r.Name, &r.Value, &r.Text, &r.Rank)
		return r, err
	})
	if err != nil || len(extra) == 0 || len(board) == 0 {
		return board, err
	}
	ids := make([]int64, len(board))
	for i, r := range board {
		ids[i] = r.PersonaID
	}
	rows, err = p.pool.Query(ctx, `SELECT persona_id, stat_key, value::text, text FROM eanation.player_stats
		WHERE title=$1 AND persona_id = ANY($2) AND stat_key = ANY($3)`, title, ids, extra)
	if err != nil {
		return nil, err
	}
	type cell struct {
		id  int64
		key string
	}
	found := map[cell]StatValue{}
	for rows.Next() {
		var c cell
		var v StatValue
		if err := rows.Scan(&c.id, &c.key, &v.Value, &v.Text); err != nil {
			rows.Close()
			return nil, err
		}
		v.Key = c.key
		found[c] = v
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range board {
		for _, k := range extra {
			v, ok := found[cell{board[i].PersonaID, k}]
			if !ok {
				v = StatValue{Key: k, Value: zeroStat}
			}
			board[i].Extra = append(board[i].Extra, v)
		}
	}
	return board, nil
}
