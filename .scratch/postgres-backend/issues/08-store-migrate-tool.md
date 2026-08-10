# cmd/store-migrate — bidirectional copy with balance verification

Status: ready-for-agent
Type: task
Created: 2026-08-10

PRD: [`../PRD.md`](../PRD.md) · Depends on: 06 · Unblocks: 10

## Summary

A one-shot tool that copies every table between the two backends and then proves the copy is correct
by comparing balances **through the public API**. Bidirectional, so cutting over is reversible after
the fact rather than only before it.

## Decisions

- **Bidirectional.** Postgres → SQLite costs almost nothing extra once the copier is a table list
  plus a dialect-aware placeholder builder, and it converts "rollback" from *revert to the archived
  `bot.db` and lose everything since* into *copy back and lose nothing*. That's the difference
  between a rollback plan and a rollback hope.
- **Verification runs through the `Ledger` API, not raw SQL.** Comparing `SELECT SUM(delta)` on both
  sides only proves the rows moved. Comparing `src.Balance(id)` to `dst.Balance(id)` proves the
  *application-level invariant* holds on both backends — which is the thing actually at risk, since
  the two run different SQL to compute it.
- **Both stores are opened through their normal `Open`,** so both run their migrations first. That's
  what lets the copier be a dumb table-for-table loop with zero per-table special cases: by the time
  it runs, the source already has `game_rounds` populated by migration `00002`.
- **Refuses a non-empty destination** unless `-force`. The cutover is run by a human at 2am
  eventually; make the footgun require a flag.
- **`DB()` accessors, documented as tool-only.** Both implementations expose their `*sql.DB` for
  table-level access the capability interfaces deliberately don't offer. Comment: nothing in `bot/`
  or `server/` may use this.

## Interface

```
Usage:
  store-migrate -from <dsn> -to <dsn> [flags]

  -from           source DSN (sqlite path or postgres:// URL)
  -to             destination DSN
  -migrate-only   run migrations on -to and exit (no copy)
  -verify-only    skip the copy, just compare -from and -to
  -force          copy even though the destination already has rows

Examples:
  store-migrate -from bot.db -to "$TTS_DATABASE_URL"            # cutover
  store-migrate -from "$TTS_DATABASE_URL" -to rollback.db       # come back
  store-migrate -from bot.db -to "$TTS_DATABASE_URL" -verify-only
```

## Work breakdown

1. **`DB() *sql.DB`** on `store/sqlite.Store` and `store/postgres.Store`.

2. **Open both** via the same scheme dispatch `bot/store.go` uses. Extract that switch into a small
   shared helper rather than copy-pasting it — one place decides what a DSN means.

3. **Refuse a dirty destination**: count `commands`, `users`, `ledger`; abort unless all zero or
   `-force`.

4. **Copy inside one destination transaction**, batched ~500 rows per multi-`VALUES` insert, in this
   order: `commands`, `users`, `accounts`, `ledger` (**explicit `id`**), `settings`, `wordle_wins`,
   `connections_wins`, `game_rounds` (`state` cast `::jsonb` when the destination is Postgres).

5. **Reset the identity sequence** when the destination is Postgres:

   ```sql
   SELECT setval(pg_get_serial_sequence('ledger','id'),
                 COALESCE((SELECT MAX(id) FROM ledger), 0) + 1, false)
   ```

   **Miss this and the first live `Credit` after cutover dies on a duplicate key.** It is the single
   easiest thing to forget and the most embarrassing to discover in production. The verifier checks
   it too.

6. **Verify**, and exit non-zero on any mismatch with the offending ids printed:
   - union of `user_id` from `users` and `ledger`; `src.Balance(id)` vs `dst.Balance(id)` for each
   - per-table row counts
   - global `SUM(delta)` and `MAX(ledger.id)`
   - `src.Leaderboard(50)` vs `dst.Leaderboard(50)`, element-wise — **this is what catches the
     collation trap before production does**
   - `game_rounds`: same game keys, `State` compared semantically (JSONB reorders keys)
   - destination sequence is past `MAX(id)`

7. **Output** — one screenful, countable:

   ```
   store-migrate: sqlite bot.db (schema 2) -> postgres tts (schema 2)
     commands              3
     users               114
     accounts              0   (created on demand)
     ledger           16,864   max id 16864, sequence -> 16865
     settings              3
     wordle_wins           5
     connections_wins      1
     game_rounds           0
   verify: 114/114 balances match  ·  sum(delta) 1,234,567 == 1,234,567
           counts match  ·  leaderboard top-50 identical
   OK — cut over:  TTS_DATABASE_URL='postgres://…' mise run bot:service:restart
        rollback:  unset TTS_DATABASE_URL; bot.db is untouched
   ```

## Tests

- **Round-trip, `cmd/store-migrate/main_test.go`** (Postgres cases skipped without the DSN): seed a
  temp SQLite store through the public API with users, ledger entries including one with a `ref`,
  commands, settings, both tallies and a round → copy into a temp schema → `-verify-only` passes.
- **The verifier must be able to fail.** After a clean copy, mutate one destination balance with raw
  SQL and assert `-verify-only` exits non-zero naming that user. Delete a command row and assert the
  count check catches it.
- **Reverse direction**: Postgres → a fresh temp SQLite file, verify.
- **Sequence**: after a copy, a live `Credit` on the destination succeeds and gets `MAX(id)+1`.
- **Dirty destination**: a second copy without `-force` refuses; with `-force` it proceeds.
- `go test ./...` green without Postgres.

## Out of scope

- Incremental or resumable sync. This is one-shot, taken with the bot stopped.
- Dual-write / live replication.
- Migrating `bot.tokens.json`, `connections.json`, or anything outside the database.
- A progress bar. 17k rows is instant.

## References

- Scheme dispatch to share: `bot/store.go` (`openStore`)
- Balance semantics being verified: `store/sqlite/points.go`, `store/postgres/points.go` (`Balance`, `Leaderboard`)
- The `AUTOINCREMENT` → `GENERATED BY DEFAULT AS IDENTITY` choice that makes explicit ids possible:
  issue [06](06-postgres-backend.md)
- Runbook that drives this tool: issue [10](10-production-cutover.md)
