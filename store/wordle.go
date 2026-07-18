package store

// Wordle win tally: one row per user who has solved a board, with their current
// name for the !wordlewins leaderboard. The current-round board state is stored
// separately as JSON in the settings table (owned by the bot).

// WordleWin is one leaderboard row.
type WordleWin struct {
	Login   string
	Display string
	Wins    int
}

// WordleAddWin increments a solver's win tally (creating the row on first win),
// refreshing their name, and returns the new total.
func (s *Store) WordleAddWin(userID, login, display string) (wins int, err error) {
	if display == "" {
		display = login
	}
	_, err = s.db.Exec(
		`INSERT INTO wordle_wins (user_id, login, display, wins) VALUES (?, ?, ?, 1)
		 ON CONFLICT(user_id) DO UPDATE SET wins = wins + 1, login = excluded.login, display = excluded.display`,
		userID, login, display)
	if err != nil {
		return 0, err
	}
	err = s.db.QueryRow(`SELECT wins FROM wordle_wins WHERE user_id = ?`, userID).Scan(&wins)
	return wins, err
}

// WordleLeaderboard returns the top n solvers by win count (descending).
func (s *Store) WordleLeaderboard(n int) ([]WordleWin, error) {
	rows, err := s.db.Query(
		`SELECT login, display, wins FROM wordle_wins
		 WHERE wins > 0
		 ORDER BY wins DESC, display ASC
		 LIMIT ?`, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WordleWin
	for rows.Next() {
		var w WordleWin
		if err := rows.Scan(&w.Login, &w.Display, &w.Wins); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}
