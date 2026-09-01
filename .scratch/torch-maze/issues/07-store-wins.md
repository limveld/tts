# 07 — maze_wins store slice

Status: resolved
Blocked by: —

Mirror the wordle pattern exactly.

- `store/sqlite/maze.go` and `store/postgres/maze.go` — `MazeAddWin(userID,
  login, display) (wins int, err error)` and a top-N read, modelled on
  `store/sqlite/wordle.go`.
- Migration adding `maze_wins` in **both** `store/sqlite/migrations/` and
  `store/postgres/migrations/`.
- Conformance cases in `store/storetest/tallies.go` and the interface in
  `store/storetest/storetest.go`.
- Teach `cmd/store-migrate` the new table (see the recent chat-tables commit for
  the shape).

Independent of the game itself — can land before or in parallel with 01–06.

## Comments

Landed as `maze_wins`: `store/{sqlite,postgres}/maze.go`, migration `00007` in
both trees, `store.MazeWin` in types.go, conformance cases in
`store/storetest/tallies.go`, and `maze_wins` added to `cmd/store-migrate`'s copy
list. `wantSchemaVersion` bumped to 7 in both backends' migrate tests — that
tripwire fired on the first run and did exactly its job.

The bot-side `MazeWins` interface is deliberately *not* here. `storetest.Store` is
the contract a backend owes; the bot's interfaces are the consumer's view of what
it needs, and it does not need this yet. It lands in issue 08 with its caller.

### A bug this issue found in issue 03's work

`cmd/store-migrate` kept its own list of game names with a comment saying it "must
match bot/rounds.go's constants". Adding `mazeGame` in issue 03 did not update it,
so an in-flight maze round would have been silently left behind at a
SQLite→Postgres cutover. Nothing caught it, because the two lists lived in
different packages and neither could see the other.

Both now read `store.Games` (new `store/games.go`). `bot` and `cmd/store-migrate`
already imported `tts/store`, so this is a single source of truth rather than two
lists kept in step by a comment.

### A test that was aimed at the wrong migration

`TestChatLogMigrationIsReversible` used `provider.Down()`, which rolls back
whichever migration is newest. That only reached the chat log for as long as the
chat log *was* the newest migration; adding 00007 silently repointed it, and it
failed with "chat_message survived the down migration" — which reads like the chat
log is broken rather than like the test is aimed at the wrong thing. It now uses
`DownTo(5)` and stays pointed at 00006 however many migrations land on top.

### The conformance case worth having

`testTalliesAreIndependent` writes a different number of wins to all three tallies
for the same user and insists they disagree. Each backend's `maze.go` was written
by mirroring its `wordle.go`, and the way that goes wrong is a table name left
behind in a copied query — which every single-game test would still pass, since
each one only ever looks at its own leaderboard.

### Running the tests

**`go test ./...` is not enough in this repo.** With `TEST_DATABASE_URL` unset the
entire Postgres half silently skips, and a mutation sweep run that way reported
three of my changes as untested when in fact the tests were simply not running.
Use `mise run test:all`, which sets the DSN and fails if Postgres is unreachable.
Everything above was re-verified under it, including a full mutation sweep of both
backends and the migrate tool.
