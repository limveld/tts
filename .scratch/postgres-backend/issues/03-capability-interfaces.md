# Capability interfaces at the consumer + openStore factory

Status: ready-for-agent
Type: task
Created: 2026-08-10

PRD: [`../PRD.md`](../PRD.md) · Depends on: 02 · Unblocks: 06

## Summary

Put the seam in. Six per-feature interfaces declared in `bot/` where they're consumed, one composite
so `Router.store` stays a single field, and an `openStore(dsn)` factory that dispatches on the DSN
scheme. Only type declarations change — **every `r.store.X(...)` call site stays byte-identical**.

## Decisions

- **Interfaces are declared at the consumer, split by feature**, matching the house convention
  (`TTS` in `bot/tts.go`, `OverlayPusher` in `bot/overlay.go`, `TwitchAPI` in `bot/economy.go`,
  `Player`/`Synthesizer` in `server/`). A single fat 21-method interface would be less work and less
  honest: `Economy` needs the ledger and nothing else, and its fake should have to prove that.
- **Plus one composite `Store`.** Without it, `Router` would carry six fields and `main.go` would
  wire six times. The composite is the *field type*; the capabilities are the *dependency
  declarations*.
- **The factory lives in `bot/store.go`, not in `store`.** A factory in `store` would import
  `store/sqlite`, which imports `store` — a cycle. See issue 02.
- **`-db` keeps its name and its default.** A bare path is still SQLite, so nothing about the current
  invocation changes.

## Architecture

```
bot/commands.go     CommandStore     Get Add SetResponse Delete List IncCount
bot/economy.go      Ledger           UpsertUser ResolveLogin Balance Credit
                                     Grant Spend Transfer Leaderboard
bot/store.go        SettingStore     GetSetting SetSetting
bot/rounds.go       RoundStore       (declared in issue 05)
bot/wordle.go       WordleWins       WordleAddWin WordleLeaderboard
bot/connections.go  ConnectionsWins  ConnectionsAddWin ConnectionsLeaderboard

bot/store.go        Store = all of the above + Close() error
                    openStore(dsn) (Store, error)
                      "postgres://" | "postgresql://"  -> postgres.Open(dsn)   [issue 06]
                      "sqlite://"                      -> sqlite.Open(trimmed)
                      anything else                    -> sqlite.Open(dsn)     [a file path]
```

## Work breakdown

1. **Declare the five interfaces available today** in their consumer files, each with a doc comment
   saying what it is for and who backs it. Signatures are copied verbatim from the current
   `*sqlite.Store` methods, with `store.`-qualified types.

2. **`bot/store.go`** (new) — `SettingStore`, the composite `Store`, and `openStore`. Two things the
   doc comment must carry:
   - **A nil `Store` means "persistence disabled"**, and every consumer nil-checks it. Never assign a
     *typed* nil (`var s *sqlite.Store; r.store = s`) — `r.store == nil` would be false and all ~35
     guards would silently fall through into a nil-pointer panic. Construct through `openStore` or
     leave the field zero.
   - **The bot is the sole database owner.** The `server` process has no store and gains none here.

3. **Swap the field types.** `bot/router.go:38` `store *store.Store` → `store Store`.
   `bot/economy.go:158` → `store Ledger`, and `NewEconomy` takes a `Ledger`. `bot/main.go:173`
   `seedCommands(db CommandStore, …)`. `bot/main.go:29` calls `openStore(cfg.DBPath)`.

4. **Compile-time assertion** in `bot/store.go`: `var _ Store = (*sqlite.Store)(nil)`. This is what
   makes "the backend satisfies the contract" a build error rather than a runtime surprise — and in
   issue 06 the Postgres line next to it is the entire acceptance test until the conformance suite
   exists.

5. **`bot/store_test.go`** (new) — `newTestStore(t *testing.T) Store`, a throwaway SQLite store,
   replacing the five inline `sqlite.Open(filepath.Join(t.TempDir(), …))` bodies. Doc comment: the
   bot's tests care about chat behavior, not backends; backend equivalence is proven once in
   `store/storetest` (issue 07).

## Tests

- No behavioral tests. The change is proven by the tree compiling and the existing suite passing.
- Confirm the call-site diff is genuinely empty: `git diff` should show no line containing
  `r.store.` or `e.store.` other than in the files whose *type declarations* changed.
- `go test -race ./bot/...` clean.

## Out of scope

- `RoundStore` and the `bot/rounds.go` file — issue 05, once `game_rounds` exists.
- Any Postgres code. `openStore` gets its `postgres://` arm in issue 06; until then that branch
  returns an "unsupported scheme" error.
- Running `bot/` tests against Postgres. They stay on SQLite, permanently.

## References

- Precedent for interface-at-the-consumer: `bot/tts.go` (`TTS`), `bot/overlay.go` (`OverlayPusher`),
  `bot/admin.go` (`UserResolver`), `bot/info.go` (`TwitchInfo`), `bot/chat.go` (`Chat`)
- Fields being retyped: `bot/router.go:38`, `bot/economy.go:158`
- Nil-guard pattern that must keep working: every `if r.store == nil { return }` in `bot/`
