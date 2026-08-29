// Command chat-partition keeps the chat log's daily partitions provisioned and
// expires the old ones, folding each partition's per-user totals into chat_stats
// before its lines go.
//
// One pass, run daily by launchd at 05:30 — half an hour after pg-backup.sh and
// fifteen minutes after pg-partition. Both offsets are safety properties rather
// than scheduling details: the 05:00 dump holds every row this is about to
// remove, and pg-partition has finished with gopartman's advisory lock by the
// time this asks for it.
//
//	provision   gopartman creates tomorrow's child (and premake days beyond)
//	fold        each expired partition is DETACHed and its per-user counts
//	            added to chat_stats, in one transaction
//	report      chat_folded's row counts are compared against chat_stats
//	drop        the folded partitions are dropped
//
// It is cmd/pg-partition's sibling and shares its machinery (internal/partition).
// Two things differ, and both follow from the ledger being money and this not
// being:
//
//   - The fold accumulates counts rather than a currency total, so a mistake
//     here is a wrong number in a table nothing reads at runtime.
//   - Nothing gates the drop. pg-partition refuses to destroy history until
//     every balance has been proved against it; blocking chat retention on an
//     equivalent check would stop the log expiring over a discrepancy with no
//     consequence — and -purge-user makes that discrepancy a legitimate outcome
//     rather than a symptom. The comparison is printed instead.
//
// See docs/adr/0003-chat-log.md.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"tts/store"
)

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "chat-partition: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("chat-partition", flag.ContinueOnError)
	fs.SetOutput(out)
	var (
		dsn       = fs.String("db", os.Getenv("TTS_DATABASE_URL"), "Postgres DSN (default $TTS_DATABASE_URL)")
		schema    = fs.String("schema", "public", "schema holding the chat log")
		premake   = fs.Int("premake", defaultPremake, "days of future partitions to keep ahead")
		retention = fs.Duration("retention", defaultRetention, "how long itemized chat history is kept")
		backfill  = fs.Bool("backfill", false, "create missing back-dated children and drain the default partition")
		purge     = fs.String("purge-user", "", "delete every trace of one user id, then exit (an erasure request)")
		dryRun    = fs.Bool("dry-run", false, "report what would happen and change nothing")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}

	if *dsn == "" {
		return errors.New("no database: pass -db or set TTS_DATABASE_URL")
	}
	// Refuse a SQLite DSN outright rather than failing obscurely three calls
	// later. There is no retention on SQLite by design — see docs/adr/0002 for
	// the ledger's version of the same decision.
	if b, _ := store.Classify(*dsn); b != store.PostgresBackend {
		return fmt.Errorf("this tool is Postgres-only, but %q is a %s DSN", *dsn, b)
	}
	if *retention <= 0 {
		// gopartman treats a zero RetentionPeriod as "expire everything", and so
		// would foldExpired's cutoff. Neither is ever what someone means.
		return fmt.Errorf("-retention must be positive, got %s", *retention)
	}

	cfg := Config{
		DSN:       *dsn,
		Schema:    *schema,
		Premake:   *premake,
		Retention: *retention,
		Backfill:  *backfill,
		PurgeUser: *purge,
		DryRun:    *dryRun,
	}
	if cfg.PurgeUser != "" {
		// A purge is a different job from a maintenance pass and does not run
		// alongside one: it is invoked by a person answering a specific request,
		// and it should do that one thing where they can see it.
		return purgeUser(ctx, cfg, out)
	}
	return pass(ctx, cfg, out)
}

const (
	// Two weeks of grace if the launchd agent stops firing. A missing future
	// partition is not data loss — rows land in chat_message_default and
	// -backfill recovers them — but the recovery is manual, so make it rare.
	// Migration 00006 seeds the same window for the same reason.
	defaultPremake = 14

	// Ninety days of itemized chat, against the ledger's 365.
	//
	// Shorter because the two answer different questions. A year of ledger rows
	// is what makes "why do I have this many marks" answerable; ninety days of
	// chat is what makes "what did they say before they were banned" answerable,
	// and no moderation question reaches back further than a quarter.
	//
	// It is worth being plain about the cost: pg-backup.sh keeps fourteen days of
	// dumps, so from day 91 a line's text is gone for good. chat_stats keeps the
	// counts; nothing keeps the words.
	defaultRetention = 90 * 24 * time.Hour
)
