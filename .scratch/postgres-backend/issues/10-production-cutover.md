# Execute the production cutover

Status: cut over 2026-08-10 22:00 — steps 1-7, 10 done; live chat smoke tests (8) and the soak (9, 11) pending
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

## Comments

**2026-08-10 — issues 01–09 shipped; this one is yours to run.**

The code is done and proven. This issue is deliberately *not* executed: it needs the stream offline,
no round open, and live chat smoke tests. What follows is the runbook adjusted to what actually
landed.

### Adjustments to the steps above

- **Postgres 18, not 17.** `mise run db:install` pins `postgresql@18`, which is what this Mac already
  runs.
- **`mise run test:all`** is the green-before-you-start check (step 1). It defaults
  `TEST_DATABASE_URL` to `postgres:///tts_test` and fails if Postgres is down.
- **`mise run db:rollback`** exists — it is `store-migrate -from "$TTS_DATABASE_URL" -to bot.db
  -force` with the verification, i.e. the third row of the rollback table.
- Step 7's flip is `mise run bot:service:install` (which bakes `TTS_DATABASE_URL` into the plist)
  then `mise run bot:service:restart`. `service.sh` warns but does not block if Postgres is down.

### Already exercised against the real data (on copies, never the live file)

- A `VACUUM INTO` copy of `bot.db` migrates cleanly to schema 2: 16,864 ledger rows intact,
  `settings` down from 6 rows to 3 as the three `*_round` keys move out, `game_rounds` empty
  (correct — all three live round values are `''`, i.e. finished).
- A full copy into Postgres verifies: **114/114 balances match, sum(delta) 20,089 == 20,089, counts
  match, leaderboard top-50 identical**, ledger sequence at 16,865.
- The reverse direction (Postgres → a fresh SQLite file) verifies identically.
- A `pg_dump -Fc` of that database was restored into a scratch database and verified.

So step 6's numbers are known in advance: **3 commands · 114 users · 16,864 ledger · 3 settings ·
5 wordle_wins · 1 connections_wins · 0 game_rounds · sum(delta) 20,089 · max id 16,864 · sequence
16,865.** Anything else means something changed between now and the cutover, which is worth
understanding before continuing.

### Still yours

Steps 1–2 (stream offline, no round open, stop the bot), 3 (archive — **`VACUUM INTO`, never `cp`**),
7 (flip), 8 (live chat smoke tests), 9 (watch the logs through a stream), 10 (`mise run
db:backup:install` and restore one by hand), 11 (after the soak).

### Executed 2026-08-10 ~22:00

Pre-flight was better than the runbook assumes: the bot was **already stopped** (no process, agent
unloaded), and all three `*_round` settings values were `''`, so no escrowed `!g` marks were at risk.
Steps 1–2 needed nothing. `mise run test:all` green, Postgres 18 accepting connections.

One check the runbook doesn't list and should: **the installed plist predated `TTS_DATABASE_URL`
(rendered 2026-07-09), so step 7's reinstall re-renders every baked-in secret from the installing
shell.** Compared SHA-256 prefixes of `TTS_TOKEN` / `TWITCH_CLIENT_ID` / `TWITCH_CLIENT_SECRET` and
the `-channel` argument between the old plist and the shell before installing — all four identical,
so the reinstall changed exactly one thing. Had they differed, the flip would have silently disabled
chat replies and the cause would have looked like a Postgres problem.

| Step | Result |
|---|---|
| 3 archive | `bot.db.pre-postgres` (880K) — ledger/sum/max id identical to live |
| 4 create + migrate | `tts` at schema 2, 8 tables + `goose_db_version`, empty |
| 5 copy | 3 · 114 · 0 · **16,864** · 3 · 5 · 1 · 0, sequence → 16,865 |
| 6 verify | **114/114 balances · sum(delta) 20,089 · counts match · top-50 identical** |
| 7 flip | agent reinstalled with the DSN, bot up as pid 52051, joined `#rtukpe` 21:59:32 |
| 10 backup | `tts-20260810-2200.dump` (84K) taken, `pg_restore`d into a scratch DB, **verified 114/114 against live**, scratch dropped |

Post-flip confirmation: an idle `pgx` connection on the `tts` database; `settings.charge_mode` read
from Postgres as `free`, matching the log's `economy enabled (free)`; pid stable across samples with
`last exit code = (never exited)`, so no crash loop; and **no deadlock, duplicate-key, SQLSTATE or
5432 errors** in `tts-bot.err.log`.

The one `connection refused` in the log is port **8080 — the TTS server**, which is a separate agent
and is currently not loaded. Unrelated to the cutover, but it means `!tts`/`!sfx` produce no audio
and the overlay gets no pushes until `mise run server:service:install`.

Both rollback paths are live: `bot.db` (untouched data, now itself at schema 2 because the copier
opens it through `Open`) and the pre-migration `bot.db.pre-postgres`.
