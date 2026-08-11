package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"tts/store"
)

// The loyalty-points ("marks") economy: an append-only ledger of signed deltas,
// a materialized running total in accounts.balance, and a users table mapping
// the stable Twitch user_id to a current login/display.
//
// accounts.balance is what someone has; the ledger is how they got there. The
// split exists because a balance derived from history cannot outlive it, and the
// ledger is partitioned by day with a retention horizon — old partitions are
// folded into ledger_opening and dropped, which would silently change a
// SUM(ledger) balance. applyDelta writes the balance in the same transaction as
// the ledger row it reflects, and cmd/pg-partition refuses to drop any history
// until it has proved balance == ledger_opening + SUM(ledger) for every user.
// See docs/adr/0002-ledger-retention-and-partitioning.md.
//
// The ledger's append-only-ness was never enough on its own here. Two concurrent
// Spends can both read the same balance and both pass the check, and READ
// COMMITTED will happily commit both — a lost update on real currency. The fix
// is a row lock, and when the balance was SUM(ledger) it could not be taken on
// the balance itself: a predicate has no lock that stops a concurrent INSERT. So
// there is a concrete row per user to serialize on, in accounts — the same row
// that now carries the balance.

// ensureAccount creates userID's account row if it is missing. Split out of
// lockAccount because Credit needs the row to exist but has no reason to lock it
// for reading: a credit cannot overdraw, so there is no check to serialize.
func ensureAccount(ctx context.Context, tx *sql.Tx, userID string, now int64) error {
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO accounts (user_id, created_at, updated_at) VALUES ($1, $2, $2)
		 ON CONFLICT (user_id) DO NOTHING`, userID, now); err != nil {
		return fmt.Errorf("ensure account %s: %w", userID, err)
	}
	return nil
}

// lockAccount pins userID's account row for the remainder of tx. Every path that
// reads a balance in order to change it takes this lock first, so check-then-
// debit is serialized per user. Credit deliberately does not — see the package
// comment.
func lockAccount(ctx context.Context, tx *sql.Tx, userID string, now int64) error {
	for attempt := 0; attempt < 2; attempt++ {
		if err := ensureAccount(ctx, tx, userID, now); err != nil {
			return err
		}
		var got string
		err := tx.QueryRowContext(ctx,
			`SELECT user_id FROM accounts WHERE user_id = $1 FOR UPDATE`, userID).Scan(&got)
		if err == nil {
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("lock account %s: %w", userID, err)
		}
		// A racing inserter held the key while our DO NOTHING ran and then rolled
		// back, so the row we expected isn't there. Our insert wins the retry.
	}
	return fmt.Errorf("lock account %s: row vanished twice", userID)
}

// balanceQuery is the one true way to read a balance. It is a single statement
// so it reads a single snapshot, and it is shared by Balance and balanceTx so
// the locked and unlocked reads cannot drift apart.
//
// The COALESCE covers a user with no accounts row: Balance takes no lock and has
// no guarantee the row exists, and "never seen" has to read as zero rather than
// as an error.
const balanceQuery = `SELECT COALESCE((SELECT balance FROM accounts WHERE user_id = $1), 0)`

// balanceTx reads userID's balance inside tx. Callers must already hold the
// account lock if they intend to write based on the answer.
func balanceTx(ctx context.Context, tx *sql.Tx, userID string) (int64, error) {
	var bal int64
	err := tx.QueryRowContext(ctx, balanceQuery, userID).Scan(&bal)
	return bal, err
}

// applyDelta moves userID's materialized balance by delta and records that the
// account was used. It must run in the same transaction as the ledger row it
// corresponds to: accounts.balance is a running total of ledger.delta, and the
// two committing apart is exactly the drift ADR-0001 avoided by not having a
// balance column at all. Callers pass the delta they actually wrote — Grant in
// particular passes its post-clamp value, not the requested one.
//
// The row must exist; every caller has already been through ensureAccount or
// lockAccount.
func applyDelta(ctx context.Context, tx *sql.Tx, userID string, delta, now int64) error {
	_, err := tx.ExecContext(ctx,
		`UPDATE accounts SET balance = balance + $2, updated_at = $3 WHERE user_id = $1`,
		userID, delta, now)
	return err
}

// UpsertUser records/refreshes a user's identity (called whenever we see them in
// chat or in Get Chatters), so names stay current across renames.
func (s *Store) UpsertUser(userID, login, display string) error {
	if display == "" {
		display = login
	}
	_, err := s.db.Exec(
		`INSERT INTO users (user_id, login, display, last_seen) VALUES ($1, $2, $3, $4)
		 ON CONFLICT (user_id) DO UPDATE SET login = excluded.login, display = excluded.display, last_seen = excluded.last_seen`,
		userID, login, display, time.Now().Unix())
	return err
}

// ResolveLogin returns the user_id for a login (from the users table). ok is
// false if we've never seen that login.
func (s *Store) ResolveLogin(login string) (userID string, ok bool, err error) {
	err = s.db.QueryRow(`SELECT user_id FROM users WHERE login = $1`, login).Scan(&userID)
	if err == sql.ErrNoRows {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return userID, true, nil
}

// Balance returns a user's current mark balance: a single-row lookup on
// accounts, whatever the ledger has grown to. (At 114 rows in one page the
// planner takes a seq scan over accounts_pkey and is right to; the point is that
// neither plan touches the ledger.)
func (s *Store) Balance(userID string) (int64, error) {
	var bal int64
	err := s.db.QueryRow(balanceQuery, userID).Scan(&bal)
	return bal, err
}

// Credit adds amount marks to userID with the given reason. ref, when non-empty,
// makes the credit idempotent (a repeated redemption id credits at most once);
// credited is false if that ref was already applied. Pass ref="" for accrual and
// other non-idempotent credits.
//
// No account lock: a credit can only raise a balance, so it cannot overdraw, and
// one landing mid-Spend just leaves the payer richer than that check believed.
// It is a transaction all the same, because the ledger row and the balance have
// to commit together or they can disagree.
func (s *Store) Credit(userID string, amount int64, reason, ref string) (credited bool, err error) {
	ctx := context.Background()
	now := time.Now().Unix()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	var refVal any
	if ref != "" {
		refVal = ref
		// ledger_refs, not the ledger, is what decides whether this redemption has
		// already been applied. The guarantee is about the redemption id and has to
		// outlive the ledger row, which retention can drop.
		//
		// Under concurrency this blocks rather than races: INSERT ... ON CONFLICT DO
		// NOTHING against a key held by an *uncommitted* transaction waits on the
		// speculative-insertion token, then takes the DO NOTHING path if that
		// transaction committed or inserts if it rolled back. So of N goroutines
		// racing one ref, exactly one sees n=1.
		res, err := tx.ExecContext(ctx,
			`INSERT INTO ledger_refs (ref, user_id, ts) VALUES ($1, $2, $3)
			 ON CONFLICT (ref) DO NOTHING`, ref, userID, now)
		if err != nil {
			return false, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			// Already applied. Nothing was written, so the deferred Rollback is the
			// whole cleanup — and crucially no delta is applied to the balance.
			return false, nil
		}
	}
	if err := ensureAccount(ctx, tx, userID, now); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO ledger (user_id, delta, reason, ref, ts) VALUES ($1, $2, $3, $4, $5)`,
		userID, amount, reason, refVal, now); err != nil {
		return false, err
	}
	// Last, so the row lock this takes is held for as little of the transaction
	// as possible: under a redemption retry storm every goroutine wants this row.
	if err := applyDelta(ctx, tx, userID, amount, now); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// Grant is an admin mint/claw-back (for !grant): a positive delta adds marks
// unconditionally; a negative delta removes marks but clamps at 0 (never a
// negative balance). It returns the resulting balance; the ledger records the
// actually-applied delta.
func (s *Store) Grant(userID string, delta int64, reason string) (newBal int64, err error) {
	ctx := context.Background()
	now := time.Now().Unix()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	if err := lockAccount(ctx, tx, userID, now); err != nil {
		return 0, err
	}
	bal, err := balanceTx(ctx, tx, userID)
	if err != nil {
		return 0, err
	}
	applied := delta
	if delta < 0 && -delta > bal {
		applied = -bal // clamp the removal to what they have
	}
	if applied != 0 {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO ledger (user_id, delta, reason, ts) VALUES ($1, $2, $3, $4)`,
			userID, applied, reason, now); err != nil {
			return 0, err
		}
	}
	// applied, not delta: a claw-back clamped at zero writes a clamped ledger row,
	// and the balance has to move by the same number that row records.
	if err := applyDelta(ctx, tx, userID, applied, now); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return bal + applied, nil
}

// Spend deducts amount marks from userID if they can afford it. ok is false with
// no error when the balance is insufficient.
func (s *Store) Spend(userID string, amount int64, reason string) (ok bool, err error) {
	if amount <= 0 {
		return true, nil
	}
	ctx := context.Background()
	now := time.Now().Unix()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	if err := lockAccount(ctx, tx, userID, now); err != nil {
		return false, err
	}
	bal, err := balanceTx(ctx, tx, userID)
	if err != nil {
		return false, err
	}
	if bal < amount {
		return false, nil
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO ledger (user_id, delta, reason, ts) VALUES ($1, $2, $3, $4)`,
		userID, -amount, reason, now); err != nil {
		return false, err
	}
	if err := applyDelta(ctx, tx, userID, -amount, now); err != nil {
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
func (s *Store) Transfer(fromID, toID string, amount int64, reason string) (ok bool, err error) {
	if amount <= 0 {
		return true, nil
	}
	ctx := context.Background()
	now := time.Now().Unix()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	// Sorted, always. A→B and B→A running at once would otherwise each hold the
	// row the other wants, and Postgres would break the tie by killing one of
	// them with "deadlock detected".
	first, second := fromID, toID
	if second < first {
		first, second = second, first
	}
	if err := lockAccount(ctx, tx, first, now); err != nil {
		return false, err
	}
	if second != first {
		if err := lockAccount(ctx, tx, second, now); err != nil {
			return false, err
		}
	}

	bal, err := balanceTx(ctx, tx, fromID)
	if err != nil {
		return false, err
	}
	if bal < amount {
		return false, nil
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO ledger (user_id, delta, reason, ts) VALUES ($1, $2, $3, $4)`,
		fromID, -amount, reason+"_out", now); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO ledger (user_id, delta, reason, ts) VALUES ($1, $2, $3, $4)`,
		toID, amount, reason+"_in", now); err != nil {
		return false, err
	}
	if err := applyDelta(ctx, tx, fromID, -amount, now); err != nil {
		return false, err
	}
	if err := applyDelta(ctx, tx, toID, amount, now); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

// Leaderboard returns the top n users by balance (descending), joined to their
// current names. Users with no name row are omitted.
func (s *Store) Leaderboard(n int) ([]store.LedgerEntry, error) {
	// No aggregate and no ledger: served by accounts_balance, the partial index on
	// (balance DESC) WHERE balance > 0. The old shape aggregated every ledger row
	// for every user on every call, which is the query daily partitioning would
	// have punished hardest.
	//
	// WHERE balance > 0 is the old HAVING SUM(l.delta) > 0. An account that never
	// earned anything is 0 and is excluded either way, and the join still omits
	// users with no identity row.
	rows, err := s.db.Query(
		`SELECT a.user_id, u.login, u.display, a.balance
		 FROM accounts a JOIN users u ON u.user_id = a.user_id
		 WHERE a.balance > 0
		 ORDER BY a.balance DESC, u.display COLLATE "C" ASC
		 LIMIT $1`, n)
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
