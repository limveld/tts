package main

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"tts/internal/partition"
)

// Config is one run's settings.
type Config struct {
	DSN       string
	Schema    string
	Premake   int
	Retention time.Duration
	Backfill  bool
	PurgeUser string
	DryRun    bool
}

// parent describes chat_message to the shared partition machinery. The column
// list is what the backfill moves rows with, so it has to stay in step with
// migration 00006 — a column missing here is a column silently dropped from
// every row rescued out of the default partition.
func (c Config) parent() partition.Parent {
	return partition.Parent{
		Schema: c.Schema,
		Table:  "chat_message",
		Key:    "ts_at",
		Cols: []string{
			"id", "ts", "ts_at", "room_id", "msg_id", "user_id", "login", "display",
			"text", "emotes", "is_mod", "is_sub", "is_vip", "is_broadcaster",
			"deleted_at", "deleted_by",
		},
		Premake:   c.Premake,
		Retention: c.Retention,
	}
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

	p := cfg.parent()

	// Provisioning is skipped wholesale under -dry-run rather than each write
	// being guarded individually: RegisterParent does not merely record the
	// parent, it provisions the whole premake window on the spot. A dry run that
	// quietly creates fifteen tables is worse than no dry run at all — ADR-0002
	// records finding that out on the ledger's version of this tool.
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
		}

		// Deliberately outside the advisory lock taken below: Maintain takes the
		// same per-parent lock itself and, failing to get it, logs and skips the
		// parent. Holding the lock across this call would silently turn
		// provisioning off while looking entirely healthy.
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

	// The lock is per-parent, so this one is chat_message's and never contends
	// with pg-partition's on the ledger. It is here to stop a second
	// chat-partition — a hand-run one overlapping the launchd fire — from folding
	// the same partition twice.
	unlock, held, err := partition.Lock(ctx, pool, cfg.Schema, "chat_message")
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
