package sqlite

import (
	"database/sql"
	"time"

	"tts/store"
)

// The loyalty-points ("marks") economy is an append-only ledger: every accrual,
// conversion, spend, gamble, and transfer is one signed row, and a balance is
// SUM(delta). A small users table maps the stable Twitch user_id to a current
// login/display so leaderboards and "@name" lookups can show names.
//
// As of migration 00003 that balance is also materialized on accounts.balance,
// written by applyDelta in the same transaction as the ledger row it reflects.
// Nothing reads it yet — the read paths switch over in a later migration, once
// the conformance suite has proved the column agrees with SUM(ledger). Until
// then this file maintains a value it does not consume, on purpose.
//
// 00002 created accounts and said SQLite would never write to it. That is no
// longer true: the lock it exists for is still unnecessary here (one writer at a
// time), but the balance is behavioral and conformance-tested, so it has to be
// real on both backends.

// ensureAccount creates userID's account row if it is missing, so applyDelta has
// something to update. SQLite needs no lock — one writer at a time — so unlike
// the Postgres twin this is the whole of the account bookkeeping.
func ensureAccount(tx *sql.Tx, userID string, now int64) error {
	_, err := tx.Exec(
		`INSERT INTO accounts (user_id, created_at, updated_at) VALUES (?, ?, ?)
		 ON CONFLICT(user_id) DO NOTHING`, userID, now, now)
	return err
}

// applyDelta moves userID's materialized balance by delta. It must run in the
// same transaction as the ledger row it corresponds to, or the two can disagree.
// Callers pass the delta they actually wrote — Grant passes its post-clamp value,
// not the requested one.
func applyDelta(tx *sql.Tx, userID string, delta, now int64) error {
	_, err := tx.Exec(
		`UPDATE accounts SET balance = balance + ?, updated_at = ? WHERE user_id = ?`,
		delta, now, userID)
	return err
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
// It is a transaction so the ledger row and the balance commit together.
func (s *Store) Credit(userID string, amount int64, reason, ref string) (credited bool, err error) {
	var refVal any
	query := `INSERT INTO ledger (user_id, delta, reason, ref, ts) VALUES (?, ?, ?, ?, ?)`
	if ref != "" {
		refVal = ref
		query = `INSERT OR IGNORE INTO ledger (user_id, delta, reason, ref, ts) VALUES (?, ?, ?, ?, ?)`
	}
	now := time.Now().Unix()
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	res, err := tx.Exec(query, userID, amount, reason, refVal, now)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// The ref was already applied. Nothing written, no delta applied.
		return false, nil
	}
	if err := ensureAccount(tx, userID, now); err != nil {
		return false, err
	}
	if err := applyDelta(tx, userID, amount, now); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// Grant is an admin mint/claw-back (for !grant): a positive delta adds marks
// unconditionally; a negative delta removes marks but clamps at 0 (never a
// negative balance). It returns the resulting balance; the ledger records the
// actually-applied delta.
//
// The check-then-write is atomic because Open sets _txlock=immediate and SQLite
// admits one writer at a time, so the balance read here can't be invalidated
// before the insert. A backend with concurrent writers cannot rely on that and
// must lock the account row explicitly — see store/postgres.
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
	now := time.Now().Unix()
	if applied != 0 {
		if _, err := tx.Exec(
			`INSERT INTO ledger (user_id, delta, reason, ts) VALUES (?, ?, ?, ?)`,
			userID, applied, reason, now); err != nil {
			return 0, err
		}
	}
	// Unconditional, even when applied is 0: a clawed-back-to-zero account still
	// needs its row to exist, so a later credit has something to update.
	if err := ensureAccount(tx, userID, now); err != nil {
		return 0, err
	}
	// applied, not delta — the clamped ledger row and the balance move together.
	if err := applyDelta(tx, userID, applied, now); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return bal + applied, nil
}

// Spend deducts amount marks from userID if they can afford it. ok is false with
// no error when the balance is insufficient.
//
// The balance check and the debit share one write transaction, and SQLite's
// single writer is what makes that a real guarantee: no concurrent debit can
// slip between them, so the balance can't go negative. See Grant on what a
// multi-writer backend owes here instead.
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
	now := time.Now().Unix()
	if _, err := tx.Exec(
		`INSERT INTO ledger (user_id, delta, reason, ts) VALUES (?, ?, ?, ?)`,
		userID, -amount, reason, now); err != nil {
		return false, err
	}
	if err := ensureAccount(tx, userID, now); err != nil {
		return false, err
	}
	if err := applyDelta(tx, userID, -amount, now); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

// Transfer moves amount marks from one user to another (used by !give). Both
// ledger rows land in one transaction, so marks are never in flight. ok is false
// with no error when the sender can't afford it.
//
// As with Spend, the sender's balance check is protected by SQLite's single
// writer rather than by a lock on the sender's account.
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
	for _, a := range []struct {
		userID string
		delta  int64
	}{{fromID, -amount}, {toID, amount}} {
		if err := ensureAccount(tx, a.userID, now); err != nil {
			return false, err
		}
		if err := applyDelta(tx, a.userID, a.delta, now); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

// Leaderboard returns the top n users by balance (descending), joined to their
// current names. Users with no name row are omitted.
func (s *Store) Leaderboard(n int) ([]store.LedgerEntry, error) {
	rows, err := s.db.Query(
		// GROUP BY names every selected non-aggregate column and HAVING repeats the
		// aggregate rather than referencing the "bal" alias: SQLite tolerates both
		// shortcuts, Postgres rejects them. ORDER BY may keep the alias — that one
		// is legal in either dialect.
		`SELECT l.user_id, u.login, u.display, SUM(l.delta) AS bal
		 FROM ledger l JOIN users u ON u.user_id = l.user_id
		 GROUP BY l.user_id, u.login, u.display
		 HAVING SUM(l.delta) > 0
		 ORDER BY bal DESC, u.display ASC
		 LIMIT ?`, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.LedgerEntry
	for rows.Next() {
		var e store.LedgerEntry
		if err := rows.Scan(&e.UserID, &e.Login, &e.Display, &e.Balance); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
