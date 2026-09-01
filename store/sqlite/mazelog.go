package sqlite

import (
	"database/sql"
	"strings"

	"tts/store"
)

// The Torch Maze replay log: finished rounds and the events they produced, kept
// permanently. The in-flight round lives in game_rounds and is deleted when it
// clears; this is the archive that outlives it.

// mazeEventBatch caps how many events go into one INSERT. A whole round is well
// under this, so the loop never runs twice in practice — it matches the Postgres
// twin so a batch that succeeds on one backend cannot fail on the other.
const mazeEventBatch = 200

// MazeLogRound writes a finished round and its events in one transaction.
//
// One call rather than two, because a round without its events, or events without
// their round, is a half-truth in something meant to be permanent.
//
// It is idempotent. More than one path can plausibly reach it — a round that ends
// on a tick, a moderator's !skipgame, and a bot restarting onto a round that had
// already finished — and reasoning about whether two can ever both fire is a
// worse use of anyone's time than ON CONFLICT DO NOTHING.
func (s *Store) MazeLogRound(r store.MazeRound, evs []store.MazeEvent) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(
		`INSERT INTO maze_rounds
		   (id, room_id, seed, started_at, ended_at, tick_ms, cycles, reason,
		    players, finishers, winner_id, winner_login, winner_display, input)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO NOTHING`,
		r.ID, r.RoomID, r.Seed, r.StartedAt, r.EndedAt, r.TickMS, r.Cycles, r.Reason,
		r.Players, r.Finishers, r.WinnerID, r.WinnerLogin, r.WinnerDisplay, string(r.Input),
	); err != nil {
		return err
	}

	for start := 0; start < len(evs); start += mazeEventBatch {
		end := min(start+mazeEventBatch, len(evs))
		chunk := evs[start:end]

		var b strings.Builder
		b.WriteString(`INSERT INTO maze_events
		   (round_id, seq, cycle, kind, seat, user_id, login, display, at, n, reason) VALUES `)
		args := make([]any, 0, len(chunk)*11)
		for i, e := range chunk {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString("(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)")
			args = append(args, e.RoundID, e.Seq, e.Cycle, e.Kind, e.Seat,
				e.UserID, e.Login, e.Display, e.At, e.N, e.Reason)
		}
		b.WriteString(" ON CONFLICT(round_id, seq) DO NOTHING")
		if _, err := tx.Exec(b.String(), args...); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// MazeRoundLog returns the n most recently finished rounds.
func (s *Store) MazeRoundLog(n int) ([]store.MazeRound, error) {
	rows, err := s.db.Query(
		`SELECT id, room_id, seed, started_at, ended_at, tick_ms, cycles, reason,
		        players, finishers, winner_id, winner_login, winner_display, input
		   FROM maze_rounds
		  ORDER BY ended_at DESC, id DESC
		  LIMIT ?`, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMazeRounds(rows)
}

// MazeRoundByID returns one round; ok is false when there is no such round.
func (s *Store) MazeRoundByID(id string) (store.MazeRound, bool, error) {
	rows, err := s.db.Query(
		`SELECT id, room_id, seed, started_at, ended_at, tick_ms, cycles, reason,
		        players, finishers, winner_id, winner_login, winner_display, input
		   FROM maze_rounds WHERE id = ?`, id)
	if err != nil {
		return store.MazeRound{}, false, err
	}
	defer rows.Close()
	out, err := scanMazeRounds(rows)
	if err != nil || len(out) == 0 {
		return store.MazeRound{}, false, err
	}
	return out[0], true, nil
}

// MazeRoundEvents returns a round's events in emission order.
func (s *Store) MazeRoundEvents(id string) ([]store.MazeEvent, error) {
	rows, err := s.db.Query(
		`SELECT round_id, seq, cycle, kind, seat, user_id, login, display, at, n, reason
		   FROM maze_events WHERE round_id = ? ORDER BY seq`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.MazeEvent
	for rows.Next() {
		var e store.MazeEvent
		if err := rows.Scan(&e.RoundID, &e.Seq, &e.Cycle, &e.Kind, &e.Seat,
			&e.UserID, &e.Login, &e.Display, &e.At, &e.N, &e.Reason); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

func scanMazeRounds(rows *sql.Rows) ([]store.MazeRound, error) {
	var out []store.MazeRound
	for rows.Next() {
		var r store.MazeRound
		var input string
		if err := rows.Scan(&r.ID, &r.RoomID, &r.Seed, &r.StartedAt, &r.EndedAt,
			&r.TickMS, &r.Cycles, &r.Reason, &r.Players, &r.Finishers,
			&r.WinnerID, &r.WinnerLogin, &r.WinnerDisplay, &input); err != nil {
			return nil, err
		}
		r.Input = []byte(input)
		out = append(out, r)
	}
	return out, rows.Err()
}
