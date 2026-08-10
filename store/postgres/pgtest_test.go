package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Postgres tests need a live server, which not every checkout has. They skip by
// default and turn fatal under TTS_REQUIRE_POSTGRES, so "skipped" can be made
// into an error where it matters (CI, the pre-cutover check) without making it
// one everywhere.

var schemaCounter atomic.Int64

// postgresDSN returns the conformance DSN, or skips with instructions.
func postgresDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		if os.Getenv("TTS_REQUIRE_POSTGRES") != "" {
			t.Fatal("TTS_REQUIRE_POSTGRES is set but TEST_DATABASE_URL is empty")
		}
		t.Skip("postgres tests SKIPPED: TEST_DATABASE_URL unset — run `mise run db:test:setup`")
	}
	return dsn
}

// tempSchemaDSN creates a throwaway schema and returns a DSN pinned to it, so
// migrations, the goose version table and every row land inside it and vanish on
// cleanup. Isolation is per-schema rather than per-database because CREATE
// DATABASE serializes on the template and costs 100ms+ each; a schema is cheap
// enough to do per test.
func tempSchemaDSN(t *testing.T) string {
	t.Helper()
	base := postgresDSN(t)

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
		t.Skipf("postgres tests SKIPPED: %s unreachable (%v) — run `mise run db:start`", base, err)
	}

	name := schemaName(t.Name(), schemaCounter.Add(1))
	if _, err := admin.Exec(`CREATE SCHEMA ` + name); err != nil {
		admin.Close()
		t.Fatalf("create schema %s: %v", name, err)
	}
	// Registered before the store's own Cleanup, so it runs after it: dropping a
	// schema blocks on any open connection still using it.
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
	// pins the connection's search_path without a libpq-style options= string.
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

// openTempSchema opens a store in its own throwaway schema.
func openTempSchema(t *testing.T) *Store {
	t.Helper()
	s, err := Open(tempSchemaDSN(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}
