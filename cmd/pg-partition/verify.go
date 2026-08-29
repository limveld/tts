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
