# ADR-0001: A Postgres backend beside SQLite, behind consumer-side capability interfaces

Date: 2026-08-10
Status: Accepted

## Context

All of the bot's persistent state lived in one local SQLite file (`bot.db`,
`modernc.org/sqlite`): 3 chat-managed commands, 114 users, 16,864 ledger rows,
three settings, two game tallies. The ledger is real currency — "marks" — and
includes escrowed `!g` buy-ins, which are debited on join and refunded from the
persisted round.

It worked. The reasons to want Postgres were deploying off this Mac,
multi-process access, managed durability, and real queries over the ledger.
**None of those arrive in phase 1**, which puts a local Homebrew Postgres on the
same machine — the same failure domain as a local file, plus a daemon and a
socket.

Two real defects also existed and had to be fixed on the way:

- `Leaderboard` used `HAVING bal > 0` against a `SELECT` alias and grouped only
  by `l.user_id` while selecting `u.login`/`u.display`. SQLite tolerates both;
  Postgres rejects both. It had always been a latent portability bug.
- `Grant`/`Spend`/`Transfer` did read-then-write inside `db.Begin()`, which opens
  a *deferred* transaction despite comments claiming otherwise, and leaned on
  SQLite's single-writer WAL for atomicity.

## Decision

**Two backends, permanently, behind capability interfaces declared at the
consumer.** `store` holds domain types only; `store/sqlite` and `store/postgres`
are peers. Six per-feature interfaces (`CommandStore`, `Ledger`, `SettingStore`,
`RoundStore`, `WordleWins`, `ConnectionsWins`) are declared in the `bot/` files
that use them, plus one composite `Store` as the field type. This matches the
existing house convention (`TTS`, `OverlayPusher`, `TwitchAPI`, `Chat`).

A single 21-method interface would have been less work and less honest: `Economy`
needs the ledger and nothing else, and its fakes should have to prove that.

**The ledger stays the single source of truth; `accounts` is a pure lock token.**
Under Postgres, two concurrent `Spend`s can both read the same balance and both
pass the check — a lost update on currency. `SUM(ledger)` cannot be locked (no
predicate lock stops a concurrent `INSERT`), so there is a row per user to
serialize on. It has `user_id`, `created_at`, `updated_at` and **no balance
column**: nothing derived means nothing that can drift. It is separate from
`users` because a chat user can reach `Spend` with no `users` row, and
`SELECT … FOR UPDATE` on a missing row takes no lock at all.

**No materialized balance.** At 114 users and ~150 rows each with `ledger_user`
indexed, the `SUM` is free. Revisit only on profiling.

**Migrations are `pressly/goose/v3` used as a library**, over embedded
per-dialect SQL, through the provider API — never the package-level
`SetDialect`/`Up` globals, because the migrate tool and the conformance suite
each drive both dialects in one process. Migration `00001` is the pre-migration
schema frozen verbatim, so applying it to the live `bot.db` is a no-op that
writes only the version row.

**Game rounds are promoted out of `settings`** into a `game_rounds` table, with
`room_id` and `ends_at` lifted into columns for legibility. The games still read
both from their own JSON, so the two cannot disagree in a way that breaks a game.

**The server keeps no database access.** The bot remains sole owner; the overlay
is fed by pushes, not queries.

**Fail fast on a missing database.** `store.Open` failure stays `logger.Fatalf`
and launchd's `KeepAlive` restarts until Postgres is listening.

**`COLLATE "C"` on every `ORDER BY` over a text name.** A Homebrew cluster
initdb'd `en_US.UTF-8` orders case- and punctuation-insensitively; SQLite
compares bytes.

**Dependencies go from 4 to 6**: `github.com/pressly/goose/v3` and
`github.com/jackc/pgx/v5`. Recorded here explicitly so the repo's near-stdlib
convention is consciously spent rather than quietly eroded.

## Consequences

**Good.** SQL portability, money safety under real concurrency, versioned
migrations and a verified bidirectional cutover are done and proven. Moving the
instance off-box is now a DSN change. `go test ./...` still runs hermetically
with no daemon, because SQLite is a first-class backend rather than a legacy one.
The conformance suite is a real gate: deliberately unsorting `Transfer`'s locks
hangs it, dropping `Credit`'s conflict predicate errors, removing `Spend`'s
account lock drives a balance to −40, and dropping `COLLATE "C"` flips the
leaderboard.

**Costs.** Two implementations of every store method, kept honest by the
conformance suite rather than by construction. Two direct dependencies. Anyone
adding a store feature has to add it twice and to both interface declarations —
which is the point: the compile error is the reminder.

**Not delivered by phase 1.** Off-box deployment, multi-process access, PITR.
Durability improves only to the extent a daily `pg_dump -Fc` with 14-day
retention improves it.

**Honored.** `../tts-server/issues/02`'s "native, not Docker" decision — Postgres
is a Homebrew service, not a container. And `../tts-server/issues/09`'s "all state
bot-owned, the server is a stateless relay" invariant.

## References

- PRD and issues: `.scratch/postgres-backend/`
- Operations: `docs/postgres.md`
- The contract: `store/storetest`
