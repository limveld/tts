package postgres

import "tts/store"

// Wordle win tally: one row per user who has solved a board, with their current
// name for the !wordlewins leaderboard. The current-round board lives in
// game_rounds, owned by the bot.

// WordleAddWin increments a solver's win tally (creating the row on first win),
// refreshing their name, and returns the new total.
func (s *Store) WordleAddWin(userID, login, display string) (wins int, err error) {
	if display == "" {
		display = login
	}
	err = s.db.QueryRow(
		`INSERT INTO wordle_wins (user_id, login, display, wins) VALUES ($1, $2, $3, 1)
		 ON CONFLICT (user_id) DO UPDATE SET
		     wins = wordle_wins.wins + 1,
		     login = excluded.login,
		     display = excluded.display
		 RETURNING wins`,
		userID, login, display).Scan(&wins)
	return wins, err
}

// WordleLeaderboard returns the top n solvers by win count (descending).
func (s *Store) WordleLeaderboard(n int) ([]store.WordleWin, error) {
	rows, err := s.db.Query(
		`SELECT login, display, wins FROM wordle_wins
		 WHERE wins > 0
		 ORDER BY wins DESC, display COLLATE "C" ASC
		 LIMIT $1`, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.WordleWin
	for rows.Next() {
		var w store.WordleWin
		if err := rows.Scan(&w.Login, &w.Display, &w.Wins); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}
