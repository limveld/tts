# PRD: Postgres backend for the bot

Status: in progress — issues 01–09 shipped (2026-08-10); [10](issues/10-production-cutover.md) is
ready-for-human (the live cutover needs an offline stream and chat smoke tests)
Type: prd
Created: 2026-08-10

Tracked as [`issues/01`](issues/01-store-portability-and-money-fixes.md) … [`issues/10`](issues/10-production-cutover.md).

## Summary

Give the bot a Postgres backend alongside the existing SQLite one, behind capability interfaces
defined at the consumer. Cut the live data over to a **local Homebrew Postgres on the same Mac**.
SQLite stays a permanently supported dev/test backend; one shared conformance suite proves the two
behave identically.

## Motivation

The store is a local SQLite file (`bot.db`, `modernc.org/sqlite`) holding real money data — 114
users, 16,864 ledger rows, live mark balances, escrowed `!g` buy-ins. It works. The reasons to want
Postgres are: deploying off the Mac, multi-process/remote access, durability and managed backups,
and real queries over the ledger.

**None of those arrive in this epic**, and saying so up front is the point. A local Postgres on the
same box is the same failure domain as a local file, plus a daemon and a socket. What phase 1 buys
is that the *hard* part is done and proven — SQL portability, money safety under real concurrency,
versioned migrations, and a data cutover with verified balances. Once that lands, moving the
instance off-box is a DSN change instead of a project.

Two real defects are already in `store/` and get fixed on the way:

- `Leaderboard` uses `HAVING bal > 0` against a `SELECT` alias (`store/points.go:178`) — **invalid
  Postgres SQL**. It has always been a latent portability bug.
- `Grant`/`Spend`/`Transfer` do read-then-write inside `db.Begin()` and lean on SQLite's
  single-writer WAL for atomicity. Under Postgres READ COMMITTED those are **lost-update races on
  currency** — two concurrent spends can both pass the balance check.

## Decisions (from grilling)

- **Phase 1 only.** Local Homebrew Postgres. Moving the instance (or the bot) off-box is a separate
  epic and explicitly out of scope here.
- **Both backends, permanently.** SQLite is not retired after a soak. It stays the fast, hermetic
  default for `go test ./...` and for running the bot without a daemon.
- **Seam = capability interfaces at the consumer.** Six per-feature interfaces declared in `bot/`
  where they're used, plus one composite so `Router.store` stays a single field. Matches the
  existing `Player` / `Synthesizer` / `TTS` / `OverlayPusher` / `TwitchAPI` convention.
- **Money safety = row lock on a dedicated `accounts` table.** `accounts` is a **pure lock token**
  (`user_id`, `created_at`, `updated_at`) — no balance column. The ledger stays the single source of
  truth, so there is no derived value that can drift. `SUM(ledger)` can't be locked (no predicate
  lock stops a concurrent `INSERT`), so the row exists solely to serialize check-then-debit per user.
  A separate table rather than reusing `users`, because a spender may have no `users` row and
  `SELECT … FOR UPDATE` on a missing row takes no lock at all.
- **Game rounds promoted out of `settings`.** `gamble_round` / `wordle_round` / `connections_round`
  become rows in a `game_rounds` table with `room_id` and `ends_at` lifted into real columns.
- **Migrations = `pressly/goose/v3` as a library**, embedded per-dialect SQL, provider API (never
  the package-level `SetDialect` globals — the migrate tool drives two dialects in one process).
- **Tests = one shared conformance suite** run against both backends in the same `go test ./...`.
  Postgres isolation is a temp schema per test.
- **The server gets no database access.** The bot remains sole owner; issue 09's "all state
  bot-owned, the server is a stateless relay" invariant is preserved.
- **Fail fast on a missing database.** `store.Open` failure stays `logger.Fatalf`; launchd
  `KeepAlive` restarts the bot until Postgres is listening. No retry loop, no graceful degradation
  — that would turn every `if r.store == nil` guard into a live code path.
- **Cutover via a bidirectional `cmd/store-migrate`** that verifies per-user balances through the
  public API. `bot.db` archived as the rollback; the reverse direction recovers post-cutover writes.

## Architecture

```
                       bot/  (sole DB owner)
  Router.store ─┐
  Economy.store ┴─> capability interfaces declared at the consumer
                     CommandStore · Ledger · SettingStore
                     RoundStore   · WordleWins · ConnectionsWins
                                  │  composite: bot.Store
                                  │  openStore(dsn) switches on scheme
                    ┌─────────────┴─────────────┐
            store/sqlite                  store/postgres
            modernc.org/sqlite            pgx/v5
            goose + migrations/*.sql      goose + migrations/*.sql
            single writer, no locks       accounts row FOR UPDATE
                    │                             │
                bot.db (file)          postgres://localhost/tts (Homebrew)
                    └──── cmd/store-migrate ──────┘   copy + verify, both directions

            store/          domain types only (Command, LedgerEntry, Round, …)
            store/storetest one suite, run against both backends
```

## Schema

Seven tables. Existing six carry over unchanged in shape; `accounts` and `game_rounds` are new.

| Table | Purpose | Change |
|---|---|---|
| `commands` | chat-managed custom commands | none |
| `users` | identity: `user_id` → current login/display | none |
| `ledger` | append-only marks; balance = `SUM(delta)` | `AUTOINCREMENT` → `BIGINT GENERATED BY DEFAULT AS IDENTITY` on PG |
| `settings` | KV: `charge_mode`, `depth_points`, `depth_pb` | loses the three `*_round` keys |
| `wordle_wins` / `connections_wins` | win tallies | none |
| **`accounts`** | per-user lock token for money moves | **new** |
| **`game_rounds`** | one row per in-flight round | **new** |

Timestamps stay `BIGINT` unix seconds (`game_rounds.ends_at` is unix millis, matching the existing
JSON) rather than becoming `timestamptz` — one code path for both backends and a straight value copy.
The partial unique index `ledger_ref … WHERE ref IS NOT NULL` ports identically to Postgres.

## Work breakdown

Ten issues, each independently shippable and leaving `go build/vet/test ./...` green.

| # | Issue | Depends on |
|---|---|---|
| [01](issues/01-store-portability-and-money-fixes.md) | Portability + money fixes on today's store | — |
| [02](issues/02-package-split.md) | Package split: `store` → types-only, impl to `store/sqlite` | — |
| [03](issues/03-capability-interfaces.md) | Capability interfaces + `openStore` factory | 02 |
| [04](issues/04-goose-migrations.md) | goose as a library + SQLite baseline migration | 02 |
| [05](issues/05-accounts-and-game-rounds.md) | `accounts` + `game_rounds` (SQLite) | 04 |
| [06](issues/06-postgres-backend.md) | The Postgres backend | 03, 05 |
| [07](issues/07-conformance-suite.md) | Shared conformance suite, both backends | 06 |
| [08](issues/08-store-migrate-tool.md) | `cmd/store-migrate`, bidirectional + verifying | 06 |
| [09](issues/09-config-deploy-backups-docs.md) | Config, mise tasks, launchd, backups, docs, ADR | 06 |
| [10](issues/10-production-cutover.md) | Execute the production cutover | 07, 08, 09 |

## Tests

- **Conformance (issue 07):** one set of bodies over both backends — command CRUD, credit/spend/
  balance, idempotent `ref`, leaderboard exclusion of zero/negative balances (pins the `HAVING` fix),
  grant mint + clamp, transfer, settings, both tallies, round save/load/clear/overwrite.
- **Concurrency (Postgres only):** N goroutines racing one balance; bidirectional transfers (the
  deadlock case). SQLite serializes writers by construction — running it there only tests
  `busy_timeout`.
- **Legacy fixtures (issues 04, 05):** build a pre-goose `bot.db` with the old DDL and rows, assert
  the baseline migration is a no-op with data intact, and that a `settings.gamble_round` row lands in
  `game_rounds` and still restores.
- **Round-trip (issue 08):** seed a temp SQLite store → copy into a temp schema → `-verify-only`
  passes; mutate one row → it fails.
- **Manual/live (issue 10):** balances verified across all 114 users, bot restarts clean on the DSN,
  a `!g` round and a `!wordle` survive a restart.

## Out of scope

- Moving Postgres (or the bot) off the Mac. Follow-up epic.
- Any database access from the `server` process — no read-only pool, no stats endpoint, no
  overlay hydration from the DB. The in-memory `lastState` cache stays as-is.
- Dashboards, analytics views, or reporting queries over the ledger.
- A materialized balance column. At 114 users / 16.8k rows the `SUM` is free; revisit only if
  profiling says otherwise.
- Retiring the SQLite backend.
- Converting timestamps to `timestamptz`, or normalizing round state into per-game tables.
- Connection pooling tuning, read replicas, PITR/WAL archiving. Phase 1 backup is a daily `pg_dump`.

## References

- Current store: `store/store.go` (`Open`, schema, commands CRUD), `store/points.go`
  (`Balance`/`Credit`/`Grant`/`Spend`/`Transfer`/`Leaderboard`), `store/settings.go`,
  `store/wordle.go`, `store/connections.go`.
- Consumers: `bot/router.go` (`Router.store`), `bot/economy.go` (`Economy.store`), `bot/main.go`
  (`store.Open`, `seedCommands`), `bot/admin.go`, `bot/games.go`, `bot/depth.go`.
- Round persistence to be rewired: `bot/gamble.go` (`persistGamble`/`loadGamble`), `bot/wordle.go`
  (`persistWordle`/`loadWordle`), `bot/connections.go` (`persistConnections`/`restoreConnections`).
- Interface-at-the-consumer precedent: `bot/tts.go` (`TTS`), `bot/overlay.go` (`OverlayPusher`),
  `bot/economy.go` (`TwitchAPI`), `server/synth.go` (`Synthesizer`).
- Test helper being generalized: `store/store_test.go` (`openTemp`).
- Deployment: `deploy/service.sh`, `deploy/com.rtukpe.tts-bot.plist.template`, `mise.toml`.
- The "native, not Docker" decision this epic honors: `../tts-server/issues/02-run-as-a-service-mise-tasks-launchd.md`.
- The "all state bot-owned" invariant this epic preserves: `../tts-server/issues/09-overlay-upgrade.md`.
