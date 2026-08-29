package main

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"tts/internal/partition"
)

// Config is one run's settings. It is a struct rather than loose arguments
// because pass, parent and the tests all need the same set.
type Config struct {
	DSN       string
	Schema    string
	Premake   int
	Retention time.Duration
	Backfill  bool
	DryRun    bool
}

// parent describes the ledger to the shared partition machinery. The column list
// is what the backfill moves rows with, so it has to stay in step with migration
// 00005 — a column missing here is a column silently dropped from every row
// rescued out of the default partition.
func (c Config) parent() partition.Parent {
	return partition.Parent{
		Schema:    c.Schema,
		Table:     "ledger",
		Key:       "ts_at",
		Cols:      []string{"id", "user_id", "delta", "reason", "ref", "ts", "ts_at"},
		Premake:   c.Premake,
		Retention: c.Retention,
	}
}

// pass runs one full maintenance cycle. Every step is idempotent, so a failed run
// is retried simply by running again — which is what launchd does tomorrow.
func pass(ctx context.Context, cfg Config, out io.Writer) error {
	pool, err := pgxpool.New(ctx, cfg.DSN)
	if err != nil {
		return fmt.Errorf("connecting: %w", err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("connecting: %w", err)
	}

	p := cfg.parent()

	// Provisioning is skipped wholesale under -dry-run, rather than each write
	// being guarded individually. Applying partman's migrations creates a schema,
	// and RegisterParent does not merely record the parent — it provisions the
	// whole premake window on the spot. Either would make "changes nothing" a lie,
	// and a dry run that quietly creates fifteen tables is worse than no dry run at
	// all.
	if !cfg.DryRun {
		mgr, err := partition.NewManager(ctx, pool)
		if err != nil {
			return err
		}
		if err := partition.Register(ctx, mgr, p, out); err != nil {
			return err
		}

		if cfg.Backfill {
			if _, err := partition.Backfill(ctx, pool, p, false, out); err != nil {
				return err
			}
			// gopartman's PartitionData is deliberately not called here. Backfill
			// moves the rows in the same transaction that creates the children — it
			// has to, because Postgres will not create a bounded partition while the
			// default holds rows for it — so by this point there is nothing left to
			// drain.
		}

		// Deliberately outside our advisory lock: Maintain takes the same per-parent
		// lock itself and, failing to get it, logs and skips the parent. Holding the
		// lock across this call would silently turn provisioning off while looking
		// entirely healthy.
		if err := mgr.Maintain(ctx); err != nil {
			return fmt.Errorf("provisioning: %w", err)
		}
	} else if cfg.Backfill {
		if _, err := partition.Backfill(ctx, pool, p, true, out); err != nil {
			return err
		}
	}

	if err := partition.Report(ctx, pool, p, out); err != nil {
		return err
	}

	// Now take it, for the destructive half only. gopartman is done with the
	// ledger by this point, so the lock is doing its real job: keeping a second
	// pg-partition (a hand-run one, say, overlapping the launchd fire) from folding
	// the same partition twice.
	unlock, held, err := partition.Lock(ctx, pool, cfg.Schema, "ledger")
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
