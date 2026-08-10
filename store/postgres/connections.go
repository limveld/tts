package postgres

import "tts/store"

// Connections completion tally: one row per player who has landed the final
// group of a puzzle. Per-group solves pay marks via the ledger but aren't
// tallied here — this table counts full completions only.

// ConnectionsAddWin increments a completer's tally (creating the row on first
// completion), refreshing their name, and returns the new total.
func (s *Store) ConnectionsAddWin(userID, login, display string) (wins int, err error) {
	if display == "" {
		display = login
	}
	err = s.db.QueryRow(
		`INSERT INTO connections_wins (user_id, login, display, wins) VALUES ($1, $2, $3, 1)
		 ON CONFLICT (user_id) DO UPDATE SET
		     wins = connections_wins.wins + 1,
		     login = excluded.login,
		     display = excluded.display
		 RETURNING wins`,
		userID, login, display).Scan(&wins)
	return wins, err
}

// ConnectionsLeaderboard returns the top n completers by count (descending).
func (s *Store) ConnectionsLeaderboard(n int) ([]store.ConnectionsWin, error) {
	rows, err := s.db.Query(
		`SELECT login, display, wins FROM connections_wins
		 WHERE wins > 0
		 ORDER BY wins DESC, display COLLATE "C" ASC
		 LIMIT $1`, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.ConnectionsWin
	for rows.Next() {
		var w store.ConnectionsWin
		if err := rows.Scan(&w.Login, &w.Display, &w.Wins); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}
