# accounts + game_rounds tables, and rewiring the three games

Status: ready-for-agent
Type: task
Created: 2026-08-10

PRD: [`../PRD.md`](../PRD.md) · Depends on: 04 · Unblocks: 06

> **Highest-risk issue in the epic.** It rewrites the persistence path of three live games, one of
> which (`!g`) holds **escrowed marks** — buy-ins are debited on join and refunded from the persisted
> round. Deploy this with no round open.

## Summary

Add the two new tables on the SQLite side and move the three JSON round blobs out of `settings` into
a real `game_rounds` table. `accounts` is created here too, unused by SQLite, so that both backends
have the same table list before Postgres arrives.

## Decisions

- **`accounts` is a pure lock token** — `user_id`, `created_at`, `updated_at`, no balance column. The
  ledger stays the single source of truth, so there is no derived value that can drift out of sync
  and no dual-write to get wrong. It exists because `SUM(ledger)` cannot be locked: no predicate lock
  stops a concurrent `INSERT`, so check-then-debit needs a concrete row to serialize on.
- **Separate from `users`, not a column on it.** A chat user can reach `Spend` (`bot/router.go:202`,
  `bot/gamble.go:187`) with no `users` row at all, and `SELECT … FOR UPDATE` on a missing row takes
  **no lock**. A dedicated table lets the lock helper upsert-then-lock without inventing placeholder
  display names in the identity table.
- **SQLite creates `accounts` and never writes to it.** Single writer; the lock is unnecessary there.
  It exists purely so the migrate tool in issue 08 stays a table-for-table copier with no special
  cases.
- **`room_id` and `ends_at` are lifted into columns**, but the game code still reads both out of its
  own JSON document. The columns are for legibility — `psql` should be able to answer "is a round
  pending and when does it end" without decoding a blob. Nothing downstream depends on the split, so
  the two can never disagree in a way that breaks a game.
- **"Clear" becomes a `DELETE`, not a `""` write.** `LoadRound` returns `ok=false` when absent, so
  each load site's `!ok || value == ""` double-check collapses to one condition.

## Architecture

```
before                                     after
settings                                   game_rounds
  gamble_round      = '{"id":…,"endsAt":…}'   game='gamble'      room_id ends_at state updated_at
  wordle_round      = '{"answer":…}'          game='wordle'      …
  connections_round = '{…whole puzzle…}'      game='connections' …
  charge_mode  depth_points  depth_pb    settings  (keeps only the three real settings)

bot/gamble.go       persistGamble ─┐
bot/wordle.go       persistWordle ─┼─> bot/rounds.go  saveRound / loadRoundInto / clearRound
bot/connections.go  persistConn.  ─┘        └─> RoundStore  SaveRound / LoadRound / ClearRound
```

## Work breakdown

1. **`store/sqlite/migrations/00002_accounts_and_rounds.sql`**

   ```sql
   -- +goose Up
   CREATE TABLE IF NOT EXISTS accounts (
       user_id    TEXT PRIMARY KEY,
       created_at INTEGER NOT NULL DEFAULT 0,
       updated_at INTEGER NOT NULL DEFAULT 0);

   CREATE TABLE IF NOT EXISTS game_rounds (
       game       TEXT PRIMARY KEY,
       room_id    TEXT NOT NULL DEFAULT '',
       ends_at    INTEGER NOT NULL DEFAULT 0,   -- unix millis; 0 = no deadline
       state      TEXT NOT NULL,
       updated_at INTEGER NOT NULL DEFAULT 0);  -- unix seconds

   -- Carry the live rows over. The old clear path wrote '' rather than deleting,
   -- so empty values are skipped: those are finished rounds, not in-flight ones.
   -- All three documents already spell the deadline "endsAt" and the room "roomID".
   INSERT OR IGNORE INTO game_rounds (game, room_id, ends_at, state, updated_at)
   SELECT replace(key, '_round', ''),
          COALESCE(json_extract(value, '$.roomID'), ''),
          COALESCE(json_extract(value, '$.endsAt'), 0),
          value, 0
   FROM settings
   WHERE key IN ('gamble_round','wordle_round','connections_round') AND value <> '';

   DELETE FROM settings WHERE key IN ('gamble_round','wordle_round','connections_round');
   ```

   Confirm `json_extract` is available in the pinned `modernc.org/sqlite` build (JSON1 has been
   built in since SQLite 3.38, so it should be). If it isn't, drop the two `COALESCE` expressions to
   their defaults — the game code reads both fields out of the JSON anyway, so nothing breaks.

2. **`store/types.go`** — the `Round` type:

   ```go
   // Round is one in-flight game round: the durable state of gamble/wordle/
   // connections. State is the game's own JSON document and the store never looks
   // inside it. RoomID and EndsAt (unix millis, 0 = none) are lifted out as columns
   // so a round is legible without decoding — the game code still reads them from
   // its own document, so nothing downstream depends on the split.
   type Round struct {
       Game      string
       RoomID    string
       EndsAt    int64
       State     []byte
       UpdatedAt int64
   }
   ```

3. **`store/sqlite/rounds.go`** — `SaveRound` (upsert on `game`), `LoadRound(game) (Round, bool, error)`,
   `ClearRound(game) error`.

4. **`bot/rounds.go`** (new) — the `RoundStore` interface (added to the composite `Store` from issue
   03) plus three helpers that collapse nine near-identical functions:

   ```go
   const (
       gambleGame      = "gamble"
       wordleGame      = "wordle"
       connectionsGame = "connections"
   )

   // saveRound marshals v as game's in-flight round. Persistence failures are
   // logged, never returned: a live round must not die because the store hiccuped.
   func (r *Router) saveRound(game, roomID string, endsAt int64, v any)

   // loadRoundInto decodes game's stored round into v. ok is false when nothing is
   // stored or the document is unreadable — a corrupt round is dropped, not resurrected.
   func (r *Router) loadRoundInto(game string, v any) (ok bool)

   func (r *Router) clearRound(game string)
   ```

5. **Rewire the three games.** Each is three swaps and one deletion:
   - `bot/gamble.go` — `persistGamble` body becomes `r.saveRound(gambleGame, g.roomID, rec.EndsAt, rec)`;
     `clearGamblePersist` keeps its `gambleMu` re-check (that guard is load-bearing — it's what stops
     a settling round from clearing a newly-opened one) and calls `r.clearRound`; `loadGamble` swaps
     its `GetSetting` block for `r.loadRoundInto(gambleGame, &rec)`. Delete `gambleSettingKey`.
   - `bot/wordle.go` — same three swaps; delete `wordleSettingKey`. Hoist the anonymous persist struct
     into a named `wordleRec` so persist and load stop duplicating a six-field literal.
   - `bot/connections.go` — same three swaps; delete `connSettingKey`. `connRec` still embeds the whole
     puzzle, deliberately: a restored round must survive a corpus change.

   Note the two pre-existing unguarded writes (`bot/wordle.go` `loadWordle`, `bot/connections.go`
   `restoreConnections` assign `r.wordle` / `r.conn` without holding the mutex). They are safe only
   because they run before IRC starts. Don't make them worse; a comment saying so is enough.

## Tests

- **Legacy fixture, `store/sqlite/migrate_test.go`** — seed a pre-`00002` database with a
  `settings.gamble_round` row containing a real `gambleRec` JSON document, then `Open` and assert:
  the row is now in `game_rounds` with `room_id` and `ends_at` extracted, the `settings` key is gone,
  and `LoadRound("gamble")` returns it.
- **`bot/gamble_test.go:250` (`persistedRound`)** — the one test that reaches into persistence. Rework
  it to read `st.LoadRound(gambleGame)` and unmarshal `rec.State`.
- **Restart semantics, end to end (`bot/`)** — with a temp store: open a `!g` round with entrants,
  build a fresh `Router` over the same store, `loadGamble`, and assert the round is restored and its
  timer re-armed. Same for a half-played Wordle and a Connections board with one group solved. These
  should already exist in some form; extend them rather than duplicating.
- Round save → load → clear → load returns `ok=false`.
- `go test -race ./bot ./store/...` clean.

## Out of scope

- Normalizing round state into per-game tables (entrants, guesses, groups). The document stays opaque
  to the store.
- Writing to `accounts` from SQLite.
- Any Postgres DDL — the mirrored `00002` lands in issue 06.
- Moving `depth_points` / `depth_pb` / `charge_mode` out of `settings`. Those are genuinely settings.

## References

- `bot/gamble.go` (`gambleRec`, `persistGamble`, `clearGamblePersist`, `loadGamble`, `gambleStaleAfter`)
- `bot/wordle.go` (`persistWordle`, `clearWordlePersist`, `loadWordle`)
- `bot/connections.go` (`connRec`, `persistConnections`, `restoreConnections`)
- `bot/main.go:71-75, 141` — startup order: depth push, `loadWordle`, `loadConnections`, then
  `loadGamble` after the economy is up. That ordering comment must survive.
- `store/sqlite/settings.go` — the KV path the rounds are leaving
