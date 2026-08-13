# Serialize token refresh; make persistence atomic and non-fatal

Status: done (2026-08-13)
Type: task
Created: 2026-08-13

## Summary

Channel-point redemptions stopped being credited on 2026-08-13 at 09:40, right after the Mac woke
from ~18h of sleep:

```
09:40:34 events: stream info: refresh after 401: persist refreshed token: rename bot.tokens.json.tmp bot.tokens.json: no such file or directory
09:40:34 conversion: get-redemptions: refresh after 401: persist refreshed token: rename bot.tokens.json.tmp bot.tokens.json: no such file or directory
09:40:34 accrual: is-live: Get ".../helix/streams?user_id=87773600": context deadline exceeded
09:40:35 events: stream info: refresh after 401: persist refreshed token: rename ...
09:40:35 conversion: get-redemptions: refresh after 401: persist refreshed token: rename ...
```

`~/Library/Logs/tts-bot.err.log` jumps straight from `08/12 15:10:37` to `08/13 09:40:34` — the
access token expired while the machine was out.

Root cause is an **unsynchronized refresh herd**. Four callers share one `*twitch.Client` — accrual
and conversion (`bot/economy.go`), events (`bot/events.go`), and IRC-driven chat (`bot/chat.go`). On
wake they all 401 at the same instant and each independently ran `Client.doRefresh`. Nothing deduped
them: `c.mu` was only held for the three lines that swapped the `c.token` pointer, never across the
exchange or the disk write.

## Decisions

- **The ENOENT was the symptom, not the bug.** `Store.Save` used a *fixed* temp path
  `s.path + ".tmp"`. Concurrent savers wrote the same file; the first `os.Rename` moved it away and
  the rest failed with `ENOENT`. A rename could also publish another goroutine's bytes. Fixing only
  this would have left the herd firing N `grant_type=refresh_token` exchanges on every wake — and
  Twitch rotates refresh tokens, so the losers get `400 Invalid refresh token`.
- **A failed persist must never fail the request.** `doRefresh` installed the new token *before*
  saving it but still returned the Save error, so `doRetry` bailed and never ran the retry while
  holding a perfectly valid token. That is the dropped `get-redemptions` poll. Persisting is now
  best-effort and logged loudly; a lost write only bites on the next restart.
- **Dedup by comparing against the token that failed**, not by memoizing a result.
  `ensureFresh(ctx, staleAccess)` takes the access token the caller actually sent; the winner
  exchanges, and anyone queued behind it sees `c.token` has already moved past `staleAccess` and
  returns without exchanging. Failures are deliberately not memoized — the next 401 retries.
- **A channel, not a `sync.Mutex`, for the refresh semaphore.** Waiters must be able to give up when
  their request context dies; the loops use 10–15s deadlines and the `context deadline exceeded` line
  above shows a stalled leader is a real state. `Mutex.Lock` isn't cancellable.
- **`Token.Expiry` was written to the store but read nowhere in the repo.** Refresh was purely
  reactive to a 401, which is exactly why wake-from-sleep produced a simultaneous storm. It now drives
  a proactive refresh with a 2-minute skew (Twitch tokens live ~4h, so that is one extra exchange per
  lifetime). Best-effort: if the pre-flight exchange fails we still send, because the recorded expiry
  may be pessimistic and the 401 path is the real safety net.
- **A zero `Expiry` means "unknown", not "expired".** Treating it as expired would refresh on every
  request for tokens written before the field existed.
- **`Expiry` has to drop its monotonic reading.** `time.Now().Add(...)` keeps it, and macOS's
  monotonic clock stops while the machine sleeps — so a token minted in-process before an 18h sleep
  still compared as *fresh* on wake, the precise case the check exists for. `.Round(0)` makes the
  comparison wall-clock, which is also what survives the JSON round trip.
- **A refresh response that omits `scope` would silently disable the economy on the next restart.**
  `bot/main.go` gates the economy on the *stored* token's scopes and `Scope` is `omitempty`, so the
  carry-forward rule covers `Scope` as well as `RefreshToken`.
- **Persist errors surface via an optional `*log.Logger` on the Client**, not a typed error. All four
  `do()` call sites (`getJSON`, `SendChatMessage`, `EnsureReward`, `FulfillRedemptions`) would have
  had to learn to unwrap a typed error, and one forgotten site reintroduces the bug. `SetLogger`
  mirrors the existing `SetToken` mutator and leaves `NewClient`'s two call sites alone.

Lock order is `refreshing` → `mu` → (`Store.mu`, inside `Save`), so deadlock is structurally
impossible. `mu` is never held across a network call or a disk write, and an installed `*Token` is
never mutated in place — a refresh builds a new one and swaps the pointer.

## Work breakdown

1. **`twitch/store.go`** — `Store.mu`; `os.CreateTemp(dir, filepath.Base(path)+".*.tmp")` so no two
   savers share a name, `Chmod(0600)` → `Write` → `Sync` → `Close` → `Rename`, `fsyncDir` after, and
   a `defer` that removes the temp on every failure path (it holds live secrets). `Sync` before the
   rename so a crash can't leave the store pointing at contents that never reached disk. Note
   `filepath.Dir("bot.tokens.json")` is `"."`, not `""`; passing `""` to `CreateTemp` would land in
   `os.TempDir()` and break the atomic rename across filesystems.
2. **`twitch/client.go`** — `refreshSkew`, `(*Token).needsRefresh`, `Client.errLog`,
   `Client.refreshing chan struct{}` (cap 1, initialized in `NewClient`), `SetLogger`, `logf`,
   `currentToken`; `SenderID` rebuilt on `currentToken`.
3. **`twitch/helix.go`** — `doRefresh` → `ensureFresh(ctx, staleAccess)`; proactive refresh in
   `doRetry` gated on `allowRefresh` so the retry can't re-enter it.
4. **`twitch/oauth.go`** — reject a 200 with no `access_token`; `.Round(0)` on `Expiry`.
5. **`bot/main.go`** — `client.SetLogger(logger)` in `buildChat`. `cmd/bot-auth` needs no change; it
   never refreshes and leaves the logger nil.
6. **`.gitignore`** — also ignore `/bot.tokens.json.*.tmp`; the launchd `WorkingDirectory` is the repo
   root and the default store path is a bare filename, so temps land in the repo.
7. **`mise.toml`** — `bot:build` `sources` listed only `bot/*.go`, so a `twitch/`-only change left the
   fingerprint unchanged, the task no-opped, and `bot:service:restart` would have relaunched the
   **stale binary**. Now covers every package `go list -deps ./bot` reports; `server:build` had the
   same gap for `sfxlib`.

## Tests

New `twitch/refresh_test.go`, plus two in `twitch/twitch_test.go`. All five fail against the pre-fix
code, and `TestConcurrent401RefreshesOnce` / `TestStoreConcurrentSave` reproduce the production error
string verbatim (`rename ...tok.json.tmp ...tok.json: no such file or directory`).

- **`TestConcurrent401RefreshesOnce`** — 8 goroutines call `IsLive` against a server that 401s the old
  token and delays the exchange 50ms. Asserts every caller succeeds, `refreshCalls == 1`, every
  exchange sent `r1`, and the store holds `new-access` with identity carried across. Pre-fix:
  `refreshCalls=8` and 7 of 8 callers ENOENT.
- **`TestRefreshSurvivesSaveFailure`** — store path is a *directory*, so the rename can never land.
  Asserts `SendChatMessage` returns nil, the retry carried `Bearer new-access`, and the log says
  `NOT persisted`.
- **`TestProactiveRefresh`** — table-driven over `Expiry` ∈ {expired, within skew, fresh, zero}; the
  Helix handler always 200s, so a refresh can only be proactive.
- **`TestRefreshPreservesOmittedFields`** — response carries neither `refresh_token` nor `scope`;
  asserts both are carried across into the store.
- **`TestStoreConcurrentSave`** — 16 concurrent saves: no error, `Load` parses (never torn), and the
  directory holds exactly one file (no leftover `*.tmp` full of secrets).
- **`TestStoreRoundTrip`** — extended with a `0600` perms assertion.

`TestSendChatMessageRefreshesOn401` and all of `api_test.go` pass unmodified.

## Acceptance

- `mise run test` green; `go test ./twitch/... -race -count=1`; concurrency tests shaken with
  `go test -run 'Refresh|Store|401' ./twitch/... -race -count=20`.
- After deploy, a forced 401 storm produces exactly one refresh, no `rename ... no such file or
  directory`, and no `conversion: get-redemptions: refresh after 401:`.
- `ls -l bot.tokens.json*` shows exactly one file, mode `-rw-------`.
