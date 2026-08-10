# Config, mise tasks, launchd, backups, docs, ADR

Status: done (2026-08-10)
Type: task
Created: 2026-08-10

PRD: [`../PRD.md`](../PRD.md) · Depends on: 06 · Unblocks: 10

## Summary

Everything around the code: how the DSN reaches the bot, the `db:*` task group, the launchd changes,
a daily `pg_dump`, the runbook, and the repo's first ADR.

## Decisions

- **`-db` keeps its name.** It already means "the database"; it just accepts a `postgres://` URL now
  as well as a path. Adding a second `-database-url` flag would create two ways to say one thing and
  a precedence question nobody wants to answer.
- **Env var is `TTS_DATABASE_URL`**, matching the `TTS_`-prefixed convention (`TTS_TOKEN`,
  `TTS_CHANNEL`) rather than the generic `DATABASE_URL`. `TEST_DATABASE_URL` (issue 07) stays
  unprefixed because it's a test-only convention, not a service setting.
- **No launchd ordering, by design.** `brew services start postgresql@17` installs its own login
  agent and the bot may well win the race at boot. That is precisely what the fail-fast decision
  covers: the bot `Fatalf`s, launchd throttles the respawn to ~10s, and it comes up once Postgres is
  listening. Document the handful of `connection refused` lines at login as *normal*, so nobody
  "fixes" it later by adding a retry loop.
- **Backup is a daily `pg_dump -Fc` LaunchAgent**, 14-day retention. It is the only piece of the
  "durability" motivation that phase 1 actually delivers, so it ships here rather than being deferred.
- **First ADR in the repo.** `CLAUDE.md` already points at `docs/adr/`; the directory doesn't exist
  yet. "Swap the persistence engine" is exactly the class of decision that warrants one.

## Work breakdown

1. **`bot/config.go`** — one line changes meaning, none change name:

   ```go
   fs.StringVar(&c.DB, "db", orString(os.Getenv("TTS_DATABASE_URL"), "bot.db"),
       "database: a postgres:// DSN, or a SQLite file path (env TTS_DATABASE_URL)")
   ```

   Rename the field `DBPath` → `DB` and update its comment. `bot/main.go:29` calls
   `openStore(cfg.DB)`; `logger.Fatalf` at `:31` stays exactly as it is.

2. **`mise.toml`** — a `db:*` group plus test umbrellas:

   | Task | Does |
   |---|---|
   | `test` | preflight banner, then `go vet ./... && go test ./...` |
   | `test:all` | `TTS_REQUIRE_POSTGRES=1`, fails if Postgres is down |
   | `store:test` | `go test ./store/...` |
   | `db:install` | `brew install postgresql@17 && brew services start postgresql@17` |
   | `db:start` / `db:stop` / `db:status` | `brew services` + `pg_isready` |
   | `db:create` | `createdb tts` (idempotent) |
   | `db:test:setup` | `createdb tts_test`, echo the `export TEST_DATABASE_URL=…` line |
   | `db:migrate` | `store-migrate -migrate-only -to "$TTS_DATABASE_URL"` |
   | `db:cutover` | `store-migrate -from bot.db -to "$TTS_DATABASE_URL"` |
   | `db:verify` | the same, `-verify-only` |
   | `db:psql` | `psql "$TTS_DATABASE_URL"` |
   | `db:backup` / `db:backup:install` | `deploy/pg-backup.sh` / install its agent |

   Keep `bot:test` as-is (muscle memory) and make `test` the umbrella. The preflight banner is where
   a human actually learns Postgres cases were skipped — `t.Skip` is invisible without `-v`.

3. **`deploy/com.rtukpe.tts-bot.plist.template`** — add `TTS_DATABASE_URL` to
   `EnvironmentVariables` behind a `__TTS_DATABASE_URL__` token; extend `PATH` with
   `/opt/homebrew/bin` so `psql`/`pg_dump` are reachable from the agent.

4. **`deploy/service.sh`** — `TTS_DB_ESC=$(esc "${TTS_DATABASE_URL:-}")` and the matching `sed` rule.
   Add a bot `preflight` that **warns but does not block** when the DSN is a `postgres://` URL and
   `pg_isready` fails — blocking would defeat the fail-fast-and-retry design.

5. **`deploy/pg-backup.sh`** — `pg_dump -Fc` to
   `~/Library/Application Support/tts/backups/tts-YYYYmmdd-HHMM.dump`, prune older than 14 days, log
   one summary line. Plus `deploy/com.rtukpe.tts-pg-backup.plist.template` with
   `StartCalendarInterval` at 05:00 and `RunAtLoad=false`. `service.sh` already dispatches on
   `<target> <cmd>`, so add `pgbackup` as a third target (~20 lines: `render()`, a no-op `health()`,
   `preflight`).

6. **`docs/postgres.md`** — install and create, dev/test setup, migrate, cutover, rollback, restore
   from a dump, and a "what the crash loop at login means" section. Pin the formula version and note
   that a major upgrade (`@17` → `@18`) needs `pg_upgrade` and takes the bot down — it is a scheduled
   task, never a `brew upgrade` side effect.

7. **`docs/adr/0001-postgres-backend.md`** — the repo's first ADR. Records: two backends behind
   consumer-side capability interfaces; ledger-as-truth with `accounts`-as-lock (and why not a
   materialized balance); goose-as-library; rounds promoted out of `settings`; the server stays
   DB-free; **and the dependency count going 4 → 6**, so the near-stdlib convention is consciously
   spent rather than quietly eroded. Acknowledge that `../tts-server/issues/02`'s "native, not
   Docker" decision is honored — Postgres is a Homebrew service, not a container.

## Tests

- `bash deploy/service.sh bot render` (or equivalent dry run) shows the DSN substituted and no
  leftover `__TTS_DATABASE_URL__` token.
- `mise run db:install && mise run db:create && mise run db:test:setup && mise run test:all` green
  from a clean state.
- `mise run test` with no Postgres running prints the banner and still exits 0.
- `deploy/pg-backup.sh` produces a restorable dump; verify by `pg_restore` into a scratch database
  and running `store-migrate -verify-only` against it. **A backup that has never been restored is not
  a backup.**
- Retention: seed 20 fake dated dumps, run the script, confirm exactly the last 14 days survive.
- `bot -h` shows the new `-db` help text.

## Out of scope

- CI, containers, `docker-compose`.
- Off-box Postgres, TLS/`sslmode=verify-full`, connection pooling middleware.
- PITR / WAL archiving. Daily logical dumps are phase 1.
- Backing up `bot.tokens.json` or the `sfx/` directory.

## References

- `bot/config.go` (`LoadConfig`, the `-db` flag), `bot/main.go:29-31`
- `deploy/service.sh`, `deploy/com.rtukpe.tts-{bot,server}.plist.template`
- `mise.toml` (existing `bot:test`, `reload`, the `*:service:*` group)
- `docs/agents/domain.md` for the domain-doc conventions; the `/domain-modeling` skill carries the
  ADR format (`ADR-FORMAT.md`) and creates `docs/adr/` lazily — this issue is the first thing that
  actually needs it
- `../tts-server/issues/02-run-as-a-service-mise-tasks-launchd.md` — the native-not-Docker decision

## Comments

**2026-08-10 — shipped.**

`Config.DBPath` → `Config.DB`, defaulting to `orString(os.Getenv("TTS_DATABASE_URL"), "bot.db")`.
`bot -h` reads: *database: a postgres:// DSN, or a SQLite file path (env TTS_DATABASE_URL) (default
"bot.db")*. `logger.Fatalf` at `bot/main.go` is untouched.

**Postgres 18, not 17.** This Mac already runs `postgresql@18` via Homebrew (plus mise's postgres
18.4 on PATH, same major, wire-compatible), so `db:install` and the docs pin `@18`. Nothing in the
epic depended on 17.

mise gained `test` (with the preflight banner), `test:all`, `store:test`, and the `db:*` group —
including a `db:rollback` the issue didn't list, which is just the reverse `store-migrate` invocation
and the one you'd most want spelled out at 2am. `bot:test` is untouched.

`service.sh` gained the `TTS_DATABASE_URL` substitution, a `pgbackup` target, and a bot preflight
that **warns without blocking** when the DSN is Postgres and `pg_isready` fails. Render verified: the
DSN substitutes, no `__TTS_DATABASE_URL__` token survives either with a DSN or without one, and
`pgbackup render` has zero leftover tokens.

**The backup was restored, not just taken.** Against the 16,864-row dry-run database: `pg_dump -Fc`
produced an 84K dump, `pg_restore` into a scratch database succeeded, and
`store-migrate -verify-only` against the original SQLite file reported *114/114 balances match ·
counts match · leaderboard top-50 match*. Retention checked with 20 dated dumps: 6 pruned, exactly
the last 14 days kept.

`pg-backup.sh` writes to a `.part` file and renames on success, so an interrupted run can't leave a
truncated file that looks like a good backup.

`docs/postgres.md` covers install, dev/test, migrations, cutover, rollback, restore, and a "what the
crash loop at login means" section that explains why there is no launchd ordering and why a retry
loop would be a regression rather than a fix. `docs/adr/0001-postgres-backend.md` is the repo's
first ADR; `docs/adr/` didn't exist until now.
