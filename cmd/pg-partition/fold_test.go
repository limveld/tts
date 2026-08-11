package main

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"tts/store/postgres"
	"tts/store/storetest"
)

// harness builds an isolated schema with the real migrations applied, seeds it
// through the public API, and hands back everything a case needs.
//
// The partman metadata cleanup is not optional. partman is a hardcoded global
// schema, so its parent_tables rows outlive the temp schema that DROP SCHEMA
// CASCADE takes with it — and every later Maintain in the whole package would
// then try to provision into schemas that no longer exist.
func harness(t *testing.T) (Config, *pgxpool.Pool, *postgres.Store) {
	t.Helper()
	base := storetest.PostgresDSN(t)
	dsn := storetest.TempSchemaDSN(t, base)

	s, err := postgres.Open(dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	schema := dsn[strings.LastIndex(dsn, "search_path=")+len("search_path="):]

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(func() {
		pool.Exec(context.Background(),
			`DELETE FROM partman.parent_tables WHERE schema_name = $1`, schema)
		pool.Close()
	})

	return Config{
		DSN:       dsn,
		Schema:    schema,
		Premake:   defaultPremake,
		Retention: defaultRetention,
	}, pool, s
}

// seedAcrossDays writes ledger rows dated into the past. Credit always stamps
// now(), so back-dated history has to be inserted directly — and it has to go
// into partitions that exist, which is what makeDay is for.
func seedAcrossDays(t *testing.T, pool *pgxpool.Pool, cfg Config, days int) {
	t.Helper()
	ctx := context.Background()
	for d := days; d >= 1; d-- {
		day := time.Now().UTC().AddDate(0, 0, -d).Truncate(24 * time.Hour)
		makeDay(t, pool, cfg, day)
		for u := 0; u < 5; u++ {
			user := fmt.Sprintf("u%d", u)
			amount := int64(10 + u)
			if _, err := pool.Exec(ctx, fmt.Sprintf(`
				INSERT INTO %s (user_id, delta, reason, ts, ts_at)
				VALUES ($1, $2, 'accrual', $3::bigint, to_timestamp($3::bigint))`,
				quoteQualified(cfg.Schema, "ledger")),
				user, amount, day.Add(12*time.Hour).Unix()); err != nil {
				t.Fatalf("seeding %s: %v", day.Format(time.DateOnly), err)
			}
			if _, err := pool.Exec(ctx, `
				INSERT INTO accounts (user_id, balance, created_at, updated_at) VALUES ($1, $2, 0, 0)
				ON CONFLICT (user_id) DO UPDATE SET balance = accounts.balance + EXCLUDED.balance`,
				user, amount); err != nil {
				t.Fatalf("seeding balance: %v", err)
			}
		}
	}
}

func makeDay(t *testing.T, pool *pgxpool.Pool, cfg Config, day time.Time) {
	t.Helper()
	name := "ledger_" + day.Format("20060102")
	_, err := pool.Exec(context.Background(), fmt.Sprintf(
		`SET LOCAL TimeZone = 'UTC';
		 CREATE TABLE IF NOT EXISTS %s PARTITION OF %s FOR VALUES FROM ('%s') TO ('%s')`,
		quoteQualified(cfg.Schema, name), quoteQualified(cfg.Schema, "ledger"),
		day.Format("2006-01-02"), day.AddDate(0, 0, 1).Format("2006-01-02")))
	if err != nil {
		t.Fatalf("creating %s: %v", name, err)
	}
}

func balances(t *testing.T, pool *pgxpool.Pool) map[string]int64 {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT user_id, balance FROM accounts`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var id string
		var b int64
		if err := rows.Scan(&id, &b); err != nil {
			t.Fatal(err)
		}
		out[id] = b
	}
	return out
}

func runPass(t *testing.T, cfg Config) (string, error) {
	t.Helper()
	var buf bytes.Buffer
	err := pass(context.Background(), cfg, &buf)
	return buf.String(), err
}

// The property that matters: folding is value-preserving. Everything else in
// this file is about the ways it could fail to be.
func TestFoldKeepsReconcileClean(t *testing.T) {
	cfg, pool, _ := harness(t)
	seedAcrossDays(t, pool, cfg, 10)
	before := balances(t, pool)

	cfg.Retention = 5 * 24 * time.Hour
	out, err := runPass(t, cfg)
	if err != nil {
		t.Fatalf("pass: %v\n%s", err, out)
	}
	if !strings.Contains(out, "reconcile: every balance agrees") {
		t.Errorf("reconcile did not pass:\n%s", out)
	}
	if !strings.Contains(out, "dropped") {
		t.Errorf("nothing was dropped, so the fold was not exercised:\n%s", out)
	}

	after := balances(t, pool)
	if len(after) != len(before) {
		t.Fatalf("user count changed: %d -> %d", len(before), len(after))
	}
	for id, want := range before {
		if after[id] != want {
			t.Errorf("%s: balance moved across the fold, %d -> %d", id, want, after[id])
		}
	}

	// And the arithmetic actually moved into ledger_opening rather than vanishing.
	var opening, remaining, total int64
	if err := pool.QueryRow(context.Background(), `
		SELECT (SELECT COALESCE(SUM(delta), 0) FROM ledger_opening),
		       (SELECT COALESCE(SUM(delta), 0) FROM ledger),
		       (SELECT COALESCE(SUM(balance), 0) FROM accounts)`).Scan(&opening, &remaining, &total); err != nil {
		t.Fatal(err)
	}
	if opening == 0 {
		t.Error("ledger_opening is empty — the fold dropped history without recording it")
	}
	if opening+remaining != total {
		t.Errorf("opening %d + ledger %d != balances %d", opening, remaining, total)
	}
}

func TestFoldIsIdempotent(t *testing.T) {
	cfg, pool, _ := harness(t)
	seedAcrossDays(t, pool, cfg, 10)
	cfg.Retention = 5 * 24 * time.Hour

	if out, err := runPass(t, cfg); err != nil {
		t.Fatalf("first pass: %v\n%s", err, out)
	}
	first := balances(t, pool)
	var openingAfterFirst int64
	pool.QueryRow(context.Background(),
		`SELECT COALESCE(SUM(delta), 0) FROM ledger_opening`).Scan(&openingAfterFirst)

	out, err := runPass(t, cfg)
	if err != nil {
		t.Fatalf("second pass: %v\n%s", err, out)
	}
	if strings.Contains(out, "dropped") {
		t.Errorf("second pass dropped something; there was nothing left to drop:\n%s", out)
	}

	var openingAfterSecond int64
	pool.QueryRow(context.Background(),
		`SELECT COALESCE(SUM(delta), 0) FROM ledger_opening`).Scan(&openingAfterSecond)
	if openingAfterSecond != openingAfterFirst {
		t.Errorf("ledger_opening changed on a re-run: %d -> %d — deltas were folded twice",
			openingAfterFirst, openingAfterSecond)
	}
	for id, want := range first {
		if got := balances(t, pool)[id]; got != want {
			t.Errorf("%s: balance moved on a re-run, %d -> %d", id, want, got)
		}
	}
}

// The crash this has to survive: the fold transaction commits, then the process
// dies before the DROP. The partition is detached, so it is invisible to
// SELECT ... FROM ledger, but it is still in pg_dump — and its deltas are
// already in ledger_opening. Re-folding it would double every one of them.
func TestFoldSurvivesCrashAfterCommit(t *testing.T) {
	cfg, pool, _ := harness(t)
	seedAcrossDays(t, pool, cfg, 10)
	cfg.Retention = 5 * 24 * time.Hour
	ctx := context.Background()

	// Fold, but stop short of dropping — exactly the state a crash leaves.
	folded, err := foldExpired(ctx, pool, cfg, time.Now().UTC().Add(-cfg.Retention), &bytes.Buffer{})
	if err != nil {
		t.Fatalf("fold: %v", err)
	}
	if len(folded) == 0 {
		t.Fatal("nothing folded, so the crash path was never entered")
	}
	before := balances(t, pool)
	var openingBefore int64
	pool.QueryRow(ctx, `SELECT COALESCE(SUM(delta), 0) FROM ledger_opening`).Scan(&openingBefore)

	var orphans int
	pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE n.nspname = $1 AND c.relname LIKE 'ledger_2%'
		   AND NOT EXISTS (SELECT 1 FROM pg_inherits i WHERE i.inhrelid = c.oid)`,
		cfg.Schema).Scan(&orphans)
	if orphans == 0 {
		t.Fatal("expected detached orphans after a fold with no drop")
	}

	out, err := runPass(t, cfg)
	if err != nil {
		t.Fatalf("recovery pass: %v\n%s", err, out)
	}

	var openingAfter int64
	pool.QueryRow(ctx, `SELECT COALESCE(SUM(delta), 0) FROM ledger_opening`).Scan(&openingAfter)
	if openingAfter != openingBefore {
		t.Errorf("ledger_opening changed during recovery: %d -> %d — the orphan was folded twice",
			openingBefore, openingAfter)
	}
	for id, want := range before {
		if got := balances(t, pool)[id]; got != want {
			t.Errorf("%s: balance moved during recovery, %d -> %d", id, want, got)
		}
	}

	pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE n.nspname = $1 AND c.relname LIKE 'ledger_2%'
		   AND NOT EXISTS (SELECT 1 FROM pg_inherits i WHERE i.inhrelid = c.oid)`,
		cfg.Schema).Scan(&orphans)
	if orphans != 0 {
		t.Errorf("%d detached orphans left behind — they stay in every pg_dump", orphans)
	}
}

// The gate. If the books do not balance, nothing may be destroyed — because a
// disagreement means either the balance or the history is wrong, and dropping
// the history removes the only evidence of which.
func TestDropIsGatedOnReconcile(t *testing.T) {
	cfg, pool, _ := harness(t)
	seedAcrossDays(t, pool, cfg, 10)
	cfg.Retention = 5 * 24 * time.Hour
	ctx := context.Background()

	if _, err := pool.Exec(ctx,
		`UPDATE accounts SET balance = balance + 1 WHERE user_id = 'u0'`); err != nil {
		t.Fatal(err)
	}

	out, err := runPass(t, cfg)
	if err == nil {
		t.Fatalf("pass succeeded with books that do not balance:\n%s", out)
	}
	if !strings.Contains(err.Error(), "disagree") {
		t.Errorf("error should name the disagreement; got: %v", err)
	}
	if strings.Contains(out, "dropped") {
		t.Errorf("a partition was dropped despite a dirty reconcile:\n%s", out)
	}

	// The folded partitions are detached but still present, so nothing is lost
	// and the next run drops them once the books are fixed.
	var present int
	pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE n.nspname = $1 AND c.relname LIKE 'ledger_2%'`, cfg.Schema).Scan(&present)
	if present == 0 {
		t.Error("no partitions left at all — the gate did not protect anything")
	}
}

func TestDryRunChangesNothing(t *testing.T) {
	cfg, pool, _ := harness(t)
	seedAcrossDays(t, pool, cfg, 10)
	cfg.Retention = 5 * 24 * time.Hour
	cfg.DryRun = true
	ctx := context.Background()

	snapshot := func() (rels int, partmanSchemas int, ledgerRows int64) {
		pool.QueryRow(ctx, `
			SELECT COUNT(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
			 WHERE n.nspname = $1`, cfg.Schema).Scan(&rels)
		pool.QueryRow(ctx,
			`SELECT COUNT(*) FROM information_schema.schemata WHERE schema_name = 'partman'`).Scan(&partmanSchemas)
		pool.QueryRow(ctx, `SELECT COUNT(*) FROM ledger`).Scan(&ledgerRows)
		return
	}

	r1, p1, l1 := snapshot()
	out, err := runPass(t, cfg)
	if err != nil {
		t.Fatalf("dry run: %v\n%s", err, out)
	}
	r2, p2, l2 := snapshot()

	if r1 != r2 {
		t.Errorf("dry run changed the relation count: %d -> %d", r1, r2)
	}
	if p1 != p2 {
		t.Errorf("dry run created the partman schema: %d -> %d", p1, p2)
	}
	if l1 != l2 {
		t.Errorf("dry run changed the ledger: %d -> %d rows", l1, l2)
	}
	if !strings.Contains(out, "would fold") {
		t.Errorf("dry run should say what it would do:\n%s", out)
	}
}

// The migration and gopartman agree on partition names only by convention —
// {parent}_YYYYMMDD, UTC. Nothing enforces it, and a mismatch is silent:
// ImportExisting reports the children as Skipped and they simply never expire.
// This is the test that makes the convention real.
func TestRegisterIsIdempotentAgainstTheMigration(t *testing.T) {
	cfg, pool, _ := harness(t)
	seedAcrossDays(t, pool, cfg, 3)
	ctx := context.Background()

	mgr, err := newManager(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := ensureRegistered(ctx, mgr, cfg, &buf); err != nil {
		t.Fatalf("first register: %v", err)
	}
	if err := ensureRegistered(ctx, mgr, cfg, &buf); err != nil {
		t.Fatalf("second register: %v", err)
	}
	if strings.Contains(buf.String(), "WARNING") {
		t.Errorf("ImportExisting reported a problem — the migration's partition names "+
			"do not match gopartman's grammar, so they would never expire:\n%s", buf.String())
	}

	// Exactly one DEFAULT partition: RegisterParent creates its own, and it has
	// to be the same one migration 00005 made.
	var defaults int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM pg_class c
		  JOIN pg_inherits i ON i.inhrelid = c.oid
		 WHERE i.inhparent = $1::regclass
		   AND pg_get_expr(c.relpartbound, c.oid) = 'DEFAULT'`,
		quoteQualified(cfg.Schema, "ledger")).Scan(&defaults); err != nil {
		t.Fatal(err)
	}
	if defaults != 1 {
		t.Errorf("%d default partitions, want exactly 1", defaults)
	}
}

// Rows stranded in the default partition can never expire: retention only
// considers registered bounded partitions. gopartman cannot rescue them either —
// its provisioner never reaches into the past, and PartitionData refuses to
// drain into a partition that does not exist. -backfill is the only thing that
// can, which makes this the recovery path for a re-cutover.
func TestBackfillRescuesTheDefaultPartition(t *testing.T) {
	cfg, pool, _ := harness(t)
	ctx := context.Background()

	// History with no matching child: it lands in ledger_default, which is
	// exactly what a fresh store-migrate of a year of data produces.
	for d := 20; d >= 15; d-- {
		day := time.Now().UTC().AddDate(0, 0, -d).Truncate(24 * time.Hour)
		if _, err := pool.Exec(ctx, fmt.Sprintf(`
			INSERT INTO %s (user_id, delta, reason, ts, ts_at)
			VALUES ('u0', 10, 'accrual', $1::bigint, to_timestamp($1::bigint))`,
			quoteQualified(cfg.Schema, "ledger")), day.Add(12*time.Hour).Unix()); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO accounts (user_id, balance, created_at, updated_at) VALUES ('u0', 60, 0, 0)`); err != nil {
		t.Fatal(err)
	}

	var stranded int64
	pool.QueryRow(ctx, `SELECT COUNT(*) FROM `+quoteQualified(cfg.Schema, "ledger_default")).Scan(&stranded)
	if stranded != 6 {
		t.Fatalf("%d rows in the default partition, want 6 — the setup did not strand them", stranded)
	}

	cfg.Backfill = true
	out, err := runPass(t, cfg)
	if err != nil {
		t.Fatalf("backfill pass: %v\n%s", err, out)
	}

	pool.QueryRow(ctx, `SELECT COUNT(*) FROM `+quoteQualified(cfg.Schema, "ledger_default")).Scan(&stranded)
	if stranded != 0 {
		t.Errorf("%d rows still in the default partition after -backfill:\n%s", stranded, out)
	}
	var total int64
	pool.QueryRow(ctx, `SELECT COUNT(*) FROM ledger`).Scan(&total)
	if total != 6 {
		t.Errorf("ledger has %d rows after the drain, want 6 — rows were lost", total)
	}
}
