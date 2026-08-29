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

// A fold moves one expired partition's lines out of the chat log and their
// counts into chat_stats, so that
//
//	a user's total == chat_stats.messages + COUNT(*) over the live rows
//
// stays true after the lines are gone. Unlike the ledger's fold this is not
// arithmetic anyone depends on at runtime — nothing in the bot reads chat_stats
// — which is exactly why the gate below is a report rather than a refusal.

// FoldResult is one partition's outcome. Skipped is non-empty when the partition
// was already recorded in chat_folded, which is the normal way a re-run behaves
// rather than an error.
type FoldResult struct {
	Name          string
	From, Through time.Time
	Rows, Users   int64
	Skipped       string
}

// retain folds every expired partition, reports on the totals, and drops what it
// folded.
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
		fmt.Fprintf(out, "fold %s: %d messages, %d chatters\n", f.Name, f.Rows, f.Users)
	}

	if err := reportTotals(ctx, pool, cfg, out); err != nil {
		return err
	}

	dropped, err := partition.DropDetached(ctx, pool, cfg.Schema, "chat_folded", cfg.DryRun, out)
	if err != nil {
		return err
	}
	if len(dropped) > 0 && !cfg.DryRun {
		fmt.Fprintf(out, "dropped %d folded partitions: %s\n", len(dropped), strings.Join(dropped, " "))
	}
	return nil
}

// foldExpired folds every chat_message child whose upper bound is at or before
// cutoff.
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
		r, err := foldOne(ctx, pool, cfg, p, c)
		if err != nil {
			return results, fmt.Errorf("folding %s: %w", c.Name, err)
		}
		results = append(results, r)
	}
	return results, nil
}

// foldOne supplies the chat log's half of the shared fold: the claim that makes
// it idempotent, and the per-user counts that have to survive the lines.
//
// Idempotency comes from chat_folded's primary key rather than from
// partman.partitions.status — the same argument ledger_folded makes: coupling
// our totals to another library's bookkeeping means a library upgrade can change
// what they mean.
func foldOne(ctx context.Context, pool *pgxpool.Pool, cfg Config, p partition.Parent, c partition.Child) (FoldResult, error) {
	res := FoldResult{Name: c.Name, From: c.From, Through: c.Through}
	folded := partition.QuoteQualified(cfg.Schema, "chat_folded")
	stats := partition.QuoteQualified(cfg.Schema, "chat_stats")

	claim := func(ctx context.Context, tx pgx.Tx, child string) (bool, error) {
		tag, err := tx.Exec(ctx, `
			INSERT INTO `+folded+` (name, from_ts, through_ts, rows, folded_at)
			SELECT $1, $2, $3, COUNT(*), $4 FROM `+child+`
			ON CONFLICT (name) DO NOTHING`,
			c.Name, c.From.Unix(), c.Through.Unix(), time.Now().Unix())
		if err != nil {
			return false, err
		}
		return tag.RowsAffected() > 0, nil
	}

	agg := func(ctx context.Context, tx pgx.Tx, child string) error {
		// login and display are aggregated as "the newest one in this partition"
		// rather than grouped on, because a rename mid-day would otherwise split
		// one person into two rows. chat_stats holds a name so the totals are
		// legible without users, which is empty whenever the economy is off.
		tag, err := tx.Exec(ctx, `
			INSERT INTO `+stats+` (user_id, login, display, messages, chars, first_ts, last_ts)
			SELECT user_id,
			       (array_agg(login   ORDER BY ts DESC, id DESC))[1],
			       (array_agg(display ORDER BY ts DESC, id DESC))[1],
			       COUNT(*), COALESCE(SUM(length(text)), 0), MIN(ts), MAX(ts)
			  FROM `+child+`
			 GROUP BY user_id
			ON CONFLICT (user_id) DO UPDATE
			  SET messages = `+stats+`.messages + EXCLUDED.messages,
			      chars    = `+stats+`.chars + EXCLUDED.chars,
			      -- LEAST and GREATEST ignore NULLs, so NULLIF makes a first_ts of
			      -- 0 (the column default, on a row nothing has folded into yet)
			      -- lose to a real timestamp instead of winning as the smallest
			      -- number available.
			      first_ts = LEAST(NULLIF(`+stats+`.first_ts, 0), EXCLUDED.first_ts),
			      last_ts  = GREATEST(`+stats+`.last_ts, EXCLUDED.last_ts),
			      login    = EXCLUDED.login,
			      display  = EXCLUDED.display`)
		if err != nil {
			return err
		}
		res.Users = tag.RowsAffected()
		return tx.QueryRow(ctx,
			`SELECT rows FROM `+folded+` WHERE name = $1`, c.Name).Scan(&res.Rows)
	}

	skipped, err := partition.Fold(ctx, pool, p, c.Name, claim, agg)
	if err != nil {
		return res, err
	}
	if skipped {
		res.Skipped = "already in chat_folded"
	}
	return res, nil
}

// reportTotals compares the row counts chat_folded recorded against the totals
// chat_stats accumulated, and says so when they disagree.
//
// This is deliberately weaker than cmd/pg-partition's reconcile, which refuses
// to drop anything until every balance has been proved against its history. That
// gate exists because the ledger is money and a wrong number there is somebody's
// marks. A wrong number here is a message count nothing reads at runtime, and
// halting retention over it would stop the log expiring for no one's benefit.
//
// It is also not an equality the tooling could insist on. -purge-user deletes
// rows that have already been folded and decrements chat_stats to match, but a
// purge run between two folds legitimately moves the two totals apart — a gate
// would turn every erasure request into a stuck nightly job.
//
// See docs/adr/0003-chat-log.md.
func reportTotals(ctx context.Context, pool *pgxpool.Pool, cfg Config, out io.Writer) error {
	var foldedRows, statMessages int64
	if err := pool.QueryRow(ctx, `
		SELECT (SELECT COALESCE(SUM(rows), 0)     FROM `+partition.QuoteQualified(cfg.Schema, "chat_folded")+`),
		       (SELECT COALESCE(SUM(messages), 0) FROM `+partition.QuoteQualified(cfg.Schema, "chat_stats")+`)`,
	).Scan(&foldedRows, &statMessages); err != nil {
		return fmt.Errorf("comparing folded counts: %w", err)
	}
	if foldedRows == statMessages {
		fmt.Fprintf(out, "counts agree: %d folded messages accounted for in chat_stats\n", foldedRows)
		return nil
	}
	fmt.Fprintf(out, "  NOTE: chat_folded recorded %d rows, chat_stats holds %d (difference %d). "+
		"A -purge-user run explains this; anything else is worth a look.\n",
		foldedRows, statMessages, foldedRows-statMessages)
	return nil
}
