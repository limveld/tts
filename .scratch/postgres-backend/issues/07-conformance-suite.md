# Shared conformance suite over both backends

Status: ready-for-agent
Type: task
Created: 2026-08-10

PRD: [`../PRD.md`](../PRD.md) · Depends on: 06 · Unblocks: 10

## Summary

One set of test bodies, run against SQLite and Postgres in the same `go test ./...`. This is the
issue that turns "two backends" from a claim into a fact — and it's where the concurrency guarantees
added in issue 06 actually get exercised.

## Decisions

- **A `store/storetest` package, not duplicated `_test.go` files.** The existing bodies in
  `store/sqlite/{store,points,wordle}_test.go` move there nearly verbatim and are deleted from their
  old homes.
- **`storetest.Store` duplicates the bot's capability interfaces on purpose.** The bot's are the
  *consumer's view of what it needs*; this one is *the contract a backend owes*. They drift only
  when a feature is added to one and not the other — which is exactly when you want a compile error.
- **Postgres isolation = one temp schema per test.** Not a temp database (`CREATE DATABASE`
  serializes on the template and costs 100ms+ each). Not transaction rollback — the implementations
  open their own transactions, savepoint nesting through `database/sql` is fragile, and it would
  hide the real `FOR UPDATE` behavior, which is the single most important thing being tested.
- **`bot/` tests stay on SQLite, permanently.** Backend equivalence is proven once, here. Running the
  bot's chat-logic tests twice buys nothing and doubles the time to red.
- **Loud has to land outside the test binary.** `t.Skip` messages are invisible without `-v` and a
  passing package's output is swallowed. Three levers instead: a `mise run test` preflight banner, a
  `TTS_REQUIRE_POSTGRES=1` mode that turns every skip into a fatal, and skip text that names the
  exact fix.

## Architecture

```
store/storetest/
  storetest.go   Store iface · New func · Run(t, New) · RunConcurrent(t, New)
  commands.go points.go settings.go rounds.go tallies.go   the bodies
  pg.go          PostgresDSN(t) · TempSchemaDSN(t, base)

store/sqlite/conformance_test.go     Run + t.TempDir() factory          — always runs
store/postgres/conformance_test.go   Run + RunConcurrent + temp schema  — skips without the DSN
```

## Work breakdown

1. **`store/storetest/storetest.go`** — the surface and the entry points:

   ```go
   // Package storetest is the conformance suite every store backend must pass.
   // One set of test bodies runs against SQLite and Postgres in the same
   // `go test ./...`, which is the only thing that makes "two backends" a claim
   // rather than a hope.
   package storetest

   type Store interface { /* the union, ~21 methods */ }

   // New builds one isolated store. Implementations must hand back a store that
   // shares nothing with any other call (its own file, its own schema); the suite
   // writes freely and never cleans up after itself.
   type New func(t *testing.T) Store

   func Run(t *testing.T, newStore New)
   func RunConcurrent(t *testing.T, newStore New)
   ```

2. **The cases** (each `t.Run`, each with a fresh store):
   `CommandCRUD` · `CommandListSorted` · `CreditSpendBalance` · `CreditIdempotentRef` ·
   `SpendInsufficient` · `UsersAndLeaderboard` · `LeaderboardExcludesZeroAndNegative` (pins issue
   01's `HAVING` fix) · `LeaderboardOrderAndTieBreak` (pins the collation fix) · `GrantMintAndClamp` ·
   `Transfer` · `TransferInsufficient` · `TransferSelfIsNoop` · `Settings` · `WordleTally` ·
   `ConnectionsTally` · `RoundSaveLoadClear` · `RoundOverwrite`.

3. **`RunConcurrent`** — Postgres only. SQLite serializes writers by construction; hammering it here
   would only test `busy_timeout`.
   - **Overdraw:** seed 100 marks, run 20 goroutines each spending 10. Exactly 10 succeed, final
     balance is 0, never negative.
   - **Deadlock:** two users, N goroutines transferring A→B and N transferring B→A concurrently. No
     error, no hang, conserved total. This is the case sorted lock acquisition exists for; without
     it, expect `deadlock detected`.
   - **Credit races:** concurrent `Credit` with the same `ref` credits exactly once.

4. **`store/storetest/pg.go`**

   ```go
   // PostgresDSN returns the conformance DSN, or skips with instructions.
   func PostgresDSN(t *testing.T) string {
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
   // migrations, the goose version table and every row land inside it and vanish
   // on cleanup. Cheap enough to do per test; parallel-safe.
   func TempSchemaDSN(t *testing.T, base string) string
   ```

   `TempSchemaDSN` pings with a short timeout first (skip, or fatal under `TTS_REQUIRE_POSTGRES`, if
   unreachable), names the schema from `t.Name()` plus a nanosecond counter, and drops it `CASCADE`
   in `t.Cleanup` — **after** the store's pool is closed, or the `DROP` blocks.

   Search path is set via a DSN query parameter (`?search_path=<schema>`); pgx v5 forwards
   unrecognized DSN keys as runtime parameters. **Confirm this early** — if it doesn't hold, the
   fallback is libpq-style `options=-c%20search_path%3D<schema>`.

5. **The two `conformance_test.go` files.** SQLite's is four lines around a `t.TempDir()` factory.
   Postgres's builds a temp schema per store and runs both `Run` and `RunConcurrent`.

6. **Delete the migrated bodies** from `store/sqlite/*_test.go`, keeping only what is genuinely
   SQLite-specific (the legacy-fixture migration tests from issues 04 and 05).

## Tests

This issue *is* tests. Its own acceptance:

- `go test ./...` green with `TEST_DATABASE_URL` **unset** — Postgres cases skip, nothing fails.
- `TTS_REQUIRE_POSTGRES=1 TEST_DATABASE_URL=… go test ./...` green against a live local Postgres.
- `TTS_REQUIRE_POSTGRES=1 go test ./...` **fails loudly** with no DSN set.
- `go test -race ./store/...` clean, including `RunConcurrent`.
- Deliberately break sorted lock ordering in `Transfer` and confirm the deadlock case catches it.
  Deliberately drop the `WHERE ref IS NOT NULL` from `Credit`'s conflict target and confirm
  `CreditIdempotentRef` catches it. A suite that can't fail isn't proving anything.

## Gotchas

- **`JSONB` normalizes key order and whitespace.** `RoundSaveLoadClear` must compare `State`
  semantically (`json.Unmarshal` into `map[string]any` + `reflect.DeepEqual`), never as bytes.
- **Schema-qualified goose.** The version table is created inside the temp schema because
  `search_path` points there — confirm, or the first test leaks a `goose_db_version` into `public`
  and the second test thinks it's already migrated.
- **`t.Parallel` and shared admin handles.** If cases run parallel, the admin `*sql.DB` used for
  `CREATE SCHEMA` must be safe to share (it is) and cleanup must not race the pool close (order it).

## Out of scope

- Running `bot/` tests against Postgres. If an end-to-end "Router on Postgres" smoke test is ever
  wanted, it belongs as a single skipped-by-default test in `bot/`, not a second full run.
- Benchmarks, fuzzing, property tests.
- CI. There is none in this repo, and adding it is not this epic's job.

## References

- Bodies to migrate: `store/sqlite/store_test.go` (`openTemp`), `store/sqlite/points_test.go`,
  `store/sqlite/wordle_test.go`
- The lock path under test: `store/postgres/points.go` (`lockAccount`, `Spend`, `Transfer`)
- Test-helper precedent: `server/overlay_test.go` (`newTestOverlayServer`, `readSSEEvent`)
