package sqlite

import "tts/store"

// Torch Maze win tally: one row per user who has escaped a maze, with their
// current name for the !mazewins leaderboard. Only the round's winner is
// counted; placements are paid in marks through the ledger, which already
// records who got what. The in-flight board lives in game_rounds, owned by the
// bot.

// MazeAddWin increments a winner's tally (creating the row on first win),
// refreshing their name, and returns the new total.
func (s *Store) MazeAddWin(userID, login, display string) (wins int, err error) {
	if display == "" {
		display = login
	}
	_, err = s.db.Exec(
		`INSERT INTO maze_wins (user_id, login, display, wins) VALUES (?, ?, ?, 1)
		 ON CONFLICT(user_id) DO UPDATE SET wins = wins + 1, login = excluded.login, display = excluded.display`,
		userID, login, display)
	if err != nil {
		return 0, err
	}
	err = s.db.QueryRow(`SELECT wins FROM maze_wins WHERE user_id = ?`, userID).Scan(&wins)
	return wins, err
}

// MazeLeaderboard returns the top n winners by win count (descending).
func (s *Store) MazeLeaderboard(n int) ([]store.MazeWin, error) {
	rows, err := s.db.Query(
		`SELECT login, display, wins FROM maze_wins
		 WHERE wins > 0
		 ORDER BY wins DESC, display ASC
		 LIMIT ?`, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.MazeWin
	for rows.Next() {
		var w store.MazeWin
		if err := rows.Scan(&w.Login, &w.Display, &w.Wins); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}
