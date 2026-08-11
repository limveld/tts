package postgres

import (
	"testing"

	"tts/store/storetest"
)

// wantSchemaVersion is the highest migration in migrations/. It must stay in step
// with store/sqlite's — the two dialects are meant to describe the same schema,
// and the migrate tool prints both.
const wantSchemaVersion = 4

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
		`INSERT INTO ledger (id, user_id, delta, reason, ts) VALUES (9000, 'u1', 5, 'copy', 1)`); err != nil {
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
