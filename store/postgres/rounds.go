package postgres

import (
	"database/sql"
	"time"

	"tts/store"
)

// One row per game, holding that game's in-flight round. state is JSONB here
// rather than TEXT so psql can query into it; the store still treats the
// document as opaque. Note that JSONB normalizes key order and whitespace, so a
// round read back is semantically but not byte-identical to what was written —
// every game unmarshals it, so nothing cares, but a test comparing bytes would.

// SaveRound stores (or replaces) game's in-flight round.
func (s *Store) SaveRound(game, roomID string, endsAt int64, state []byte) error {
	_, err := s.db.Exec(
		`INSERT INTO game_rounds (game, room_id, ends_at, state, updated_at) VALUES ($1, $2, $3, $4::jsonb, $5)
		 ON CONFLICT (game) DO UPDATE SET
		     room_id = excluded.room_id,
		     ends_at = excluded.ends_at,
		     state = excluded.state,
		     updated_at = excluded.updated_at`,
		game, roomID, endsAt, string(state), time.Now().Unix())
	return err
}

// LoadRound returns game's stored round; ok is false when none is stored.
func (s *Store) LoadRound(game string) (store.Round, bool, error) {
	r := store.Round{Game: game}
	var state string
	err := s.db.QueryRow(
		`SELECT room_id, ends_at, state::text, updated_at FROM game_rounds WHERE game = $1`, game,
	).Scan(&r.RoomID, &r.EndsAt, &state, &r.UpdatedAt)
	if err == sql.ErrNoRows {
		return store.Round{}, false, nil
	}
	if err != nil {
		return store.Round{}, false, err
	}
	r.State = []byte(state)
	return r, true, nil
}

// ClearRound drops game's stored round. Clearing an absent round is not an error.
func (s *Store) ClearRound(game string) error {
	_, err := s.db.Exec(`DELETE FROM game_rounds WHERE game = $1`, game)
	return err
}
