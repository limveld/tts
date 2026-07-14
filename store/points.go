package store

import (
	"database/sql"
	"time"
)

// The loyalty-points ("marks") economy is an append-only ledger: every accrual,
// conversion, spend, gamble, and transfer is one signed row, and a balance is
// SUM(delta). A small users table maps the stable Twitch user_id to a current
// login/display so leaderboards and "@name" lookups can show names.

// LedgerEntry is one leaderboard row (a user_id's summed balance with its name).
type LedgerEntry struct {
	UserID  string
	Login   string
	Display string
	Balance int64
}

// UpsertUser records/refreshes a user's identity (called whenever we see them in
// chat or in Get Chatters), so names stay current across renames.
func (s *Store) UpsertUser(userID, login, display string) error {
	if display == "" {
		display = login
	}
	_, err := s.db.Exec(
		`INSERT INTO users (user_id, login, display, last_seen) VALUES (?, ?, ?, ?)
		 ON CONFLICT(user_id) DO UPDATE SET login=excluded.login, display=excluded.display, last_seen=excluded.last_seen`,
		userID, login, display, time.Now().Unix())
	return err
}

// ResolveLogin returns the user_id for a login (from the users table). ok is
// false if we've never seen that login.
func (s *Store) ResolveLogin(login string) (userID string, ok bool, err error) {
	err = s.db.QueryRow(`SELECT user_id FROM users WHERE login = ?`, login).Scan(&userID)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return userID, true, nil
}

// Balance returns a user's current mark balance (SUM of their ledger deltas).
func (s *Store) Balance(userID string) (int64, error) {
	var bal int64
	err := s.db.QueryRow(`SELECT COALESCE(SUM(delta), 0) FROM ledger WHERE user_id = ?`, userID).Scan(&bal)
	return bal, err
}

// Credit adds amount marks to userID with the given reason. ref, when non-empty,
// makes the credit idempotent (a repeated redemption id credits at most once);
// credited is false if that ref was already applied. Pass ref="" for accrual and
// other non-idempotent credits.
func (s *Store) Credit(userID string, amount int64, reason, ref string) (credited bool, err error) {
	var refVal any
	query := `INSERT INTO ledger (user_id, delta, reason, ref, ts) VALUES (?, ?, ?, ?, ?)`
	if ref != "" {
		refVal = ref
		query = `INSERT OR IGNORE INTO ledger (user_id, delta, reason, ref, ts) VALUES (?, ?, ?, ?, ?)`
	}
	res, err := s.db.Exec(query, userID, amount, reason, refVal, time.Now().Unix())
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// Grant is an admin mint/claw-back (for !grant): a positive delta adds marks
// unconditionally; a negative delta removes marks but clamps at 0 (never a
// negative balance). It runs in one immediate transaction and returns the
// resulting balance. The ledger records the actually-applied delta.
func (s *Store) Grant(userID string, delta int64, reason string) (newBal int64, err error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	var bal int64
	if err := tx.QueryRow(`SELECT COALESCE(SUM(delta), 0) FROM ledger WHERE user_id = ?`, userID).Scan(&bal); err != nil {
		return 0, err
	}
	applied := delta
	if delta < 0 && -delta > bal {
		applied = -bal // clamp the removal to what they have
	}
	if applied != 0 {
		if _, err := tx.Exec(
			`INSERT INTO ledger (user_id, delta, reason, ts) VALUES (?, ?, ?, ?)`,
			userID, applied, reason, time.Now().Unix()); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return bal + applied, nil
}

// Spend deducts amount marks from userID if they can afford it, atomically
// (the balance check and the debit run in one immediate transaction so a
// concurrent credit can't be lost and the balance can't go negative). ok is
// false with no error when the balance is insufficient.
func (s *Store) Spend(userID string, amount int64, reason string) (ok bool, err error) {
	if amount <= 0 {
		return true, nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	var bal int64
	if err := tx.QueryRow(`SELECT COALESCE(SUM(delta), 0) FROM ledger WHERE user_id = ?`, userID).Scan(&bal); err != nil {
		return false, err
	}
	if bal < amount {
		return false, nil
	}
	if _, err := tx.Exec(
		`INSERT INTO ledger (user_id, delta, reason, ts) VALUES (?, ?, ?, ?)`,
		userID, -amount, reason, time.Now().Unix()); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

// Transfer moves amount marks from one user to another atomically (used by
// !give). ok is false with no error when the sender can't afford it.
func (s *Store) Transfer(fromID, toID string, amount int64, reason string) (ok bool, err error) {
	if amount <= 0 {
		return true, nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	var bal int64
	if err := tx.QueryRow(`SELECT COALESCE(SUM(delta), 0) FROM ledger WHERE user_id = ?`, fromID).Scan(&bal); err != nil {
		return false, err
	}
	if bal < amount {
		return false, nil
	}
	now := time.Now().Unix()
	if _, err := tx.Exec(`INSERT INTO ledger (user_id, delta, reason, ts) VALUES (?, ?, ?, ?)`,
		fromID, -amount, reason+"_out", now); err != nil {
		return false, err
	}
	if _, err := tx.Exec(`INSERT INTO ledger (user_id, delta, reason, ts) VALUES (?, ?, ?, ?)`,
		toID, amount, reason+"_in", now); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

// Leaderboard returns the top n users by balance (descending), joined to their
// current names. Users with no name row are omitted.
func (s *Store) Leaderboard(n int) ([]LedgerEntry, error) {
	rows, err := s.db.Query(
		`SELECT l.user_id, u.login, u.display, SUM(l.delta) AS bal
		 FROM ledger l JOIN users u ON u.user_id = l.user_id
		 GROUP BY l.user_id
		 HAVING bal > 0
		 ORDER BY bal DESC, u.display ASC
		 LIMIT ?`, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LedgerEntry
	for rows.Next() {
		var e LedgerEntry
		if err := rows.Scan(&e.UserID, &e.Login, &e.Display, &e.Balance); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
