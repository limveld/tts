package postgres

import (
	"context"
	"testing"
	"time"

	"tts/store/storetest"
)

// wantSchemaVersion is the highest migration in migrations/. It must stay in step
// with store/sqlite's — the two dialects are meant to describe the same schema,
// and the migrate tool prints both.
const wantSchemaVersion = 5

func schemaVersion(t *testing.T, s *Store) int64 {
	t.Helper()
	var v int64
	if err := s.db.QueryRow(
		`SELECT MAX(version_id) FROM goose_db_version WHERE is_applied = true`).Scan(&v); err != nil {
		t.Fatalf("read goose_db_version: %v", err)
	}
	return v
}

func TestMigrateFreshSchema(t *testing.T) {
	dsn := storetest.TempSchemaDSN(t, storetest.PostgresDSN(t))

	s, err := Open(dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	if v := schemaVersion(t, s); v != wantSchemaVersion {
		t.Errorf("goose version=%d want %d", v, wantSchemaVersion)
	}

	// The version table must land inside the temp schema. If search_path didn't
	// take, it leaks into public and the next test thinks it's already migrated —
	// which shows up as a baffling "table does not exist" much later.
	var schema string
	if err := s.db.QueryRow(
		`SELECT table_schema FROM information_schema.tables
		 WHERE table_name = 'goose_db_version' AND table_schema = current_schema()`).Scan(&schema); err != nil {
		t.Fatalf("goose_db_version is not in the temp schema: %v", err)
	}

	// Every table the two migrations create is present and usable.
	for _, table := range []string{
		"commands", "users", "ledger", "settings",
		"wordle_wins", "connections_wins", "accounts", "game_rounds",
		"ledger_refs", "ledger_opening", "ledger_folded",
	} {
		var n int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
			t.Errorf("table %s: %v", table, err)
		}
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	dsn := storetest.TempSchemaDSN(t, storetest.PostgresDSN(t))

	s, err := Open(dsn)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	if _, err := s.Credit("u1", 100, "accrual", ""); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s2, err := Open(dsn)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	t.Cleanup(func() { s2.Close() })
	if v := schemaVersion(t, s2); v != wantSchemaVersion {
		t.Errorf("goose version after reopen=%d want %d", v, wantSchemaVersion)
	}
	if b, err := s2.Balance("u1"); err != nil || b != 100 {
		t.Errorf("balance after reopen=%d err=%v want 100", b, err)
	}
}

// The ledger's identity column must be BY DEFAULT, not ALWAYS: cmd/store-migrate
// inserts explicit ids so ledger row ids survive the cutover copy. With ALWAYS
// this insert fails, and it would fail for the first time during the cutover.
func TestLedgerIdentityAcceptsExplicitIDs(t *testing.T) {
	s, err := Open(storetest.TempSchemaDSN(t, storetest.PostgresDSN(t)))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	if _, err := s.db.Exec(
		`INSERT INTO ledger (id, user_id, delta, reason, ts, ts_at) VALUES (9000, 'u1', 5, 'copy', 1, to_timestamp(1))`); err != nil {
		t.Fatalf("explicit id rejected — identity is ALWAYS, not BY DEFAULT: %v", err)
	}
	var got int64
	if err := s.db.QueryRow(`SELECT id FROM ledger WHERE user_id = 'u1'`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != 9000 {
		t.Errorf("id=%d want the explicit 9000", got)
	}
}

// The partitioning migration has to have actually partitioned the table. Asserted
// against the catalog rather than by inserting and hoping: a plain table accepts
// every insert these tests make.
func TestLedgerIsPartitionedByRange(t *testing.T) {
	s, err := Open(storetest.TempSchemaDSN(t, storetest.PostgresDSN(t)))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	var relkind, strategy string
	if err := s.db.QueryRow(
		`SELECT c.relkind, p.partstrat
		   FROM pg_class c JOIN pg_partitioned_table p ON p.partrelid = c.oid
		  WHERE c.oid = 'ledger'::regclass`).Scan(&relkind, &strategy); err != nil {
		t.Fatalf("ledger is not a partitioned table: %v", err)
	}
	if relkind != "p" || strategy != "r" {
		t.Errorf("relkind=%q partstrat=%q want \"p\"/\"r\"", relkind, strategy)
	}

	var key string
	if err := s.db.QueryRow(
		`SELECT pg_get_partkeydef('ledger'::regclass)`).Scan(&key); err != nil {
		t.Fatal(err)
	}
	if key != "RANGE (ts_at)" {
		t.Errorf("partition key=%q want RANGE (ts_at)", key)
	}
}

// gopartman's drain hardcodes {parent}_default and RegisterParent creates its own
// DEFAULT partition, so a differently-named one here means a collision two issues
// later — at which point the cause is a long way from the symptom.
func TestLedgerDefaultPartitionExists(t *testing.T) {
	s, err := Open(storetest.TempSchemaDSN(t, storetest.PostgresDSN(t)))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	var name string
	if err := s.db.QueryRow(
		`SELECT c.relname FROM pg_class c
		   JOIN pg_inherits i ON i.inhrelid = c.oid
		  WHERE i.inhparent = 'ledger'::regclass
		    AND pg_get_expr(c.relpartbound, c.oid) = 'DEFAULT'`).Scan(&name); err != nil {
		t.Fatalf("no default partition on ledger: %v", err)
	}
	if name != "ledger_default" {
		t.Errorf("default partition is %q, want exactly \"ledger_default\"", name)
	}
}

// Nothing in the schema can enforce ts_at = to_timestamp(ts): a generated column
// cannot be a partition key and a BEFORE INSERT trigger fires after tuple
// routing. So the invariant is only as good as every INSERT site remembering,
// and this is what remembers for them. Driven through the public API so it covers
// all of them.
func TestEveryLedgerRowHasTsAtMatchingTs(t *testing.T) {
	s, err := Open(storetest.TempSchemaDSN(t, storetest.PostgresDSN(t)))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	// Errors checked rather than ignored: a write path that fails outright would
	// otherwise show up here only as the "wrote nothing" guard, which points at
	// the test instead of at the INSERT that broke.
	for _, step := range []struct {
		what string
		err  error
	}{
		{"Credit", first(s.Credit("u1", 100, "accrual", ""))},
		{"Credit(ref)", first(s.Credit("u1", 50, "redemption", "ref-1"))},
		{"Grant", first(s.Grant("u1", 40, "grant"))},
		{"Grant(clamped)", first(s.Grant("u1", -1_000_000, "clawback"))},
		{"Spend", first(s.Spend("u1", 10, "tts"))},
		{"Transfer", first(s.Transfer("u1", "u2", 5, "give"))},
	} {
		if step.err != nil {
			t.Fatalf("%s: %v", step.what, step.err)
		}
	}

	var total, mismatched int
	if err := s.db.QueryRow(
		`SELECT COUNT(*), COUNT(*) FILTER (WHERE ts_at <> to_timestamp(ts)) FROM ledger`,
	).Scan(&total, &mismatched); err != nil {
		t.Fatal(err)
	}
	if total == 0 {
		t.Fatal("no ledger rows — the workload wrote nothing, so this proved nothing")
	}
	if mismatched != 0 {
		t.Errorf("%d of %d ledger rows have ts_at out of step with ts", mismatched, total)
	}
}

// Partition bounds are timestamptz and are therefore read in the session's
// TimeZone, but the partition *names* come from UTC dates. On a cluster running
// anything but UTC — this one is Europe/Malta — a migration that forgets
// SET LOCAL TimeZone = 'UTC' puts every child hours away from the day it is named
// after, and rows near midnight UTC leak into the default partition, which is
// exactly where retention can never reach them.
func TestPartitionBoundsAreUTC(t *testing.T) {
	s, err := Open(storetest.TempSchemaDSN(t, storetest.PostgresDSN(t)))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	// 23:30 UTC today: inside today's UTC day, but outside it under any positive
	// offset. Inserted directly, because Credit always stamps now().
	var landedIn string
	if err := s.db.QueryRow(
		`WITH t AS (
		   SELECT (date_trunc('day', now() AT TIME ZONE 'UTC') + interval '23 hours 30 minutes')
		            AT TIME ZONE 'UTC' AS at
		 )
		 INSERT INTO ledger (user_id, delta, reason, ts, ts_at)
		 SELECT 'edge', 1, 'accrual', extract(epoch FROM t.at)::bigint, t.at FROM t
		 RETURNING tableoid::regclass::text`).Scan(&landedIn); err != nil {
		t.Fatal(err)
	}

	want := "ledger_" + time.Now().UTC().Format("20060102")
	if landedIn != want {
		t.Errorf("a row at 23:30 UTC landed in %s, want %s — partition bounds are not UTC", landedIn, want)
	}
}

// first discards a two-result call's first value and keeps its error, so a table
// of write-path calls with differing first results stays readable.
func first[T any](_ T, err error) error { return err }

// A down migration nobody has run is a rollback plan nobody has. 00005 rewrites
// the whole ledger, so its reverse is the one most worth exercising: it has to
// give back a plain table with every row, the right sequence position and no
// ts_at.
func TestPartitionMigrationIsReversible(t *testing.T) {
	dsn := storetest.TempSchemaDSN(t, storetest.PostgresDSN(t))
	s, err := Open(dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	for i := 0; i < 3; i++ {
		if _, err := s.Credit("u1", 100, "accrual", ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Grant("u2", 250, "grant"); err != nil {
		t.Fatal(err)
	}

	var wantRows, wantMax, wantTotal int64
	if err := s.db.QueryRow(
		`SELECT (SELECT COUNT(*) FROM ledger), (SELECT MAX(id) FROM ledger),
		        (SELECT SUM(balance) FROM accounts)`).Scan(&wantRows, &wantMax, &wantTotal); err != nil {
		t.Fatal(err)
	}

	p, err := provider(s.db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Down(context.Background()); err != nil {
		t.Fatalf("down: %v", err)
	}

	var relkind string
	if err := s.db.QueryRow(
		`SELECT relkind::text FROM pg_class WHERE oid = 'ledger'::regclass`).Scan(&relkind); err != nil {
		t.Fatal(err)
	}
	if relkind != "r" {
		t.Errorf("relkind after down=%q want \"r\" (an ordinary table)", relkind)
	}

	var tsAtCols int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM information_schema.columns
		  WHERE table_schema = current_schema() AND table_name = 'ledger' AND column_name = 'ts_at'`,
	).Scan(&tsAtCols); err != nil {
		t.Fatal(err)
	}
	if tsAtCols != 0 {
		t.Error("ts_at survived the down migration")
	}

	var rows, max, total int64
	if err := s.db.QueryRow(
		`SELECT (SELECT COUNT(*) FROM ledger), (SELECT MAX(id) FROM ledger),
		        (SELECT SUM(balance) FROM accounts)`).Scan(&rows, &max, &total); err != nil {
		t.Fatal(err)
	}
	if rows != wantRows || max != wantMax || total != wantTotal {
		t.Errorf("after down: rows=%d max=%d total=%d, want %d/%d/%d",
			rows, max, total, wantRows, wantMax, wantTotal)
	}

	// The sequence has to come back too, or the first Credit after a rollback
	// collides — the same trap the forward direction has.
	if _, err := p.Up(context.Background()); err != nil {
		t.Fatalf("re-up: %v", err)
	}
	if _, err := s.Credit("u3", 5, "accrual", ""); err != nil {
		t.Fatalf("credit after down-then-up: %v", err)
	}
}
