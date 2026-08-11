package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	gopartman "github.com/jirevwe/gopartman"
)

// Config is one run's settings. It is a struct rather than loose arguments
// because pass, ensureRegistered and the tests all need the same set.
type Config struct {
	DSN       string
	Schema    string
	Premake   int
	Retention time.Duration
	Backfill  bool
	DryRun    bool
}

func (c Config) ref() gopartman.ParentRef {
	return gopartman.ParentRef{SchemaName: c.Schema, TableName: "ledger"}
}

// pass runs one full maintenance cycle. Every step is idempotent, so a failed
// run is retried simply by running again — which is what launchd does tomorrow.
func pass(ctx context.Context, cfg Config, out io.Writer) error {
	pool, err := pgxpool.New(ctx, cfg.DSN)
	if err != nil {
		return fmt.Errorf("connecting: %w", err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("connecting: %w", err)
	}

	// Provisioning is skipped wholesale under -dry-run, rather than each write
	// being guarded individually. Applying partman's migrations creates a schema,
	// and RegisterParent does not merely record the parent — it provisions the
	// whole premake window on the spot. Either would make "changes nothing" a
	// lie, and a dry run that quietly creates fifteen tables is worse than no dry
	// run at all.
	if !cfg.DryRun {
		mgr, err := newManager(ctx, pool)
		if err != nil {
			return err
		}
		if err := ensureRegistered(ctx, mgr, cfg, out); err != nil {
			return err
		}

		if cfg.Backfill {
			if _, err := backfillChildren(ctx, pool, cfg, out); err != nil {
				return err
			}
			// gopartman's PartitionData is deliberately not called here.
			// backfillChildren moves the rows in the same transaction that creates
			// the children — it has to, because Postgres will not create a bounded
			// partition while the default holds rows for it — so by this point
			// there is nothing left to drain.
		}

		// Deliberately outside our advisory lock: Maintain takes the same
		// per-parent lock itself and, failing to get it, logs and skips the
		// parent. Holding the lock across this call would silently turn
		// provisioning off while looking entirely healthy.
		if err := mgr.Maintain(ctx); err != nil {
			return fmt.Errorf("provisioning: %w", err)
		}
	} else if cfg.Backfill {
		if _, err := backfillChildren(ctx, pool, cfg, out); err != nil {
			return err
		}
	}

	if err := reportPartitions(ctx, pool, cfg, out); err != nil {
		return err
	}

	// Now take it, for the destructive half only. gopartman is done with the
	// ledger by this point, so the lock is doing its real job: keeping a second
	// pg-partition (a hand-run one, say, overlapping the launchd fire) from
	// folding the same partition twice.
	unlock, held, err := lockParent(ctx, pool, cfg.Schema, "ledger")
	if err != nil {
		return err
	}
	if !held {
		// Not an error: another pass is doing the work.
		fmt.Fprintln(out, "another pass holds the maintenance lock; skipping retention")
		return nil
	}
	defer unlock()

	return retain(ctx, pool, cfg, out)
}

// newManager builds the gopartman manager. It is configured to do exactly one
// job — create tomorrow's child table — and specifically not to expire anything:
//
//   - WithHook returning HookSkip vetoes every drop candidate, so Sweep can
//     never touch the ledger. Expiry is ours, because it has to fold per-user
//     totals into ledger_opening in the same transaction as the DETACH, and a
//     pre-drop hook has no way to do that.
//   - The RetentionPeriod below is therefore inert, but it still may not be
//     zero: gopartman reads zero as "no cutoff", and ListExpiredPartitions'
//     bounds_to <= now filter then matches every past partition. With a nil hook
//     the default decision is HookDrop, so zero means "drop all history", not
//     "retention off". The hook already makes that unreachable; this is the
//     second lock on the same door.
func newManager(ctx context.Context, pool *pgxpool.Pool) (*gopartman.Manager, error) {
	for _, m := range gopartman.Migrations() {
		// One Exec per file, never split on ';' — the bodies contain dollar-quoted
		// plpgsql. (The same thing that made our own 00005 need goose's
		// StatementBegin markers.)
		if _, err := pool.Exec(ctx, m.SQL); err != nil {
			return nil, fmt.Errorf("partman migration %d (%s): %w", m.Version, m.Name, err)
		}
	}
	mgr, err := gopartman.New(
		gopartman.WithDB(pool),
		gopartman.WithClock(gopartman.NewRealClock()),
		gopartman.WithHook(func(context.Context, gopartman.PartitionRef) gopartman.HookDecision {
			return gopartman.HookSkip
		}),
		// Warnings and above only. The library logs a tick summary at INFO on
		// every run, and this runs nightly forever — the log should be a place
		// where a line appearing means something happened.
		gopartman.WithLogger(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
			Level: slog.LevelWarn,
		}))),
	)
	if err != nil {
		return nil, fmt.Errorf("partman: %w", err)
	}
	return mgr, nil
}

// ensureRegistered registers the ledger and adopts the children migration 00005
// created. Idempotent: from the second run onward RegisterParent returns
// ErrParentAlreadyExists, which is the expected outcome and not a problem.
func ensureRegistered(ctx context.Context, mgr *gopartman.Manager, cfg Config, out io.Writer) error {
	err := mgr.RegisterParent(ctx, gopartman.ParentConfig{
		SchemaName:        cfg.Schema,
		TableName:         "ledger",
		PartitionBy:       "ts_at",
		PartitionInterval: gopartman.PartitionDayInterval,
		Premake:           cfg.Premake,
		RetentionPeriod:   cfg.Retention, // inert; see newManager
	})
	switch {
	case err == nil:
		fmt.Fprintf(out, "registered %s.ledger (daily, premake %d)\n", cfg.Schema, cfg.Premake)
	case errors.Is(err, gopartman.ErrParentAlreadyExists):
		// Already registered by a previous run.
	default:
		return fmt.Errorf("registering the ledger: %w", err)
	}

	report, err := mgr.ImportExisting(ctx, cfg.ref())
	if err != nil {
		return fmt.Errorf("adopting existing partitions: %w", err)
	}
	if n := len(report.Imported); n > 0 {
		fmt.Fprintf(out, "adopted %d existing partitions\n", n)
	}
	// Skipped and Drifted are reported loudly rather than logged at debug,
	// because both mean a partition that retention will never consider: a
	// non-conforming name is invisible to the expiry query, and a drifted one
	// holds different rows than its name claims. Neither is self-healing.
	for _, s := range report.Skipped {
		fmt.Fprintf(out, "  WARNING: %s skipped (%s) — it will never expire\n", s.Name, s.Reason)
	}
	for _, d := range report.Drifted {
		fmt.Fprintf(out, "  WARNING: %s has drifted: name says %v, PG says %s (%s)\n",
			d.Name, d.NameBounds, d.ActualBound, d.Reason)
	}
	for _, o := range report.Orphaned {
		fmt.Fprintf(out, "  WARNING: metadata row %v has no partition in PG\n", o)
	}
	return nil
}

// backfillChildren creates a daily child for every day between the oldest row
// still sitting in ledger_default and today.
//
// This is a recovery tool, not a routine step. It exists for two situations:
// the launchd agent stopped firing for longer than Premake days, or a fresh
// store-migrate cutover copied a year of history into a database where
// migration 00005 had no data to derive children from. In both cases the rows
// are in ledger_default, and nothing else can rescue them — gopartman's
// provisioner only ever builds {current} u {premake futures} and never reaches
// into the past, and PartitionData refuses to drain into a partition that does
// not exist.
func backfillChildren(ctx context.Context, pool *pgxpool.Pool, cfg Config, out io.Writer) ([]string, error) {
	var days []time.Time
	rows, err := pool.Query(ctx, `
		SELECT generate_series(
		         (SELECT MIN(ts_at) AT TIME ZONE 'UTC' FROM `+quoteQualified(cfg.Schema, "ledger_default")+`)::date,
		         (now() AT TIME ZONE 'UTC')::date,
		         interval '1 day')::date`)
	if err != nil {
		return nil, fmt.Errorf("finding stranded days: %w", err)
	}
	for rows.Next() {
		var d time.Time
		if err := rows.Scan(&d); err != nil {
			rows.Close()
			return nil, err
		}
		days = append(days, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("finding stranded days: %w", err)
	}
	if len(days) == 0 {
		fmt.Fprintln(out, "backfill: ledger_default is empty, nothing to do")
		return nil, nil
	}

	var created []string
	for _, d := range days {
		created = append(created, "ledger_"+d.Format("20060102"))
	}
	if cfg.DryRun {
		fmt.Fprintf(out, "backfill: would create %d children (%s … %s) and move the "+
			"default partition's rows into them\n", len(created), created[0], created[len(created)-1])
		return created, nil
	}

	// The order here is forced by Postgres, and the obvious order does not work.
	//
	// CREATE TABLE ... PARTITION OF is refused while the default partition holds
	// any row that would belong in the new bounds ("updated partition constraint
	// for default partition would be violated by some row"). So the children
	// cannot be created until the rows leave — and the rows cannot be routed
	// anywhere until the children exist. The way out is to detach the default
	// first, which makes it an ordinary table that constrains nothing:
	//
	//	 1. DETACH the default        — it becomes a plain table
	//	 2. create the missing children against a parent with no default
	//	 3. INSERT ... SELECT the rows back through the parent, which routes each
	//	    one into its day, and delete what moved
	//	 4. ATTACH the default again
	//
	// All in one transaction, so a failure anywhere leaves the table exactly as
	// it was. That also makes gopartman's PartitionData unnecessary here: by the
	// time this commits there is nothing left to drain.
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	// Same reason migration 00005 pins it: bounds are timestamptz and are read in
	// the session's zone while the names come from UTC dates.
	if _, err := tx.Exec(ctx, `SET LOCAL TimeZone = 'UTC'`); err != nil {
		return nil, err
	}

	parent := quoteQualified(cfg.Schema, "ledger")
	def := quoteQualified(cfg.Schema, "ledger_default")
	if _, err := tx.Exec(ctx, fmt.Sprintf(`ALTER TABLE %s DETACH PARTITION %s`, parent, def)); err != nil {
		return nil, fmt.Errorf("detaching the default partition: %w", err)
	}
	for i, d := range days {
		if _, err := tx.Exec(ctx, fmt.Sprintf(
			`CREATE TABLE IF NOT EXISTS %s PARTITION OF %s FOR VALUES FROM ('%s') TO ('%s')`,
			quoteQualified(cfg.Schema, created[i]), parent,
			d.Format("2006-01-02"), d.AddDate(0, 0, 1).Format("2006-01-02"))); err != nil {
			return nil, fmt.Errorf("creating %s: %w", created[i], err)
		}
	}

	lo := days[0]
	hi := days[len(days)-1].AddDate(0, 0, 1)
	moved, err := tx.Exec(ctx, fmt.Sprintf(`
		WITH moved AS (
		    DELETE FROM %s WHERE ts_at >= '%s' AND ts_at < '%s'
		    RETURNING id, user_id, delta, reason, ref, ts, ts_at
		)
		INSERT INTO %s (id, user_id, delta, reason, ref, ts, ts_at)
		SELECT id, user_id, delta, reason, ref, ts, ts_at FROM moved`,
		def, lo.Format("2006-01-02"), hi.Format("2006-01-02"), parent))
	if err != nil {
		return nil, fmt.Errorf("moving rows out of the default partition: %w", err)
	}

	if _, err := tx.Exec(ctx, fmt.Sprintf(
		`ALTER TABLE %s ATTACH PARTITION %s DEFAULT`, parent, def)); err != nil {
		return nil, fmt.Errorf("re-attaching the default partition: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}

	fmt.Fprintf(out, "backfill: created %d children (%s … %s), moved %d rows out of ledger_default\n",
		len(created), created[0], created[len(created)-1], moved.RowsAffected())
	return created, nil
}

// reportPartitions prints the shape of the table after provisioning, and warns
// about anything left in the default partition. A non-empty default means rows
// arrived on a day with no child — which is recoverable with -backfill, but
// only if somebody notices.
func reportPartitions(ctx context.Context, pool *pgxpool.Pool, cfg Config, out io.Writer) error {
	var children int
	var oldest, newest *string
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*),
		       MIN(c.relname) FILTER (WHERE c.relname <> 'ledger_default'),
		       MAX(c.relname) FILTER (WHERE c.relname <> 'ledger_default')
		  FROM pg_class c
		  JOIN pg_inherits i ON i.inhrelid = c.oid
		 WHERE i.inhparent = $1::regclass`,
		quoteQualified(cfg.Schema, "ledger")).Scan(&children, &oldest, &newest); err != nil {
		return fmt.Errorf("listing partitions: %w", err)
	}
	fmt.Fprintf(out, "partitions: %d (%s … %s)\n", children, deref(oldest), deref(newest))

	var stranded int64
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM `+quoteQualified(cfg.Schema, "ledger_default")).Scan(&stranded); err != nil {
		return fmt.Errorf("counting the default partition: %w", err)
	}
	if stranded > 0 {
		fmt.Fprintf(out, "  WARNING: %d rows in ledger_default — they will never expire. "+
			"Run with -backfill.\n", stranded)
	}
	return nil
}

func deref(s *string) string {
	if s == nil {
		return "none"
	}
	return *s
}
