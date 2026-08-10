# goose as a library + the SQLite baseline migration

Status: ready-for-agent
Type: task
Created: 2026-08-10

PRD: [`../PRD.md`](../PRD.md) · Depends on: 02 · Unblocks: 05

## Summary

Replace the inline `CREATE TABLE IF NOT EXISTS` loop in `sqlite.Open` with real versioned migrations,
run by `pressly/goose/v3` used as a library over embedded SQL. The interesting part is that `bot.db`
is live, has 16,864 ledger rows, and has no version history — so the first migration has to be
adoptable by an existing database without any stamping tricks.

## Decisions

- **The baseline migration is the current DDL, byte for byte.** Every statement in `sqlite.Open`'s
  schema slice is already `IF NOT EXISTS`. Applying that file to the live `bot.db` is a genuine no-op
  that only writes the version row; applying it to a fresh file produces exactly today's schema. No
  `goose fix`, no manual stamping, no "if tables exist then pretend" branch. That property is the
  whole reason to freeze migration `00001` rather than write a tidier one.
- **goose as a library, via the provider API.** `goose.NewProvider(dialect, db, fsys)` — never the
  package-level `goose.SetDialect` / `goose.Up` globals. `cmd/store-migrate` (issue 08) and the
  conformance suite (issue 07) both drive two dialects in one process, and process-global dialect
  state is a footgun there.
- **Default version table name (`goose_db_version`).** Renaming it to `schema_migrations` requires a
  custom `database.Store` implementation and buys nothing.
- **Migrations run inside `Open`.** A fresh file or database comes up ready; an existing one is
  brought forward. Combined with the fail-fast decision, this means a bad migration is a crash loop —
  which is exactly why the legacy-fixture test below is not optional.
- **Dependency accepted.** `github.com/pressly/goose/v3` is direct dependency #5. Recorded in the ADR
  in issue 09 so the repo's near-stdlib convention isn't eroded silently.

## Work breakdown

1. **`store/sqlite/migrations/00001_baseline.sql`** — the eight statements currently in
   `store/sqlite/store.go`'s schema slice, verbatim, under `-- +goose Up`, with a header comment
   explaining the no-op-on-live-db property. A `-- +goose Down` that drops them in reverse order.

   Includes the partial unique index exactly as-is:
   ```sql
   CREATE UNIQUE INDEX IF NOT EXISTS ledger_ref ON ledger(ref) WHERE ref IS NOT NULL;
   ```

2. **`store/sqlite/store.go`** — `//go:embed migrations/*.sql`, and replace the schema loop with:

   ```go
   func migrate(db *sql.DB) error {
       sub, err := fs.Sub(migrationsFS, "migrations")
       if err != nil { return err }
       p, err := goose.NewProvider(goose.DialectSQLite3, db, sub)
       if err != nil { return fmt.Errorf("migrations: %w", err) }
       if _, err := p.Up(context.Background()); err != nil {
           return fmt.Errorf("migrating: %w", err)
       }
       return nil
   }
   ```

   Keep the pragma loop (and issue 01's `_txlock=immediate`) ahead of it.

3. **Silence goose's logger** or route it through the caller's `*log.Logger` — by default it writes
   to stdout, which under launchd lands in `~/Library/Logs/tts-bot.out.log` unlabelled. Prefer
   `goose.WithLogger` with a small adapter, or a no-op logger plus one summary line from `Open`.

4. **`go mod tidy`.**

## Tests

- **`store/sqlite/migrate_test.go` — the legacy fixture, the test that matters.** In a `t.TempDir()`,
  open a raw `database/sql` handle and execute the *pre-goose* DDL plus a few rows (a command, a
  user, three ledger entries, a settings key). Close it. Then `sqlite.Open` the same path and assert:
  - no error,
  - all seeded rows are still present and readable through the public API,
  - `goose_db_version` reports version 1,
  - a second `Open` is idempotent.
- Fresh-database test: `Open` on an empty temp path produces a schema that passes the existing
  `store/sqlite` suite unchanged.
- `go test ./...` green.

## Out of scope

- New tables — `accounts` and `game_rounds` are migration `00002`, in issue 05.
- The Postgres migration set (issue 06). It mirrors SQLite's *final* shape, which is why it waits
  until 05 has landed.
- A `goose` CLI, a `migrations` mise task for hand-running, or down-migrations in production. `Down`
  exists for tests and local resets only.

## References

- `store/sqlite/store.go` (`Open`, the schema slice at lines 43–97 pre-split)
- goose provider API: `goose.NewProvider`, `goose.DialectSQLite3`, `goose.DialectPostgres`
- Fail-fast decision this interacts with: `bot/main.go:31` (`logger.Fatalf`)
