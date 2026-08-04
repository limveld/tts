package store

// Connections completion tally: one row per player who has landed the final
// group of a puzzle, with their current name for the !connectionswins
// leaderboard. Per-group solves pay marks via the ledger but aren't tallied
// here — this table counts full completions only. The current-round board state
// is stored separately as JSON in the settings table (owned by the bot).

// ConnectionsWin is one leaderboard row.
type ConnectionsWin struct {
	Login   string
	Display string
	Wins    int
}

// ConnectionsAddWin increments a completer's tally (creating the row on first
// completion), refreshing their name, and returns the new total.
func (s *Store) ConnectionsAddWin(userID, login, display string) (wins int, err error) {
	if display == "" {
		display = login
	}
	_, err = s.db.Exec(
		`INSERT INTO connections_wins (user_id, login, display, wins) VALUES (?, ?, ?, 1)
		 ON CONFLICT(user_id) DO UPDATE SET wins = wins + 1, login = excluded.login, display = excluded.display`,
		userID, login, display)
	if err != nil {
		return 0, err
	}
	err = s.db.QueryRow(`SELECT wins FROM connections_wins WHERE user_id = ?`, userID).Scan(&wins)
	return wins, err
}

// ConnectionsLeaderboard returns the top n completers by count (descending).
func (s *Store) ConnectionsLeaderboard(n int) ([]ConnectionsWin, error) {
	rows, err := s.db.Query(
		`SELECT login, display, wins FROM connections_wins
		 WHERE wins > 0
		 ORDER BY wins DESC, display ASC
		 LIMIT ?`, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ConnectionsWin
	for rows.Next() {
		var w ConnectionsWin
		if err := rows.Scan(&w.Login, &w.Display, &w.Wins); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}
