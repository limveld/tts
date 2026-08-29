package main

import (
	"context"
	"fmt"
	"io"

	"github.com/jackc/pgx/v5/pgxpool"

	"tts/internal/partition"
)

// purgeUser deletes every trace of one user id from the chat log: their live
// lines, and the folded counts standing in for the lines already dropped.
//
// It exists so that "delete my data" has a mechanism behind it. A tombstone is
// not one — the log keeps a deleted message's text on purpose, because the
// moderation question is what somebody said — so an erasure request needs a real
// delete, and hand-written SQL under time pressure is where mistakes live.
//
// Both deletes are one transaction. A purge that removed the messages but left
// the stats row behind would leave a count of lines nobody can read, which is
// the opposite of what was asked for.
//
// The DELETE over chat_message carries no time predicate, so it Appends across
// every partition. That is the point: a purge that pruned would miss exactly the
// history it was run to remove.
func purgeUser(ctx context.Context, cfg Config, out io.Writer) error {
	pool, err := pgxpool.New(ctx, cfg.DSN)
	if err != nil {
		return fmt.Errorf("connecting: %w", err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("connecting: %w", err)
	}

	messages := partition.QuoteQualified(cfg.Schema, "chat_message")
	stats := partition.QuoteQualified(cfg.Schema, "chat_stats")

	if cfg.DryRun {
		var live, folded int64
		if err := pool.QueryRow(ctx, `
			SELECT (SELECT COUNT(*) FROM `+messages+` WHERE user_id = $1),
			       (SELECT COALESCE(SUM(messages), 0) FROM `+stats+` WHERE user_id = $1)`,
			cfg.PurgeUser).Scan(&live, &folded); err != nil {
			return fmt.Errorf("counting %s: %w", cfg.PurgeUser, err)
		}
		fmt.Fprintf(out, "purge %s: would delete %d live messages and a folded count of %d\n",
			cfg.PurgeUser, live, folded)
		return nil
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	msgs, err := tx.Exec(ctx, `DELETE FROM `+messages+` WHERE user_id = $1`, cfg.PurgeUser)
	if err != nil {
		return fmt.Errorf("deleting %s's messages: %w", cfg.PurgeUser, err)
	}
	// Deleted rather than zeroed: a row of zeroes still says this user was here,
	// and an erasure request is asking for the opposite.
	rows, err := tx.Exec(ctx, `DELETE FROM `+stats+` WHERE user_id = $1`, cfg.PurgeUser)
	if err != nil {
		return fmt.Errorf("deleting %s's stats: %w", cfg.PurgeUser, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}

	fmt.Fprintf(out, "purge %s: deleted %d live messages and %d stats row(s)\n",
		cfg.PurgeUser, msgs.RowsAffected(), rows.RowsAffected())
	// The counts in chat_folded still include the purged lines, which is why
	// reportTotals reports rather than gates. Said out loud so the next nightly
	// run's NOTE is not a surprise.
	fmt.Fprintln(out, "  chat_folded still counts the purged history; the next pass will note the difference")
	return nil
}
