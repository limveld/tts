# store-migrate, launchd, and the ADR

Status: done (2026-08-29)
Type: task
Created: 2026-08-29

PRD: [`../PRD.md`](../PRD.md) · Depends on: 01, 05 · Unblocks: —

## Summary

Teach `cmd/store-migrate` the three chat tables, install `chat-partition` at 05:30, and write it all
down.

## Decisions

- **`chat_message` derives `ts_at` on the way in**, like `ledger`, and needs its own sequence reset
  — so `resetLedgerSequence` became `resetSequence(dst, table)`.
- **05:30**, after the 05:00 dump and the ledger's 05:15 pass. gopartman's lock is per-parent so the
  two cannot deadlock, but both run partman's migrations and call `Maintain`; keeping them apart
  means a failure has one obvious owner.
- **The install warning differs from `pgpartition`'s.** For the ledger the dump *is* the archive of
  what gets dropped. For chat it is not — backups keep 14 days and retention is 90 — so it is a
  recovery window for a bug in the job, and the warning has to say which.

## What the work found

`store-migrate` copies rows as `[]any`. SQLite returns `INTEGER 0/1` for the four boolean role
columns; Postgres declares them `BOOLEAN`; pgx refuses `int64` for a bool parameter, **at encode
time**, before any cast in the statement could help. Fixed with `::int::boolean`, which changes what
pgx is asked for rather than what it is given. Caught by extending `seedSource` with chat rows —
review would not have found it.

## Done when

`TestChatMessagesSurviveTheCopy` passes along with every pre-existing `store-migrate` case in both
directions, `bash -n deploy/service.sh` is clean, and ADR-0003 plus `docs/postgres.md` are written.
