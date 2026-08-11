# Deploy: launchd agent at 05:15, service.sh target, mise tasks

Status: ready-for-agent
Type: task
Created: 2026-08-11

PRD: [`../PRD.md`](../PRD.md) · Depends on: 06 · Unblocks: 08

## Summary

Run `cmd/pg-partition` daily under launchd, copying the `pgbackup` agent line for line.

## Decisions

- **05:15, fifteen minutes after `pg-backup.sh`.** This is a safety property, not a scheduling
  detail: the 05:00 `pg_dump` contains every row the 05:15 pass is about to remove, so the
  worst-case recovery from a fold bug is a backup taken fifteen minutes earlier. **Put that in the
  plist as a comment** — otherwise the next person to tidy the schedule moves it.
- **A third launchd agent, not a step inside `pg-backup.sh`.** Backup failure and partition-
  maintenance failure want separate logs, separate exit codes and separate `service.sh status`
  answers. Chaining them means a fold bug looks like a backup bug.
- **`RunAtLoad false`.** Installing the agent must not immediately run a destructive pass. The
  operator runs `mise run db:partition:dry` first; the daily fire is the routine path.
- **`preflight()` refuses a non-`postgres://` DSN**, the same way the backup target does. On a
  SQLite deployment this agent should decline to install rather than fail nightly.
- **`ProcessType Background` and an explicit `PATH`** including `/opt/homebrew/bin` — the same
  reason the backup agent needs it, since a Homebrew Postgres is not on launchd's default path.

## Work breakdown

1. **`deploy/com.rtukpe.tts-pg-partition.plist.template`** — model on
   `com.rtukpe.tts-pg-backup.plist.template`:
   - `Label` `com.rtukpe.tts-pg-partition`
   - `StartCalendarInterval` Hour 5, Minute 15, with the comment above
   - `RunAtLoad` false, `ProcessType` Background
   - `__TTS_DATABASE_URL__`, `__REPO__`, `__LOGDIR__` substitutions
   - stdout/stderr to `__LOGDIR__/tts-pg-partition.{out,err}.log`

2. **`deploy/service.sh`** — a `pgpartition` case arm beside `pgbackup`: `LABEL`, `BIN="$REPO/bin/pg-partition"`,
   `BUILD_HINT="db:partition:build"`, `render()`, `preflight()`, `health()`. Add it to the usage
   string and to whatever `all`-style loop exists.

3. **`mise.toml`** — `db:partition:install`, `db:partition:uninstall`, `db:partition:status`,
   `db:partition:logs`, mirroring the `db:backup:*` task names exactly. (`db:partition`,
   `db:partition:dry`, `db:partition:backfill` and `db:partition:build` arrive in issue 05.)

## Tests

Deployment is not unit-testable here; the gate is a real install.

- `deploy/service.sh pgpartition render` produces a plist with no `__PLACEHOLDER__` left in it.
- `service.sh pgpartition install` → `launchctl list | grep tts-pg-partition` shows it loaded with
  no run yet.
- `launchctl kickstart -k gui/$(id -u)/com.rtukpe.tts-pg-partition` → exit 0, and
  `tts-pg-partition.out.log` shows the registration line, the `Maintain` summary and a clean
  reconcile.
- `service.sh pgpartition uninstall` removes it and leaves no plist behind.
- With `TTS_DATABASE_URL` pointed at a SQLite path, `install` refuses with a clear message.

## Acceptance

- Installed, fired once by hand, log inspected.
- `service.sh pgbackup status` and `service.sh pgpartition status` are independently meaningful.
- `docs/postgres.md` gets its operational lines in issue 08, not here.
