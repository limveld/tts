# Postgres

The bot runs on either of two storage backends, chosen by the `-db` flag (or
`TTS_DATABASE_URL`):

| DSN | Backend |
|---|---|
| `bot.db`, or any bare path | SQLite — the default, no daemon |
| `sqlite:///abs/path.db` | SQLite, spelled as a URL |
| `postgres://…` / `postgresql://…` | Postgres |

Both are permanently supported. SQLite stays the fast, hermetic default for
`go test ./...` and for running the bot without a daemon; Postgres is what the
live channel runs on. One shared conformance suite (`store/storetest`) proves the
two behave identically, and it runs against both in the same `go test ./...`.

Phase 1 is a **local** Postgres on this Mac. That is deliberately not a
deployment win — a local daemon is the same failure domain as a local file, plus
a socket. What it buys is that the hard parts (SQL portability, money safety
under real concurrency, versioned migrations, a verified data cutover) are done,
so moving the instance off-box later is a DSN change rather than a project.

## Install

```sh
mise run db:install    # brew install postgresql@18 && brew services start postgresql@18
mise run db:create     # createdb tts
mise run db:status     # service state + pg_isready
```

Pinned to **`postgresql@18`**. A major-version upgrade (`@18` → `@19`) needs
`pg_upgrade` and takes the bot down for the duration — it is a scheduled task,
never a `brew upgrade` side effect. Check `brew services list` before upgrading
anything Postgres-shaped.

## Dev and test

The Postgres half of the conformance suite is skipped unless a test DSN is set:

```sh
mise run db:test:setup    # createdb tts_test, prints the line to add
```

Then put it in `mise.local.toml` (gitignored):

```toml
[env]
TEST_DATABASE_URL = "postgres:///tts_test"
```

| Command | Behavior |
|---|---|
| `mise run test` | whole suite; prints a banner saying whether Postgres cases will run |
| `mise run test:all` | whole suite with `TTS_REQUIRE_POSTGRES=1`; **fails** if Postgres is down |
| `mise run store:test` | just the store suites and the migrate tool |

The banner matters: `t.Skip` messages are invisible without `-v` and a passing
package's output is swallowed, so without it a green `go test ./...` looks
identical whether or not half the backend was exercised.

Isolation is one temp schema per test, created and dropped around each store. It
is not a temp database (`CREATE DATABASE` serializes on the template and costs
100ms+ each) and not transaction rollback (the implementations open their own
transactions, and rolling back around them would hide the `FOR UPDATE` behavior
that is the single most important thing being tested).

## Migrations

Versioned SQL under `store/{sqlite,postgres}/migrations/`, run by
`pressly/goose/v3` as a library. **They run inside `Open`**, so a fresh database
comes up ready and an existing one is carried forward; there is no separate
migrate step to forget.

Migration `00001` is frozen. It is the pre-migration schema statement for
statement, and because every statement is `IF NOT EXISTS`, applying it to the
live `bot.db` is a genuine no-op that writes only the version row. Tidying it
would break that.

To create or upgrade a schema without copying data:

```sh
TTS_DATABASE_URL='postgres:///tts' mise run db:migrate
```

## Cutover

Run off-stream, with no `!g`, `!wordle` or `!connections` round open. Escrowed
`!g` buy-ins are the one thing in the database a botched cutover could turn into
visible unfairness.

```sh
mise run bot:service:stop

# ⚠️  Archive with VACUUM INTO, never cp — see the WAL trap below.
sqlite3 bot.db "VACUUM INTO 'bot.db.pre-postgres'"
sqlite3 bot.db.pre-postgres "SELECT COUNT(*) FROM ledger"   # write this number down

export TTS_DATABASE_URL='postgres:///tts'
mise run db:create
mise run db:migrate
mise run db:cutover     # copies, then verifies; exits non-zero on any mismatch
mise run db:verify      # standalone re-check, for the record

mise run bot:service:install    # bakes TTS_DATABASE_URL into the plist
mise run bot:service:restart
```

`db:cutover` prints per-table counts and then a verification block. **Any
mismatch stops the cutover** — the tool exits non-zero and nothing has changed
yet, so the bot restarts on SQLite.

### ⚠️ The WAL trap

`bot.db` runs in WAL mode, and recent writes live in `bot.db-wal` until a
checkpoint. **`cp bot.db backup.db` copies the main file only and silently
discards every uncheckpointed row** — the archive looks fine and is missing the
most recent marks, gambles and wins. With the bot stopped, either:

```sh
sqlite3 bot.db "VACUUM INTO 'bot.db.pre-postgres'"     # preferred
```

or copy `bot.db`, `bot.db-wal` and `bot.db-shm` **together**. `VACUUM INTO` is
preferred because it produces one self-contained, already-checkpointed file.

## Rollback

| When | How |
|---|---|
| Before the restart | Nothing changed. `mise run bot:service:start` — still SQLite. |
| After the restart, before any writes | `unset TTS_DATABASE_URL`, `mise run bot:service:install`, restart. `bot.db` is untouched. |
| After live writes | `mise run db:rollback` (copies Postgres → `bot.db`, verifying), then unset and restart. **Nothing written to Postgres is lost.** |

Worst case, `bot.db.pre-postgres` is a known-good snapshot of the moment before
the cutover.

## Backups

A daily `pg_dump -Fc` LaunchAgent at 05:00, keeping 14 days in
`~/Library/Application Support/tts/backups`:

```sh
mise run db:backup:install    # install the agent
mise run db:backup            # take one now
```

**A backup that has never been restored is not a backup.** To check one:

```sh
createdb tts_restore_check
pg_restore --dbname=tts_restore_check ~/Library/Application\ Support/tts/backups/tts-<stamp>.dump
./bin/store-migrate -from "$TTS_DATABASE_URL" -to postgres:///tts_restore_check -verify-only
dropdb tts_restore_check
```

Phase 1 is daily logical dumps only — no PITR, no WAL archiving. The window of
loss is up to 24 hours.

## What the crash loop at login means

`brew services start postgresql@18` installs its own login agent, and the bot's
agent may well win the race at boot. **There is deliberately no launchd ordering
between them.**

The bot fails fast on a store error (`logger.Fatalf`), launchd's `KeepAlive`
throttles the respawn to roughly 10 seconds, and the bot comes up as soon as
Postgres is listening. So a handful of lines like this in
`~/Library/Logs/tts-bot.err.log` right after login are **normal**:

```
store postgres:///tts: connecting: failed to connect …: connection refused
```

They should stop within a minute. If they don't, Postgres isn't starting —
`mise run db:status`.

This is worth understanding rather than "fixing". A retry loop inside the bot
would mean `openStore` returns a store that isn't connected yet, which turns
every `if r.store == nil { return }` guard in `bot/` into a live code path
serving half-broken behavior to chat. Failing fast and letting launchd retry
keeps "the store works" a precondition instead of a maybe.

## Operating

```sh
mise run db:psql       # psql against $TTS_DATABASE_URL
mise run db:status     # is it up?
mise run db:verify     # compare bot.db against Postgres (during a soak)
```

The `server` process has no database access and gains none — the bot is the sole
owner, and the overlay is fed by pushes from the bot rather than by queries.

Useful now that rounds are real rows rather than a JSON blob in `settings`:

```sql
SELECT game, room_id, to_timestamp(ends_at/1000) AS ends FROM game_rounds;
SELECT reason, COUNT(*), SUM(delta) FROM ledger GROUP BY reason ORDER BY 2 DESC;
```
