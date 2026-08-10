package storetest

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// Postgres conformance needs a live server, which not every checkout has. It
// skips by default and turns fatal under TTS_REQUIRE_POSTGRES, so "skipped" can
// be made into an error where it matters — before a cutover, say — without
// making it one everywhere.
//
// t.Skip messages are invisible without -v and a passing package's output is
// swallowed, so the skip is only half the story: mise's `test` task prints a
// preflight banner, which is where a human actually learns Postgres cases
// didn't run.

var schemaCounter atomic.Int64

// PostgresDSN returns the conformance DSN, or skips with instructions.
func PostgresDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		if os.Getenv("TTS_REQUIRE_POSTGRES") != "" {
			t.Fatal("TTS_REQUIRE_POSTGRES is set but TEST_DATABASE_URL is empty")
		}
		t.Skip("postgres conformance SKIPPED: TEST_DATABASE_URL unset — run `mise run db:test:setup`")
	}
	return dsn
}

// TempSchemaDSN creates a throwaway schema and returns a DSN pinned to it, so
// migrations, the goose version table and every row land inside it and vanish on
// cleanup.
//
// Isolation is per-schema rather than per-database because CREATE DATABASE
// serializes on the template and costs 100ms+ each; a schema is cheap enough to
// do per test. It is not transaction-rollback either: the implementations open
// their own transactions, and rolling back around them would hide the FOR UPDATE
// behavior that is the single most important thing being tested.
func TempSchemaDSN(t *testing.T, base string) string {
	t.Helper()

	admin, err := sql.Open("pgx", base)
	if err != nil {
		t.Fatalf("open admin connection: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := admin.PingContext(ctx); err != nil {
		admin.Close()
		if os.Getenv("TTS_REQUIRE_POSTGRES") != "" {
			t.Fatalf("TTS_REQUIRE_POSTGRES is set but %s is unreachable: %v", base, err)
		}
		t.Skipf("postgres conformance SKIPPED: %s unreachable (%v) — run `mise run db:start`", base, err)
	}

	name := schemaName(t.Name(), schemaCounter.Add(1))
	if _, err := admin.Exec(`CREATE SCHEMA ` + name); err != nil {
		admin.Close()
		t.Fatalf("create schema %s: %v", name, err)
	}
	// Registered here, so it runs *after* the caller's own store-closing
	// cleanup: DROP SCHEMA blocks on any connection still using it.
	t.Cleanup(func() {
		defer admin.Close()
		if _, err := admin.Exec(`DROP SCHEMA IF EXISTS ` + name + ` CASCADE`); err != nil {
			t.Errorf("drop schema %s: %v", name, err)
		}
	})

	sep := "?"
	if strings.Contains(base, "?") {
		sep = "&"
	}
	// pgx forwards unrecognized DSN keys as startup runtime parameters, so this
	// pins search_path without a libpq-style options= string.
	return base + sep + "search_path=" + name
}

// schemaName turns a test name into a legal, unique, lowercase identifier.
func schemaName(testName string, n int64) string {
	var b strings.Builder
	b.WriteString("t_")
	for _, r := range strings.ToLower(testName) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return fmt.Sprintf("%.40s_%d", b.String(), n)
}
