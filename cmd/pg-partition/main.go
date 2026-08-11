// Command pg-partition keeps the ledger's daily partitions provisioned and
// expires the old ones, without moving anyone's marks.
//
// One pass, run daily by launchd at 05:15 — fifteen minutes after pg-backup.sh,
// which is a safety property rather than a scheduling detail: the 05:00 dump
// holds every row this is about to remove, so the worst case recovery from a
// fold bug is a backup taken fifteen minutes prior.
//
//	provision   gopartman creates tomorrow's child (and premake days beyond)
//	fold        each expired partition is DETACHed and its per-user totals
//	            added to ledger_opening, in one transaction
//	reconcile   every user's accounts.balance is checked against
//	            ledger_opening + SUM(ledger)
//	drop        the folded partitions are dropped — only if reconcile is clean
//
// It lives in cmd/ rather than in the bot for four reasons, any one of which
// would be enough: ADR-0001 says Store.DB() is for cmd/ only, the bot may be
// running on SQLite where none of this means anything, gopartman needs a
// pgxpool while the store is database/sql, and a job that runs DDL has no
// business inside the process that answers chat.
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
		fmt.Fprintf(os.Stderr, "pg-partition: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("pg-partition", flag.ContinueOnError)
	fs.SetOutput(out)
	var (
		dsn       = fs.String("db", os.Getenv("TTS_DATABASE_URL"), "Postgres DSN (default $TTS_DATABASE_URL)")
		schema    = fs.String("schema", "public", "schema holding the ledger")
		premake   = fs.Int("premake", defaultPremake, "days of future partitions to keep ahead")
		retention = fs.Duration("retention", defaultRetention, "how long itemized ledger history is kept")
		backfill  = fs.Bool("backfill", false, "create missing back-dated children and drain the default partition")
		dryRun    = fs.Bool("dry-run", false, "report what would happen and change nothing")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}

	if *dsn == "" {
		return errors.New("no database: pass -db or set TTS_DATABASE_URL")
	}
	// Refuse a SQLite DSN outright rather than failing obscurely three calls
	// later. There is no retention on SQLite by design — see docs/adr/0002.
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
		DryRun:    *dryRun,
	}
	return pass(ctx, cfg, out)
}

const (
	// Two weeks of grace if the launchd agent stops firing. A missing future
	// partition is not data loss — rows land in ledger_default and -backfill
	// recovers them — but the recovery is manual, so make it rare.
	defaultPremake = 14

	// A full year of itemized history, which is what makes "why do I have this
	// many marks" answerable, while bounding the table at ~234k rows.
	defaultRetention = 365 * 24 * time.Hour
)
