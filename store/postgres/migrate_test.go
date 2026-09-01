package postgres

import (
	"context"
	"strconv"
	"testing"
	"time"

	"tts/store"
	"tts/store/storetest"
)

// wantSchemaVersion is the highest migration in migrations/. It must stay in step
// with store/sqlite's — the two dialects are meant to describe the same schema,
// and the migrate tool prints both.
const wantSchemaVersion = 7

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
		"wordle_wins", "connections_wins", "maze_wins", "accounts", "game_rounds",
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
	// DownTo(4) rather than Down(): Down reverses exactly one migration, and the
	// newest is no longer 00005. Naming the target version keeps this test aimed
	// at the ledger rewrite however many migrations land on top of it.
	if _, err := p.DownTo(context.Background(), 4); err != nil {
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

// The chat log is the ledger's second partitioned parent, and every trap 00005
// hit applies to it unchanged. These are the ledger assertions above, retargeted
// — deliberately duplicated rather than shared, because the day one of the two
// tables stops being partitioned is a day a shared helper would go quiet about.

func TestChatMessageIsPartitionedByRange(t *testing.T) {
	s, err := Open(storetest.TempSchemaDSN(t, storetest.PostgresDSN(t)))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	var relkind, strategy string
	if err := s.db.QueryRow(
		`SELECT c.relkind, p.partstrat
		   FROM pg_class c JOIN pg_partitioned_table p ON p.partrelid = c.oid
		  WHERE c.oid = 'chat_message'::regclass`).Scan(&relkind, &strategy); err != nil {
		t.Fatalf("chat_message is not a partitioned table: %v", err)
	}
	if relkind != "p" || strategy != "r" {
		t.Errorf("relkind=%q partstrat=%q want \"p\"/\"r\"", relkind, strategy)
	}

	var key string
	if err := s.db.QueryRow(
		`SELECT pg_get_partkeydef('chat_message'::regclass)`).Scan(&key); err != nil {
		t.Fatal(err)
	}
	if key != "RANGE (ts_at)" {
		t.Errorf("partition key=%q want RANGE (ts_at)", key)
	}
}

func TestChatMessageDefaultPartitionExists(t *testing.T) {
	s, err := Open(storetest.TempSchemaDSN(t, storetest.PostgresDSN(t)))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	var name string
	if err := s.db.QueryRow(
		`SELECT c.relname FROM pg_class c
		   JOIN pg_inherits i ON i.inhrelid = c.oid
		  WHERE i.inhparent = 'chat_message'::regclass
		    AND pg_get_expr(c.relpartbound, c.oid) = 'DEFAULT'`).Scan(&name); err != nil {
		t.Fatalf("no default partition on chat_message: %v", err)
	}
	if name != "chat_message_default" {
		t.Errorf("default partition is %q, want exactly \"chat_message_default\"", name)
	}
}

// The migration seeds today plus a 14-day premake window, because the bot starts
// writing the moment it comes up and cmd/chat-partition does not run until 05:30
// the next morning. Without these the first night of chat lands in the default
// partition, where retention can never reach it.
func TestChatMessagePremakeWindowExists(t *testing.T) {
	s, err := Open(storetest.TempSchemaDSN(t, storetest.PostgresDSN(t)))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	day := time.Now().UTC()
	for i := 0; i <= 14; i++ {
		want := "chat_message_" + day.AddDate(0, 0, i).Format("20060102")
		var n int
		if err := s.db.QueryRow(
			`SELECT COUNT(*) FROM pg_class c
			   JOIN pg_inherits i ON i.inhrelid = c.oid
			  WHERE i.inhparent = 'chat_message'::regclass AND c.relname = $1`, want).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 1 {
			t.Errorf("no partition %s (day +%d of the premake window)", want, i)
		}
	}
}

// The same invariant 00005 documents for the ledger: nothing in the schema can
// enforce ts_at = to_timestamp(ts), so it is only as good as every INSERT site
// remembering. Driven through the public API so it covers all of them.
func TestEveryChatRowHasTsAtMatchingTs(t *testing.T) {
	s, err := Open(storetest.TempSchemaDSN(t, storetest.PostgresDSN(t)))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	now := time.Now().UTC().Unix()
	batch := make([]store.ChatMessage, 0, 3)
	for i := range 3 {
		batch = append(batch, store.ChatMessage{
			TS: now - int64(i), RoomID: "room1", MsgID: "m" + strconv.Itoa(i),
			UserID: "u1", Login: "bob", Display: "Bob", Text: "hi",
		})
	}
	if err := s.LogMessages(batch); err != nil {
		t.Fatal(err)
	}

	var total, mismatched int
	if err := s.db.QueryRow(
		`SELECT COUNT(*), COUNT(*) FILTER (WHERE ts_at <> to_timestamp(ts)) FROM chat_message`,
	).Scan(&total, &mismatched); err != nil {
		t.Fatal(err)
	}
	if total == 0 {
		t.Fatal("no chat rows — the workload wrote nothing, so this proved nothing")
	}
	if mismatched != 0 {
		t.Errorf("%d of %d chat rows have ts_at out of step with ts", mismatched, total)
	}
}

// TestPartitionBoundsAreUTC's twin. The cluster is Europe/Malta, so a migration
// that forgets SET LOCAL TimeZone = 'UTC' puts every child two hours off the day
// it is named after and leaks rows near midnight into the default partition.
func TestChatPartitionBoundsAreUTC(t *testing.T) {
	s, err := Open(storetest.TempSchemaDSN(t, storetest.PostgresDSN(t)))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	// 23:30 UTC today: inside today's UTC day, but outside it under any positive
	// offset. Inserted directly, because LogMessages takes the caller's timestamp
	// and the bot's is always now().
	var landedIn string
	if err := s.db.QueryRow(
		`WITH t AS (
		   SELECT (date_trunc('day', now() AT TIME ZONE 'UTC') + interval '23 hours 30 minutes')
		            AT TIME ZONE 'UTC' AS at
		 )
		 INSERT INTO chat_message (ts, ts_at, room_id, msg_id, user_id, login, display, text)
		 SELECT extract(epoch FROM t.at)::bigint, t.at, 'room1', 'edge', 'u1', 'bob', 'Bob', 'hi' FROM t
		 RETURNING tableoid::regclass::text`).Scan(&landedIn); err != nil {
		t.Fatal(err)
	}

	want := "chat_message_" + time.Now().UTC().Format("20060102")
	if landedIn != want {
		t.Errorf("a row at 23:30 UTC landed in %s, want %s — partition bounds are not UTC", landedIn, want)
	}
}

// 00006's down drops a partitioned parent with CASCADE, which takes the children
// with it. Worth exercising: the children are partitions rather than data of
// their own, and a DROP that left them behind would orphan every one of them
// into pg_dump.
//
// It rolls back to 5 rather than stepping down once. Down() undoes whichever
// migration happens to be newest, so a single step only reached the chat log for
// as long as the chat log was the last migration — which stopped being true the
// moment 00007 was added, and the failure read as "the chat log is not
// reversible" rather than "this test is aimed at the wrong migration". Naming the
// target keeps it pointed at 00006 however many migrations land on top.
func TestChatLogMigrationIsReversible(t *testing.T) {
	s, err := Open(storetest.TempSchemaDSN(t, storetest.PostgresDSN(t)))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	if err := s.LogMessages([]store.ChatMessage{
		{TS: time.Now().Unix(), RoomID: "room1", MsgID: "m1", UserID: "u1", Login: "bob", Display: "Bob", Text: "hi"},
	}); err != nil {
		t.Fatal(err)
	}

	p, err := provider(s.db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.DownTo(context.Background(), 5); err != nil {
		t.Fatalf("down to 5: %v", err)
	}

	for _, name := range []string{"chat_message", "chat_stats", "chat_folded"} {
		var n int
		if err := s.db.QueryRow(
			`SELECT COUNT(*) FROM information_schema.tables
			  WHERE table_schema = current_schema() AND table_name = $1`, name).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Errorf("%s survived the down migration", name)
		}
	}
	// No orphaned children left behind by the CASCADE.
	var orphans int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM pg_class c
		   JOIN pg_namespace n ON n.oid = c.relnamespace AND n.nspname = current_schema()
		  WHERE c.relname LIKE 'chat_message_%'`).Scan(&orphans); err != nil {
		t.Fatal(err)
	}
	if orphans != 0 {
		t.Errorf("%d chat_message_* tables survived the CASCADE", orphans)
	}

	if _, err := p.Up(context.Background()); err != nil {
		t.Fatalf("re-up: %v", err)
	}
	if err := s.LogMessages([]store.ChatMessage{
		{TS: time.Now().Unix(), RoomID: "room1", MsgID: "m2", UserID: "u1", Login: "bob", Display: "Bob", Text: "back"},
	}); err != nil {
		t.Fatalf("log after down-then-up: %v", err)
	}
}
