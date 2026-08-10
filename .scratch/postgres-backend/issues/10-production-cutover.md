# Execute the production cutover

Status: ready-for-agent
Type: task
Created: 2026-08-10

PRD: [`../PRD.md`](../PRD.md) · Depends on: 07, 08, 09 · Unblocks: the follow-up off-box epic

## Summary

Move the live data to Postgres and run the bot on it. The code is done and proven by this point; this
issue is a runbook, executed once, with a verified rollback at every step.

## Decisions

- **Run it off-stream, with no game round open.** Escrowed `!g` buy-ins are the one thing in the
  database that a botched cutover could turn into real, visible unfairness.
- **Archive with `VACUUM INTO`, never `cp`.** See the warning below. This is the single most
  dangerous step in the epic and it looks completely harmless.
- **Verify before flipping, not after.** `store-migrate` exits non-zero on any mismatch; nothing gets
  restarted until it exits 0.
- **Keep the SQLite path live for a soak.** Rolling back is one env var for as long as `bot.db` sits
  there untouched — and after the soak, the reverse copy recovers everything written since.

## ⚠️ The WAL trap

`bot.db` is ~900 KB with a **3.9 MB uncheckpointed `-wal`**. A plain `cp bot.db backup.db` copies the
main file only and **silently discards every recent ledger row** — the archive would look fine and be
missing the most recent marks, gambles and wins. Either:

```sh
sqlite3 bot.db "PRAGMA wal_checkpoint(TRUNCATE);"   # with the bot STOPPED
sqlite3 bot.db "VACUUM INTO 'bot.db.pre-postgres'"
```

or copy `bot.db`, `bot.db-wal` and `bot.db-shm` **together**, with the bot stopped. `VACUUM INTO` is
preferred: it produces one self-contained, already-checkpointed file.

## Runbook

1. **Pre-flight.** No `!g` / `!wordle` / `!connections` round open; stream offline.
   `mise run db:status` → Postgres up. `mise run test:all` → green.
2. **Stop the bot.** `mise run bot:service:stop`. Confirm with `bot:service:status`.
3. **Archive.** `sqlite3 bot.db "VACUUM INTO 'bot.db.pre-postgres'"`. Record the file size and
   `SELECT COUNT(*) FROM ledger` from the archive — those are the numbers step 6 must match.
4. **Create and migrate.** `mise run db:create` then `mise run db:migrate` (`-migrate-only`), so the
   destination schema exists at version 2 before any rows move.
5. **Copy.** `mise run db:cutover` — `store-migrate -from bot.db -to "$TTS_DATABASE_URL"`. Read the
   table counts against step 3.
6. **Verify.** The tool verifies inline; run `mise run db:verify` again standalone for a clean record.
   Required: **114/114 balances match**, row counts match, `SUM(delta)` matches, `MAX(ledger.id)`
   matches, top-50 leaderboard identical, ledger sequence past `MAX(id)`.
   **Any mismatch stops the cutover.** Nothing has changed yet; the bot restarts on SQLite.
7. **Flip.** Set `TTS_DATABASE_URL` and reinstall the agent so the plist carries it
   (`mise run bot:service:install`), then `mise run bot:service:restart`.
8. **Smoke test, live.** In chat:
   - `!marks` returns the same balance it did before the cutover (check two or three known users)
   - `!leaderboard` matches the pre-cutover top 10
   - a custom command fires and its `$count` increments
   - `!don +10` moves the depth widget and survives a bot restart
   - open a `!g` round with a second account, join, restart the bot mid-round, confirm the round
     restores and settles with correct payouts
   - start a `!wordle`, guess once, restart, confirm the board comes back
   - `!grant` a small amount and `!give` it away — the two locked paths
9. **Watch the logs** through the next stream. `~/Library/Logs/tts-bot.err.log`. Expect nothing;
   specifically expect **no** `deadlock detected` and no duplicate-key errors on `ledger`.
10. **Install backups.** `mise run db:backup:install`, then `mise run db:backup` once by hand and
    restore that dump into a scratch database to prove it works.
11. **After the soak** (a couple of streams): archive `bot.db.pre-postgres` somewhere durable and note
    in the progress log that the reverse-direction rollback is now the supported path.

## Rollback

| When | How |
|---|---|
| Before step 7 | Nothing changed. `bot:service:start` — still SQLite. |
| After step 7, before any writes | Unset `TTS_DATABASE_URL`, reinstall the agent, restart. `bot.db` is untouched. |
| After live writes | `store-migrate -from "$TTS_DATABASE_URL" -to bot.db -force`, verify, then unset and restart. Nothing written to Postgres is lost. |

Worst case, `bot.db.pre-postgres` is a known-good snapshot of the moment before the cutover.

## Tests

Live, from the runbook — steps 6, 8 and 10 are the acceptance criteria:

- [ ] `store-migrate` verification passes on real data: 114/114 balances, counts, sum, max id, top-50
- [ ] Bot starts clean on the Postgres DSN; no errors in `tts-bot.err.log`
- [ ] `!marks` / `!leaderboard` match pre-cutover values for known users
- [ ] A `!g` round survives a mid-round bot restart and pays out correctly
- [ ] A `!wordle` board survives a restart
- [ ] `!grant` and `!give` both work
- [ ] `pg_dump` backup taken and **restored** into a scratch database, verified
- [ ] One full stream with no database errors

## Out of scope

- Moving Postgres or the bot off the Mac. That is the follow-up epic this unblocks.
- Deleting the SQLite backend. It stays.
- Dashboards or analytics against the new database.

## References

- The tool: issue [08](08-store-migrate-tool.md) (`cmd/store-migrate`)
- Tasks and plist changes: issue [09](09-config-deploy-backups-docs.md)
- Startup order that must still hold: `bot/main.go:71-75, 141` (depth push → wordle → connections →
  economy → `loadGamble`)
- `deploy/service.sh`, `~/Library/Logs/tts-bot.{out,err}.log`
