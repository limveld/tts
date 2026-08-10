# Package split: store becomes types-only, impl moves to store/sqlite

Status: done (2026-08-10)
Type: task
Created: 2026-08-10

PRD: [`../PRD.md`](../PRD.md) · Depends on: nothing (rebases cleanly over 01) · Unblocks: 03, 04

## Summary

Make room for a second backend. `store` keeps only the domain types; the whole SQLite implementation
moves down into `store/sqlite`. Pure mechanical change — no behavior, no SQL, no signatures change.

## Decisions

- **`store` holds types only, and imports nothing from its children.** This is what prevents the
  import cycle: `store/sqlite` imports `store` for `Command`/`LedgerEntry`, so `store` must never
  import `store/sqlite`. That is also why there is no `store.Open(dsn)` factory — see issue 03,
  where the dispatch lands at the consumer instead.
- **`git mv`, not copy-and-delete**, so the diff reads as a move and the history follows.
- **Package name is `sqlite`, not `sqlitestore`.** The import path `tts/store/sqlite` disambiguates,
  and the driver import is blank (`_ "modernc.org/sqlite"`) so there is no collision.

## Architecture

```
before                          after
store/                          store/
  store.go   package store        types.go        package store  — types only
  points.go  package store        sqlite/
  settings.go                       store.go      package sqlite
  wordle.go                         points.go
  connections.go                    settings.go
  *_test.go                         wordle.go
                                    connections.go
                                    *_test.go
```

## Work breakdown

1. **`store/types.go`** (new, `package store`) — move the exported domain types out of the impl
   files, with their doc comments:
   - `Command` (from `store/store.go:15`)
   - `LedgerEntry` (from `store/points.go:14`)
   - `WordleWin` (from `store/wordle.go`)
   - `ConnectionsWin` (from `store/connections.go`)

   Give the package a doc comment explaining that it is deliberately type-only and why (the cycle).

2. **`git mv store/{store,points,settings,wordle,connections}.go store/sqlite/`** plus the three test
   files. Change `package store` → `package sqlite` in each. Add `import "tts/store"` and qualify the
   moved types: `Command` → `store.Command`, `[]LedgerEntry` → `[]store.LedgerEntry`, etc.

3. **Update the `Store` doc comment** in `store/sqlite/store.go` — it currently says "Stage 2 holds
   custom commands; later stages (loyalty points) add tables to the same DB", which is stale. Say
   what it is now: the SQLite implementation of the bot's store.

4. **Update importers.** `bot/main.go:29` (`store.Open`), `bot/router.go:38`, `bot/economy.go:158`,
   plus wherever `store.Command` is named (`bot/commands.go`, `bot/main.go` `seedCommands`) and the
   five bot test files that call `store.Open` (`bot/charge_test.go:18`, `bot/commands_test.go:15`,
   `bot/economy_test.go:50`, and the two helpers derived from them). Most become a two-import split:
   `tts/store` for types, `tts/store/sqlite` for `Open`.

5. **`go mod tidy`** — this will promote `modernc.org/sqlite` from `// indirect` to a direct
   requirement, which it has always actually been.

## Tests

- No new tests. The existing suite must pass untouched apart from package/import lines.
- `go build ./... && go vet ./... && go test ./...` green.
- `git log --follow store/sqlite/points.go` shows the pre-move history.

## Out of scope

- Interfaces (issue 03), migrations (04), new tables (05), Postgres (06).
- Moving the tests into `storetest` — that happens in issue 07, after there are two backends to run
  them against.

## References

- `store/store.go`, `store/points.go`, `store/settings.go`, `store/wordle.go`, `store/connections.go`
- Importers: `bot/main.go`, `bot/router.go`, `bot/economy.go`, `bot/commands.go`

## Comments

**2026-08-10 — shipped.**

`store/types.go` holds `Command`, `LedgerEntry`, `WordleWin`, `ConnectionsWin` and a package doc
explaining the type-only rule and why there is no `store.Open` factory. The five implementation
files and the three test files moved with `git mv`, so `git log --follow` sees through it.

Importer split came out as expected: `bot/router.go` and `bot/economy.go` need only
`tts/store/sqlite` (their remaining `store.` hits are the `r.store.`/`e.store.` *field*, not the
package); `bot/main.go` and `bot/commands_test.go` need both; `bot/commands.go` keeps only
`tts/store`. `go mod tidy` promoted `modernc.org/sqlite` to a direct requirement.
