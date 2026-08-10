# Config, mise tasks, launchd, backups, docs, ADR

Status: ready-for-agent
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
