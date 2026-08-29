package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"tts/internal/partition"
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
//
// The transaction skeleton — claim, detach, aggregate, commit — lives in
// internal/partition, shared with the chat log's fold. What stays here is the
// arithmetic and the gate, which are the parts that are about money.

// FoldResult is one partition's outcome. Skipped is non-empty when the partition
// was already recorded in ledger_folded — which is the normal way a re-run
// behaves, not an error.
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

	dropped, err := partition.DropDetached(ctx, pool, cfg.Schema, "ledger_folded", cfg.DryRun, out)
	if err != nil {
		return err
	}
	if len(dropped) > 0 && !cfg.DryRun {
		fmt.Fprintf(out, "dropped %d folded partitions: %s\n", len(dropped), strings.Join(dropped, " "))
	}
	return nil
}

// foldExpired folds every ledger child whose upper bound is at or before cutoff.
func foldExpired(ctx context.Context, pool *pgxpool.Pool, cfg Config, cutoff time.Time, out io.Writer) ([]FoldResult, error) {
	p := cfg.parent()
	children, err := partition.ListChildren(ctx, pool, p)
	if err != nil {
		return nil, err
	}

	var results []FoldResult
	for _, c := range children {
		if !c.Expired(cutoff) {
			continue
		}
		if cfg.DryRun {
			fmt.Fprintf(out, "fold %s: would fold (bounds %s … %s)\n",
				c.Name, c.From.Format(time.DateOnly), c.Through.Format(time.DateOnly))
			results = append(results, FoldResult{Name: c.Name, From: c.From, Through: c.Through, Skipped: "dry run"})
			continue
		}
		r, err := foldOne(ctx, pool, p, c)
		if err != nil {
			return results, fmt.Errorf("folding %s: %w", c.Name, err)
		}
		results = append(results, r)
	}
	return results, nil
}

// foldOne supplies the ledger's half of the shared fold: the claim that makes it
// idempotent, and the per-user totals that have to survive the rows.
//
// Idempotency comes from ledger_folded's primary key rather than from
// partman.partitions.status: coupling this to another library's bookkeeping
// would mean a library upgrade could change what the money means.
func foldOne(ctx context.Context, pool *pgxpool.Pool, p partition.Parent, c partition.Child) (FoldResult, error) {
	res := FoldResult{Name: c.Name, From: c.From, Through: c.Through}

	claim := func(ctx context.Context, tx pgx.Tx, child string) (bool, error) {
		tag, err := tx.Exec(ctx, `
			INSERT INTO ledger_folded (name, from_ts, through_ts, rows, delta, folded_at)
			SELECT $1, $2, $3, COUNT(*), COALESCE(SUM(delta), 0), $4 FROM `+child+`
			ON CONFLICT (name) DO NOTHING`,
			c.Name, c.From.Unix(), c.Through.Unix(), time.Now().Unix())
		if err != nil {
			return false, err
		}
		return tag.RowsAffected() > 0, nil
	}

	agg := func(ctx context.Context, tx pgx.Tx, child string) error {
		tag, err := tx.Exec(ctx, `
			INSERT INTO ledger_opening (user_id, delta, through_ts)
			SELECT user_id, SUM(delta), MAX(ts) FROM `+child+` GROUP BY user_id
			ON CONFLICT (user_id) DO UPDATE
			  SET delta      = ledger_opening.delta + EXCLUDED.delta,
			      through_ts = GREATEST(ledger_opening.through_ts, EXCLUDED.through_ts)`)
		if err != nil {
			return err
		}
		res.Users = tag.RowsAffected()
		return tx.QueryRow(ctx,
			`SELECT rows, delta FROM ledger_folded WHERE name = $1`, c.Name).Scan(&res.Rows, &res.Delta)
	}

	skipped, err := partition.Fold(ctx, pool, p, c.Name, claim, agg)
	if err != nil {
		return res, err
	}
	if skipped {
		res.Skipped = "already in ledger_folded"
	}
	return res, nil
}
