package main

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Mismatch is one user whose materialized balance disagrees with the history
// behind it.
type Mismatch struct {
	UserID           string
	Balance, Derived int64
}

// reconcile returns every user whose accounts.balance disagrees with
// ledger_opening.delta + SUM(ledger.delta). It gates the drop: history is only
// destroyed once it has proved it agrees with the balances that outlive it.
//
// This is what makes the materialized balance something the tooling proves
// rather than something the schema hopes for — the trade ADR-0002 made when it
// reversed ADR-0001's "no derived value can drift".
//
// One statement, so it reads one snapshot. Two statements would take two, and a
// fold committing between them would show as a phantom mismatch that stops a
// drop for no reason.
//
// The FROM clause is the union of accounts and ledger and ledger_opening rather
// than just accounts: a user with history and no account row is money with no
// owner, and a query anchored on accounts would never see them.
func reconcile(ctx context.Context, pool *pgxpool.Pool) ([]Mismatch, error) {
	rows, err := pool.Query(ctx, `
		WITH ids AS (
		    SELECT user_id FROM accounts
		    UNION SELECT user_id FROM ledger
		    UNION SELECT user_id FROM ledger_opening
		)
		SELECT i.user_id,
		       COALESCE((SELECT balance FROM accounts       WHERE user_id = i.user_id), 0) AS balance,
		       COALESCE((SELECT delta   FROM ledger_opening WHERE user_id = i.user_id), 0)
		     + COALESCE((SELECT SUM(delta) FROM ledger      WHERE user_id = i.user_id), 0) AS derived
		  FROM ids i
		 WHERE COALESCE((SELECT balance FROM accounts       WHERE user_id = i.user_id), 0)
		    <> COALESCE((SELECT delta   FROM ledger_opening WHERE user_id = i.user_id), 0)
		     + COALESCE((SELECT SUM(delta) FROM ledger      WHERE user_id = i.user_id), 0)
		 ORDER BY i.user_id`)
	if err != nil {
		return nil, fmt.Errorf("reconciling: %w", err)
	}
	defer rows.Close()

	var out []Mismatch
	for rows.Next() {
		var m Mismatch
		if err := rows.Scan(&m.UserID, &m.Balance, &m.Derived); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// lockParent takes gopartman's own per-parent advisory lock, so its maintenance
// and our fold can never run against the ledger at the same time.
//
// Two things about this are copied from the library rather than invented, and
// both matter:
//
//   - hashtext() is evaluated by Postgres, not in Go. gopartman issues
//     pg_try_advisory_lock(hashtext($1), hashtext($2)) with the schema and table
//     as text. Reimplementing hashtext here would produce a different key, two
//     locks that never contend, and no mutual exclusion whatsoever — a bug that
//     would look exactly like working code.
//   - Advisory locks are session-scoped, so the lock lives on one connection.
//     Taking it through the pool would hand the connection straight back, and
//     the unlock would then run on a different session and quietly fail. So a
//     connection is acquired and held for the duration.
//
// held is false when someone else has it. That is not an error: a nightly job
// overlapping itself means the work is being done, just not by this process.
//
// Do NOT hold this across gopartman's Maintain. Maintain takes the same lock
// per parent and, failing to get it, logs and skips — so nesting them would
// silently turn provisioning off. See pass().
func lockParent(ctx context.Context, pool *pgxpool.Pool, schema, table string) (unlock func(), held bool, err error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("acquiring a connection for the maintenance lock: %w", err)
	}
	if err := conn.QueryRow(ctx,
		`SELECT pg_try_advisory_lock(hashtext($1), hashtext($2))`, schema, table).Scan(&held); err != nil {
		conn.Release()
		return nil, false, fmt.Errorf("taking the maintenance lock: %w", err)
	}
	if !held {
		conn.Release()
		return func() {}, false, nil
	}
	return func() {
		// WithoutCancel so a cancelled run still releases rather than leaving the
		// lock to the connection's eventual teardown.
		conn.Exec(context.WithoutCancel(ctx),
			`SELECT pg_advisory_unlock(hashtext($1), hashtext($2))`, schema, table)
		conn.Release()
	}, true, nil
}
