// Package partition is the shared machinery behind the tools that keep this
// database's partitioned tables provisioned and bounded.
//
// It exists because there are two such tables whose folds genuinely differ — the
// ledger folds SUM(delta) into ledger_opening and refuses to drop anything until
// every balance reconciles, the chat log folds message counts into chat_stats
// and gates on nothing, because nothing there is money — while everything around
// those folds is identical: gopartman registration, the UTC bounds handling, the
// advisory lock, the order of DETACH against the aggregate, the orphan sweep,
// and the default-partition rescue.
//
// Two hand-written copies were the alternative and were rejected on the record
// in docs/adr/0002: the UTC-versus-session-TimeZone bug, the non-read-only
// -dry-run, the advisory lock that has to be gopartman's own, and the backfill
// ordering that Postgres forces were all found the hard way. A second copy
// re-opens every one of them.
//
// See docs/adr/0002-ledger-retention-and-partitioning.md and
// docs/adr/0003-chat-log.md.
package partition

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	gopartman "github.com/jirevwe/gopartman"
)

// Parent describes one partitioned table and how it is maintained.
type Parent struct {
	Schema string
	Table  string   // the partitioned parent, e.g. "ledger"
	Key    string   // the partition key column, e.g. "ts_at"
	Cols   []string // every column, in order, for the backfill's row move

	Premake   int
	Retention time.Duration
}

// Qualified is the parent as a quoted schema.table identifier.
func (p Parent) Qualified() string { return QuoteQualified(p.Schema, p.Table) }

// DefaultName is where rows land on a day with no bounded child. The name is not
// a choice: gopartman's drain.PartitionData hardcodes {parent}_default, and
// RegisterParent issues its own CREATE TABLE ... DEFAULT that would collide with
// a differently-named one.
func (p Parent) DefaultName() string { return p.Table + "_default" }

// ChildName is the name gopartman's grammar gives the child holding day d. It
// must match exactly, or ImportExisting silently skips the partition — and a
// skipped partition is one that never expires.
func (p Parent) ChildName(d time.Time) string { return p.Table + "_" + d.Format("20060102") }

// Ref identifies the parent to gopartman.
func (p Parent) Ref() gopartman.ParentRef {
	return gopartman.ParentRef{SchemaName: p.Schema, TableName: p.Table}
}

// How long Lock keeps asking before it concludes another full pass really is
// running. Long enough to outlast a sibling tool's Maintain tick, far too short
// to outlast a fold — which is exactly the distinction it exists to draw.
const (
	lockWait = 15 * time.Second
	lockPoll = 250 * time.Millisecond
)

// Child is one bounded partition: the name it carries and the half-open range it
// holds.
type Child struct {
	Name          string
	From, Through time.Time
}

// Expired reports whether every row this child can hold is older than cutoff.
//
// The bounds are half-open, so the test is on the exclusive upper bound: a
// partition expires once nothing it can hold is still inside the horizon. Using
// the lower bound would expire a partition that is still taking writes.
func (c Child) Expired(cutoff time.Time) bool { return !c.Through.After(cutoff) }

// NewManager builds the gopartman manager. It is configured to do exactly one
// job — create tomorrow's child table — and specifically not to expire anything:
//
//   - WithHook returning HookSkip vetoes every drop candidate, so Sweep can
//     never touch our tables. Expiry is ours, because it has to fold each
//     partition's rows into whatever outlives them in the same transaction as
//     the DETACH, and a pre-drop hook has no way to do that.
//   - The RetentionPeriod callers pass is therefore inert, but it still may not
//     be zero: gopartman reads zero as "no cutoff", and ListExpiredPartitions'
//     bounds_to <= now filter then matches every past partition. With a nil hook
//     the default decision is HookDrop, so zero means "drop all history", not
//     "retention off". The hook already makes that unreachable; this is the
//     second lock on the same door.
func NewManager(ctx context.Context, pool *pgxpool.Pool) (*gopartman.Manager, error) {
	if err := applyPartmanMigrations(ctx, pool); err != nil {
		return nil, err
	}
	mgr, err := gopartman.New(
		gopartman.WithDB(pool),
		gopartman.WithClock(gopartman.NewRealClock()),
		gopartman.WithHook(func(context.Context, gopartman.PartitionRef) gopartman.HookDecision {
			return gopartman.HookSkip
		}),
		// Warnings and above only. The library logs a tick summary at INFO on every
		// run, and this runs nightly forever — the log should be a place where a
		// line appearing means something happened.
		gopartman.WithLogger(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
			Level: slog.LevelWarn,
		}))),
	)
	if err != nil {
		return nil, fmt.Errorf("partman: %w", err)
	}
	return mgr, nil
}

// applyPartmanMigrations brings gopartman's own schema up to date, serialized
// across processes.
//
// The serialization is not decoration. partman's schema is a hardcoded global
// shared by every parent, so two of our tools starting at the same moment apply
// the same migrations to the same catalog rows and Postgres fails one of them
// with "tuple concurrently updated" (XX000). The scheduled agents are fifteen
// minutes apart and cannot collide, so what reaches this are the two cases that
// do: a hand-run pass overlapping the launchd fire, and `go test ./...`, which
// runs the two commands' suites concurrently and found this immediately.
//
// The lock is deliberately ours and global rather than gopartman's per-parent
// one: what is being protected is the library's own schema, which the parents
// share. It is the blocking pg_advisory_lock rather than the try_ variant —
// there is nothing useful to do without the migrations, so waiting a moment
// beats skipping them.
func applyPartmanMigrations(ctx context.Context, pool *pgxpool.Pool) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("acquiring a connection for the partman migration lock: %w", err)
	}
	defer conn.Release()

	// Advisory locks are session-scoped, so every statement below has to run on
	// this one connection rather than through the pool.
	if _, err := conn.Exec(ctx,
		`SELECT pg_advisory_lock(hashtext($1), hashtext($2))`, "gopartman", "migrations"); err != nil {
		return fmt.Errorf("taking the partman migration lock: %w", err)
	}
	defer conn.Exec(context.WithoutCancel(ctx),
		`SELECT pg_advisory_unlock(hashtext($1), hashtext($2))`, "gopartman", "migrations")

	for _, m := range gopartman.Migrations() {
		// One Exec per file, never split on ';' — the bodies contain dollar-quoted
		// plpgsql, the same thing that made our own 00005 need goose's
		// StatementBegin markers.
		if _, err := conn.Exec(ctx, m.SQL); err != nil {
			return fmt.Errorf("partman migration %d (%s): %w", m.Version, m.Name, err)
		}
	}
	return nil
}

// Register registers the parent with gopartman and adopts the children its
// migration created. Idempotent: from the second run onward RegisterParent
// returns ErrParentAlreadyExists, which is the expected outcome and not a
// problem.
func Register(ctx context.Context, mgr *gopartman.Manager, p Parent, out io.Writer) error {
	err := mgr.RegisterParent(ctx, gopartman.ParentConfig{
		SchemaName:        p.Schema,
		TableName:         p.Table,
		PartitionBy:       p.Key,
		PartitionInterval: gopartman.PartitionDayInterval,
		Premake:           p.Premake,
		RetentionPeriod:   p.Retention, // inert; see NewManager
	})
	switch {
	case err == nil:
		fmt.Fprintf(out, "registered %s.%s (daily, premake %d)\n", p.Schema, p.Table, p.Premake)
	case errors.Is(err, gopartman.ErrParentAlreadyExists):
		// Already registered by a previous run.
	default:
		return fmt.Errorf("registering %s: %w", p.Table, err)
	}

	report, err := mgr.ImportExisting(ctx, p.Ref())
	if err != nil {
		return fmt.Errorf("adopting existing partitions: %w", err)
	}
	if n := len(report.Imported); n > 0 {
		fmt.Fprintf(out, "adopted %d existing partitions\n", n)
	}
	// Skipped and Drifted are reported loudly rather than logged at debug, because
	// both mean a partition that retention will never consider: a non-conforming
	// name is invisible to the expiry query, and a drifted one holds different
	// rows than its name claims. Neither is self-healing.
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

// ListChildren returns every bounded child of the parent, oldest name first. The
// default partition is excluded: it has no bounds, so it can never expire and is
// never a fold candidate.
func ListChildren(ctx context.Context, pool *pgxpool.Pool, p Parent) ([]Child, error) {
	rows, err := pool.Query(ctx, `
		SELECT c.relname,
		       (regexp_match(pg_get_expr(c.relpartbound, c.oid), 'FROM \(''([^'']+)''\)'))[1]::timestamptz,
		       (regexp_match(pg_get_expr(c.relpartbound, c.oid), 'TO \(''([^'']+)''\)'))[1]::timestamptz
		  FROM pg_class c
		  JOIN pg_inherits i ON i.inhrelid = c.oid
		 WHERE i.inhparent = $1::regclass
		   AND pg_get_expr(c.relpartbound, c.oid) <> 'DEFAULT'
		 ORDER BY c.relname`, p.Qualified())
	if err != nil {
		return nil, fmt.Errorf("listing partitions: %w", err)
	}
	defer rows.Close()

	var out []Child
	for rows.Next() {
		var c Child
		if err := rows.Scan(&c.Name, &c.From, &c.Through); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing partitions: %w", err)
	}
	return out, nil
}

// Claim inserts a partition's marker row into its folded table and reports
// whether this pass got it. Callers build the statement, because the marker's
// columns differ: the ledger records the delta it folded, the chat log a row
// count. What does not differ is that the claim happens first, inside the same
// transaction as everything else.
type Claim func(ctx context.Context, tx pgx.Tx, child string) (claimed bool, err error)

// Aggregate folds one partition's rows into whatever outlives them, inside the
// transaction that is detaching it.
type Aggregate func(ctx context.Context, tx pgx.Tx, child string) error

// Fold detaches one expired partition and folds its arithmetic in the same
// transaction, so the invariant tying the two together is never observably
// false — not for a millisecond, and not if the process dies mid-pass.
// PostgreSQL has transactional DDL, so that costs nothing.
//
// The order is the design. Claim comes first: if a previous run committed the
// fold and then died before dropping, the claim conflicts and this returns
// skipped, so the pass goes straight to the drop. The alternative is folding the
// same partition's numbers in twice, which is the one arithmetic mistake this
// function must never make.
//
// Detaching before the aggregate takes ACCESS EXCLUSIVE on the parent up front,
// which on a small child at 05:15 with no stream running is milliseconds.
// DETACH CONCURRENTLY cannot run inside a transaction block, which is exactly
// why it is not used.
func Fold(ctx context.Context, pool *pgxpool.Pool, p Parent, child string, claim Claim, agg Aggregate) (skipped bool, err error) {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)

	qualified := QuoteQualified(p.Schema, child)

	claimed, err := claim(ctx, tx, qualified)
	if err != nil {
		return false, err
	}
	if !claimed {
		return true, nil
	}

	if _, err := tx.Exec(ctx, fmt.Sprintf(`ALTER TABLE %s DETACH PARTITION %s`,
		p.Qualified(), qualified)); err != nil {
		return false, err
	}
	if err := agg(ctx, tx, qualified); err != nil {
		return false, err
	}
	return false, tx.Commit(ctx)
}

// DropDetached drops every child already recorded in foldedTable that is no
// longer attached to anything.
//
// It works from the marker table rather than from a list of what this pass just
// folded, so it also cleans up orphans: a crash between a fold's COMMIT and its
// DROP leaves a detached table that no longer answers SELECT against the parent
// but is still in pg_dump, quietly growing every backup.
func DropDetached(ctx context.Context, pool *pgxpool.Pool, schema, foldedTable string, dryRun bool, out io.Writer) ([]string, error) {
	rows, err := pool.Query(ctx, `
		SELECT f.name
		  FROM `+QuoteQualified(schema, foldedTable)+` f
		  JOIN pg_class c ON c.relname = f.name
		  JOIN pg_namespace n ON n.oid = c.relnamespace AND n.nspname = $1
		 WHERE NOT EXISTS (SELECT 1 FROM pg_inherits i WHERE i.inhrelid = c.oid)
		 ORDER BY f.name`, schema)
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

	if dryRun {
		for _, n := range names {
			fmt.Fprintf(out, "would drop %s\n", n)
		}
		return names, nil
	}
	for _, n := range names {
		if _, err := pool.Exec(ctx, `DROP TABLE `+QuoteQualified(schema, n)); err != nil {
			return nil, fmt.Errorf("dropping %s: %w", n, err)
		}
	}
	return names, nil
}

// Backfill creates a daily child for every day between the oldest row still
// sitting in the default partition and today, and moves those rows into them.
//
// This is a recovery tool, not a routine step. It exists for two situations: the
// launchd agent stopped firing for longer than Premake days, or a fresh
// store-migrate cutover copied history into a database where the migration had
// no data to derive children from. In both cases the rows are in the default
// partition, and nothing else can rescue them — gopartman's provisioner only
// ever builds {current} u {premake futures} and never reaches into the past, and
// PartitionData refuses to drain into a partition that does not exist.
func Backfill(ctx context.Context, pool *pgxpool.Pool, p Parent, dryRun bool, out io.Writer) ([]string, error) {
	def := QuoteQualified(p.Schema, p.DefaultName())

	var days []time.Time
	rows, err := pool.Query(ctx, `
		SELECT generate_series(
		         (SELECT MIN(`+QuoteIdent(p.Key)+`) AT TIME ZONE 'UTC' FROM `+def+`)::date,
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
		fmt.Fprintf(out, "backfill: %s is empty, nothing to do\n", p.DefaultName())
		return nil, nil
	}

	created := make([]string, 0, len(days))
	for _, d := range days {
		created = append(created, p.ChildName(d))
	}
	if dryRun {
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
	// All in one transaction, so a failure anywhere leaves the table exactly as it
	// was. That also makes gopartman's PartitionData unnecessary here: by the time
	// this commits there is nothing left to drain.
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	// Same reason the migrations pin it: bounds are timestamptz and are read in
	// the session's zone while the names come from UTC dates.
	if _, err := tx.Exec(ctx, `SET LOCAL TimeZone = 'UTC'`); err != nil {
		return nil, err
	}

	parent := p.Qualified()
	if _, err := tx.Exec(ctx, fmt.Sprintf(`ALTER TABLE %s DETACH PARTITION %s`, parent, def)); err != nil {
		return nil, fmt.Errorf("detaching the default partition: %w", err)
	}
	for i, d := range days {
		if _, err := tx.Exec(ctx, fmt.Sprintf(
			`CREATE TABLE IF NOT EXISTS %s PARTITION OF %s FOR VALUES FROM ('%s') TO ('%s')`,
			QuoteQualified(p.Schema, created[i]), parent,
			d.Format("2006-01-02"), d.AddDate(0, 0, 1).Format("2006-01-02"))); err != nil {
			return nil, fmt.Errorf("creating %s: %w", created[i], err)
		}
	}

	cols := strings.Join(quoteAll(p.Cols), ", ")
	lo := days[0]
	hi := days[len(days)-1].AddDate(0, 0, 1)
	moved, err := tx.Exec(ctx, fmt.Sprintf(`
		WITH moved AS (
		    DELETE FROM %s WHERE %s >= '%s' AND %s < '%s'
		    RETURNING %s
		)
		INSERT INTO %s (%s)
		SELECT %s FROM moved`,
		def, QuoteIdent(p.Key), lo.Format("2006-01-02"), QuoteIdent(p.Key), hi.Format("2006-01-02"),
		cols, parent, cols, cols))
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

	fmt.Fprintf(out, "backfill: created %d children (%s … %s), moved %d rows out of %s\n",
		len(created), created[0], created[len(created)-1], moved.RowsAffected(), p.DefaultName())
	return created, nil
}

// Report prints the shape of the table after provisioning, and warns about
// anything left in the default partition. A non-empty default means rows arrived
// on a day with no child — recoverable with Backfill, but only if somebody
// notices.
func Report(ctx context.Context, pool *pgxpool.Pool, p Parent, out io.Writer) error {
	var children int
	var oldest, newest *string
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*),
		       MIN(c.relname) FILTER (WHERE c.relname <> $2),
		       MAX(c.relname) FILTER (WHERE c.relname <> $2)
		  FROM pg_class c
		  JOIN pg_inherits i ON i.inhrelid = c.oid
		 WHERE i.inhparent = $1::regclass`,
		p.Qualified(), p.DefaultName()).Scan(&children, &oldest, &newest); err != nil {
		return fmt.Errorf("listing partitions: %w", err)
	}
	fmt.Fprintf(out, "partitions: %d (%s … %s)\n", children, deref(oldest), deref(newest))

	var stranded int64
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM `+QuoteQualified(p.Schema, p.DefaultName())).Scan(&stranded); err != nil {
		return fmt.Errorf("counting the default partition: %w", err)
	}
	if stranded > 0 {
		fmt.Fprintf(out, "  WARNING: %d rows in %s — they will never expire. "+
			"Run with -backfill.\n", stranded, p.DefaultName())
	}
	return nil
}

// Lock takes gopartman's own per-parent advisory lock, so its maintenance and
// our fold can never run against the same table at the same time.
//
// Two things about this are copied from the library rather than invented, and
// both matter:
//
//   - hashtext() is evaluated by Postgres, not in Go. gopartman issues
//     pg_try_advisory_lock(hashtext($1), hashtext($2)) with the schema and table
//     as text. Reimplementing hashtext here would produce a different key, two
//     locks that never contend, and no mutual exclusion whatsoever — a bug that
//     would look exactly like working code.
//   - Advisory locks are session-scoped, so the lock lives on one connection.
//     Taking it through the pool would hand the connection straight back, and
//     the unlock would then run on a different session and quietly fail. So a
//     connection is acquired and held for the duration.
//
// held is false when someone else still has it after lockWait. That is not an
// error: a nightly job overlapping itself means the work is being done, just not
// by this process.
//
// It retries rather than giving up on the first refusal, because there are two
// very different reasons the lock can be busy and only one of them means "skip".
// A second full pass folding partitions holds it for the whole fold, and waiting
// out lockWait will not change the answer. But gopartman's own Maintain takes
// the same per-parent lock for every parent in its registry — which is global
// and holds *both* our tables — so a sibling tool merely provisioning holds it
// for milliseconds. Treating that as "another pass is running" would silently
// skip a night's retention because a different job was mid-tick.
//
// Do NOT hold this across gopartman's Maintain. Maintain takes the same lock per
// parent and, failing to get it, logs and skips — so nesting them would silently
// turn provisioning off.
func Lock(ctx context.Context, pool *pgxpool.Pool, schema, table string) (unlock func(), held bool, err error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("acquiring a connection for the maintenance lock: %w", err)
	}
	deadline := time.Now().Add(lockWait)
	for {
		if err := conn.QueryRow(ctx,
			`SELECT pg_try_advisory_lock(hashtext($1), hashtext($2))`, schema, table).Scan(&held); err != nil {
			conn.Release()
			return nil, false, fmt.Errorf("taking the maintenance lock: %w", err)
		}
		if held || time.Now().After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			conn.Release()
			return nil, false, ctx.Err()
		case <-time.After(lockPoll):
		}
	}
	if !held {
		conn.Release()
		return func() {}, false, nil
	}
	return func() {
		// WithoutCancel so a cancelled run still releases rather than leaving the
		// lock to the connection's eventual teardown.
		conn.Exec(context.WithoutCancel(ctx),
			`SELECT pg_advisory_unlock(hashtext($1), hashtext($2))`, schema, table)
		conn.Release()
	}, true, nil
}

// QuoteQualified builds a double-quoted schema.table identifier.
//
// Partition names cannot be bound as parameters — DDL takes identifiers, not
// values — so they get interpolated into SQL, and interpolation without quoting
// is how injection happens. Every name here is either a constant or read back
// out of pg_class, so the practical risk is nil; the quoting is here so that
// stays true when someone later threads a name in from a flag.
//
// Doubling embedded quotes is the whole of Postgres's escaping rule for
// identifiers.
func QuoteQualified(schema, name string) string {
	return QuoteIdent(schema) + "." + QuoteIdent(name)
}

// QuoteIdent double-quotes one identifier.
func QuoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

func quoteAll(names []string) []string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = QuoteIdent(n)
	}
	return out
}

func deref(s *string) string {
	if s == nil {
		return "none"
	}
	return *s
}
