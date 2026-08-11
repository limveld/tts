package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// A fold moves one expired partition's history out of the ledger and its
// arithmetic into ledger_opening, so that
//
//	accounts.balance == ledger_opening.delta + SUM(ledger.delta)
//
// still holds for every user afterwards. Balances themselves never move: they
// live on accounts and have since migration 00003, which is what demotes this
// whole file from a money path to an audit path. A bug here is a reconcile
// alert, not somebody's marks changing.

// FoldResult is one partition's outcome. Skipped is non-empty when the
// partition was already recorded in ledger_folded — which is the normal way a
// re-run behaves, not an error.
type FoldResult struct {
	Name          string
	From, Through time.Time
	Rows, Users   int64
	Delta         int64
	Skipped       string
}

// retain folds every expired partition, checks the books, and only then drops
// anything. The order is the whole design: nothing is destroyed until the
// arithmetic that outlives it has been shown to add up.
func retain(ctx context.Context, pool *pgxpool.Pool, cfg Config, out io.Writer) error {
	cutoff := time.Now().UTC().Add(-cfg.Retention)
	folded, err := foldExpired(ctx, pool, cfg, cutoff, out)
	if err != nil {
		return err
	}
	for _, f := range folded {
		if f.Skipped != "" {
			fmt.Fprintf(out, "fold %s: already folded, skipping\n", f.Name)
			continue
		}
		fmt.Fprintf(out, "fold %s: %d rows, %d users, delta %d\n", f.Name, f.Rows, f.Users, f.Delta)
	}

	// Always reconcile, even when nothing folded: it is the cheapest possible
	// check that the materialized balance still agrees with its history, and a
	// nightly job is exactly where you want to find out that it does not.
	bad, err := reconcile(ctx, pool)
	if err != nil {
		return err
	}
	if len(bad) > 0 {
		for i, m := range bad {
			if i == 10 {
				fmt.Fprintf(out, "  ... and %d more\n", len(bad)-10)
				break
			}
			fmt.Fprintf(out, "  %s: balance %d, history %d\n", m.UserID, m.Balance, m.Derived)
		}
		return fmt.Errorf("%d users' balances disagree with their history; nothing dropped", len(bad))
	}
	fmt.Fprintln(out, "reconcile: every balance agrees with its history")

	dropped, err := dropFolded(ctx, pool, cfg, out)
	if err != nil {
		return err
	}
	if len(dropped) > 0 {
		fmt.Fprintf(out, "dropped %d folded partitions: %s\n", len(dropped), strings.Join(dropped, " "))
	}
	return nil
}

// foldExpired folds every ledger child whose upper bound is at or before cutoff.
//
// Each partition is one transaction that DETACHes it and adds its per-user
// totals to ledger_opening in the same commit. PostgreSQL has transactional
// DDL, so that costs nothing and means the invariant is never observably false
// — not for a millisecond, and not if the process dies mid-pass. Detaching
// first takes ACCESS EXCLUSIVE on the parent up front, which on a ~640-row child
// at 05:15 with no stream running is milliseconds. (DETACH CONCURRENTLY cannot
// run inside a transaction block, which is exactly why it is not used.)
//
// Idempotency comes from ledger_folded's primary key rather than from
// partman.partitions.status: coupling this to another library's bookkeeping
// would mean a library upgrade could change what the money means.
func foldExpired(ctx context.Context, pool *pgxpool.Pool, cfg Config, cutoff time.Time, out io.Writer) ([]FoldResult, error) {
	type candidate struct {
		name       string
		from, thru time.Time
	}
	var candidates []candidate

	rows, err := pool.Query(ctx, `
		SELECT c.relname,
		       (regexp_match(pg_get_expr(c.relpartbound, c.oid), 'FROM \(''([^'']+)''\)'))[1]::timestamptz,
		       (regexp_match(pg_get_expr(c.relpartbound, c.oid), 'TO \(''([^'']+)''\)'))[1]::timestamptz
		  FROM pg_class c
		  JOIN pg_inherits i ON i.inhrelid = c.oid
		 WHERE i.inhparent = $1::regclass
		   AND pg_get_expr(c.relpartbound, c.oid) <> 'DEFAULT'
		 ORDER BY c.relname`, quoteQualified(cfg.Schema, "ledger"))
	if err != nil {
		return nil, fmt.Errorf("listing partitions: %w", err)
	}
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.name, &c.from, &c.thru); err != nil {
			rows.Close()
			return nil, err
		}
		// Half-open bounds: a partition is expired once its exclusive upper bound
		// has passed the cutoff, i.e. every row it can hold is older than the
		// horizon. Using the lower bound would expire a partition still taking
		// writes.
		if !c.thru.After(cutoff) {
			candidates = append(candidates, c)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing partitions: %w", err)
	}

	var results []FoldResult
	for _, c := range candidates {
		if cfg.DryRun {
			fmt.Fprintf(out, "fold %s: would fold (bounds %s … %s)\n",
				c.name, c.from.Format(time.DateOnly), c.thru.Format(time.DateOnly))
			results = append(results, FoldResult{Name: c.name, From: c.from, Through: c.thru, Skipped: "dry run"})
			continue
		}
		r, err := foldOne(ctx, pool, cfg, c.name, c.from, c.thru)
		if err != nil {
			return results, fmt.Errorf("folding %s: %w", c.name, err)
		}
		results = append(results, r)
	}
	return results, nil
}

func foldOne(ctx context.Context, pool *pgxpool.Pool, cfg Config, name string, from, thru time.Time) (FoldResult, error) {
	res := FoldResult{Name: name, From: from, Through: thru}

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return res, err
	}
	defer tx.Rollback(ctx)

	// Claim it first. If a previous run committed the fold and then died before
	// dropping, this conflicts and we skip straight to the drop — the alternative
	// is folding the same partition's deltas into ledger_opening twice, which is
	// the one arithmetic mistake this file must never make.
	claimed, err := tx.Exec(ctx, `
		INSERT INTO ledger_folded (name, from_ts, through_ts, rows, delta, folded_at)
		SELECT $1, $2, $3, COUNT(*), COALESCE(SUM(delta), 0), $4 FROM `+quoteQualified(cfg.Schema, name)+`
		ON CONFLICT (name) DO NOTHING`,
		name, from.Unix(), thru.Unix(), time.Now().Unix())
	if err != nil {
		return res, err
	}
	if claimed.RowsAffected() == 0 {
		res.Skipped = "already in ledger_folded"
		return res, nil
	}

	if _, err := tx.Exec(ctx, fmt.Sprintf(`ALTER TABLE %s DETACH PARTITION %s`,
		quoteQualified(cfg.Schema, "ledger"), quoteQualified(cfg.Schema, name))); err != nil {
		return res, err
	}

	tag, err := tx.Exec(ctx, `
		INSERT INTO ledger_opening (user_id, delta, through_ts)
		SELECT user_id, SUM(delta), MAX(ts) FROM `+quoteQualified(cfg.Schema, name)+` GROUP BY user_id
		ON CONFLICT (user_id) DO UPDATE
		  SET delta      = ledger_opening.delta + EXCLUDED.delta,
		      through_ts = GREATEST(ledger_opening.through_ts, EXCLUDED.through_ts)`)
	if err != nil {
		return res, err
	}
	res.Users = tag.RowsAffected()

	if err := tx.QueryRow(ctx,
		`SELECT rows, delta FROM ledger_folded WHERE name = $1`, name).Scan(&res.Rows, &res.Delta); err != nil {
		return res, err
	}
	return res, tx.Commit(ctx)
}

// dropFolded drops every detached child already recorded in ledger_folded.
//
// It works from ledger_folded rather than from a list of what was just folded,
// so it also cleans up orphans: a crash between a fold's COMMIT and its DROP
// leaves a detached table that no longer answers SELECT ... FROM ledger but is
// still in pg_dump, quietly growing every backup.
func dropFolded(ctx context.Context, pool *pgxpool.Pool, cfg Config, out io.Writer) ([]string, error) {
	rows, err := pool.Query(ctx, `
		SELECT f.name
		  FROM ledger_folded f
		  JOIN pg_class c ON c.relname = f.name
		  JOIN pg_namespace n ON n.oid = c.relnamespace AND n.nspname = $1
		 WHERE NOT EXISTS (SELECT 1 FROM pg_inherits i WHERE i.inhrelid = c.oid)
		 ORDER BY f.name`, cfg.Schema)
	if err != nil {
		return nil, fmt.Errorf("listing folded partitions: %w", err)
	}
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return nil, err
		}
		names = append(names, n)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing folded partitions: %w", err)
	}

	if cfg.DryRun {
		for _, n := range names {
			fmt.Fprintf(out, "would drop %s\n", n)
		}
		return names, nil
	}
	for _, n := range names {
		if _, err := pool.Exec(ctx, `DROP TABLE `+quoteQualified(cfg.Schema, n)); err != nil {
			return nil, fmt.Errorf("dropping %s: %w", n, err)
		}
	}
	return names, nil
}
