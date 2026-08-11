package sqlite

import (
	"database/sql"
	"path/filepath"
	"testing"

	"tts/store"
)

// legacySchema is the DDL as it stood before there were migrations — the exact
// statements the live bot.db was built from. Every test that seeds a "pre-goose"
// database uses this, because the property under test is that migration 00001 is
// a no-op against it.
var legacySchema = []string{
	`CREATE TABLE IF NOT EXISTS commands (
		name     TEXT PRIMARY KEY,
		response TEXT NOT NULL,
		cooldown INTEGER NOT NULL DEFAULT 0,
		min_role TEXT NOT NULL DEFAULT 'everyone',
		count    INTEGER NOT NULL DEFAULT 0
	)`,
	`CREATE TABLE IF NOT EXISTS users (
		user_id   TEXT PRIMARY KEY,
		login     TEXT NOT NULL,
		display   TEXT NOT NULL,
		last_seen INTEGER NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS ledger (
		id      INTEGER PRIMARY KEY AUTOINCREMENT,
		user_id TEXT NOT NULL,
		delta   INTEGER NOT NULL,
		reason  TEXT NOT NULL,
		ref     TEXT,
		ts      INTEGER NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS ledger_user ON ledger(user_id)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS ledger_ref ON ledger(ref) WHERE ref IS NOT NULL`,
	`CREATE TABLE IF NOT EXISTS settings (
		key   TEXT PRIMARY KEY,
		value TEXT NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS wordle_wins (
		user_id TEXT PRIMARY KEY,
		login   TEXT NOT NULL,
		display TEXT NOT NULL,
		wins    INTEGER NOT NULL DEFAULT 0
	)`,
	`CREATE TABLE IF NOT EXISTS connections_wins (
		user_id TEXT PRIMARY KEY,
		login   TEXT NOT NULL,
		display TEXT NOT NULL,
		wins    INTEGER NOT NULL DEFAULT 0
	)`,
}

// seedLegacyDB builds a database at path with the pre-migration schema and the
// given rows, using a raw handle so none of Open's migration machinery runs.
func seedLegacyDB(t *testing.T, path string, rows ...string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("raw open: %v", err)
	}
	defer db.Close()
	for _, stmt := range append(append([]string{}, legacySchema...), rows...) {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("seed %q: %v", stmt, err)
		}
	}
}

// wantSchemaVersion is the highest migration in migrations/. Bump it when one is
// added — a test that tracked the max automatically would pass even if the
// migration never ran.
const wantSchemaVersion = 3

func schemaVersion(t *testing.T, s *Store) int64 {
	t.Helper()
	var v int64
	if err := s.db.QueryRow(
		`SELECT MAX(version_id) FROM goose_db_version WHERE is_applied = 1`).Scan(&v); err != nil {
		t.Fatalf("read goose_db_version: %v", err)
	}
	return v
}

// The load-bearing test of issue 04: the live bot.db has this schema and no
// version history, so adopting it must be a no-op that writes only the version
// row. If this breaks, the bot crash-loops on its real data.
func TestMigrateAdoptsLegacyDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.db")
	seedLegacyDB(t, path,
		`INSERT INTO commands (name, response, cooldown, min_role, count)
		 VALUES ('discord', 'join here', 5, 'everyone', 42)`,
		`INSERT INTO users (user_id, login, display, last_seen)
		 VALUES ('u1', 'bob', 'Bob', 1700000000)`,
		`INSERT INTO ledger (user_id, delta, reason, ref, ts) VALUES ('u1', 100, 'accrual', NULL, 1)`,
		`INSERT INTO ledger (user_id, delta, reason, ref, ts) VALUES ('u1', 50, 'convert', 'red-1', 2)`,
		`INSERT INTO ledger (user_id, delta, reason, ref, ts) VALUES ('u1', -30, 'tts', NULL, 3)`,
		`INSERT INTO settings (key, value) VALUES ('charge_mode', 'paid')`,
	)

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open on a legacy database: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	// Everything seeded is still there, read back through the public API.
	if c, ok, err := s.Get("discord"); err != nil || !ok || c.Response != "join here" || c.Count != 42 {
		t.Errorf("command after migrate: %+v ok=%v err=%v", c, ok, err)
	}
	if id, ok, err := s.ResolveLogin("bob"); err != nil || !ok || id != "u1" {
		t.Errorf("user after migrate: id=%q ok=%v err=%v", id, ok, err)
	}
	if b, err := s.Balance("u1"); err != nil || b != 120 {
		t.Errorf("balance after migrate=%d err=%v want 120", b, err)
	}
	if v, ok, err := s.GetSetting("charge_mode"); err != nil || !ok || v != "paid" {
		t.Errorf("setting after migrate: %q ok=%v err=%v", v, ok, err)
	}
	// The partial unique index survived, so ref idempotency still holds.
	if credited, err := s.Credit("u1", 50, "convert", "red-1"); err != nil || credited {
		t.Errorf("duplicate ref after migrate: credited=%v err=%v want false/nil", credited, err)
	}

	if v := schemaVersion(t, s); v != wantSchemaVersion {
		t.Errorf("goose version=%d want %d", v, wantSchemaVersion)
	}

	// Reopening is idempotent: no pending migrations, no error, same data.
	s.Close()
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	t.Cleanup(func() { s2.Close() })
	if b, _ := s2.Balance("u1"); b != 120 {
		t.Errorf("balance after reopen=%d want 120", b)
	}
	if v := schemaVersion(t, s2); v != wantSchemaVersion {
		t.Errorf("goose version after reopen=%d want %d", v, wantSchemaVersion)
	}
}

func TestMigrateFreshDatabase(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "fresh.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	if v := schemaVersion(t, s); v != wantSchemaVersion {
		t.Errorf("goose version=%d want %d", v, wantSchemaVersion)
	}
	// A smoke pass over each table the baseline creates.
	if _, err := s.Add(store.Command{Name: "x", Response: "y"}); err != nil {
		t.Errorf("commands: %v", err)
	}
	if err := s.UpsertUser("u1", "bob", "Bob"); err != nil {
		t.Errorf("users: %v", err)
	}
	if _, err := s.Credit("u1", 10, "accrual", ""); err != nil {
		t.Errorf("ledger: %v", err)
	}
	if err := s.SetSetting("charge_mode", "free"); err != nil {
		t.Errorf("settings: %v", err)
	}
	if _, err := s.WordleAddWin("u1", "bob", "Bob"); err != nil {
		t.Errorf("wordle_wins: %v", err)
	}
	if _, err := s.ConnectionsAddWin("u1", "bob", "Bob"); err != nil {
		t.Errorf("connections_wins: %v", err)
	}
}

// Migration 00002 must carry an in-flight round out of settings and into
// game_rounds without losing it. This is the case that would strand escrowed !g
// buy-ins if it were wrong, so the fixture uses a real gamble document rather
// than a synthetic one.
func TestMigrateCarriesRoundsOutOfSettings(t *testing.T) {
	const gambleDoc = `{"id":"1723312800000000000","roomID":"12345","buyIn":100,` +
		`"endsAt":1723312860000,"entrants":[{"userID":"u1","login":"bob","display":"Bob"},` +
		`{"userID":"u2","login":"amy","display":"Amy"}],"winner":-1}`

	path := filepath.Join(t.TempDir(), "rounds.db")
	seedLegacyDB(t, path,
		`INSERT INTO settings (key, value) VALUES ('gamble_round', '`+gambleDoc+`')`,
		// A finished round: the old clear path wrote "" rather than deleting, so
		// this must NOT be resurrected as an in-flight round.
		`INSERT INTO settings (key, value) VALUES ('wordle_round', '')`,
		`INSERT INTO settings (key, value) VALUES ('charge_mode', 'paid')`,
	)

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	round, ok, err := s.LoadRound("gamble")
	if err != nil || !ok {
		t.Fatalf("LoadRound(gamble): ok=%v err=%v — the round was lost", ok, err)
	}
	// The columns were extracted from the document by json_extract.
	if round.RoomID != "12345" {
		t.Errorf("room_id=%q want 12345", round.RoomID)
	}
	if round.EndsAt != 1723312860000 {
		t.Errorf("ends_at=%d want 1723312860000", round.EndsAt)
	}
	// And the document itself came across byte-identical, so the entrants (and
	// therefore the escrow) are intact.
	if string(round.State) != gambleDoc {
		t.Errorf("state=%s\nwant %s", round.State, gambleDoc)
	}

	// The empty wordle round was skipped, not migrated as an in-flight round.
	if _, ok, err := s.LoadRound("wordle"); err != nil || ok {
		t.Errorf("LoadRound(wordle): ok=%v err=%v want false/nil (it was finished)", ok, err)
	}

	// The three *_round keys are gone from settings; real settings are untouched.
	for _, k := range []string{"gamble_round", "wordle_round", "connections_round"} {
		if _, ok, _ := s.GetSetting(k); ok {
			t.Errorf("settings.%s still present after migrate", k)
		}
	}
	if v, ok, _ := s.GetSetting("charge_mode"); !ok || v != "paid" {
		t.Errorf("charge_mode=%q ok=%v want paid/true", v, ok)
	}
}
