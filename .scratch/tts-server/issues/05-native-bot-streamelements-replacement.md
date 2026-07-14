# Native Go bot: make it reply-capable + replace StreamElements

Status: in progress — Stage 3 done (marks economy + gamble/give + owner grant/free-paid); retire-SE (Stage 4) remains
Type: task
Created: 2026-07-03

## Summary

Evolve our anonymous, read-only Twitch bot into an **authenticated, reply-capable** bot that
replaces StreamElements' **chatbot layer** — custom commands, timers/announcements, and a
**loyalty-points economy** — so we can drop SE. This is the "SE-replacement" rung, *not* a
full Streamer.bot-style trigger→action platform (explicitly scoped out).

Motivation: the user wants the bot active in chat (replying) and to stop depending on SE.
Streamer.bot itself is a non-option — it's a Windows/.NET app, macOS-only under Wine
(Whisky, being deprecated), C#-scripted. Talking **directly to Twitch's APIs from Go is
cleaner on a Mac** and drops all the Windows/Wine baggage.

## Complexity verdict (the "how complex is it" answer)

| Stage | Effort | Notes |
|---|---|---|
| Auth + reply | **Small** | One-time OAuth, then Helix `POST /chat/messages`. Std-lib `net/http`. |
| Custom commands + chat-mgmt + timers | **Moderate** | A command engine, variable substitution, `!addcom` CRUD, interval posts. |
| Loyalty-points economy | **Large (the bulk)** | Watch-time accrual + persistence + gamble/redeem/give/leaderboard, money-like correctness. |

Overall: a meaningful, **incremental** build (ship in the order above), **100% clean on
macOS/Go**, and it adds **exactly one dependency** (pure-Go SQLite) — no EventSub/WebSocket
needed at this scope, no Windows/Wine.

## Feasibility (macOS / Go)

- All Twitch surfaces are HTTPS + (optionally) WebSocket — OS-agnostic; nothing Windows-only.
- **Reply:** register a **Confidential** Twitch app (client id/secret, one-time), run the
  **Authorization Code** flow with an `http://localhost` redirect (a tiny one-shot `net/http`
  listener catches `?code=`), store access+refresh token. Send via **Helix
  `POST https://api.twitch.tv/helix/chat/messages`** (scope `user:write:chat`); thread replies
  with `reply_parent_message_id` (msg id comes from the IRC read tags). Refresh on HTTP 401;
  a Confidential app's refresh token doesn't expire → **unattended forever** after one consent.
- **No EventSub** at this scope (we chose watch-time accrual, not sub/cheer bonuses), so **no
  WebSocket dependency**. Reading chat stays on the **existing anonymous IRC** connection.

## Decisions (from grilling)

- Scope = **SE-replacement**: custom commands + timers + **loyalty points** (SE features in use).
- **Dedicated bot account** posts the replies (must be a **mod** in the channel so it can call Get Chatters). Reading stays anonymous IRC.
- Commands are **chat-managed**: `!addcom` / `!editcom` / `!delcom` (mod-only), file/DB-backed.
- Points accrue by **watch time** — poll Helix **Get Chatters** every N min, award present viewers. Zero-dep path (no EventSub).
- Points economy: **balance & leaderboard** (`!points`, `!leaderboard`), **gambling** (`!gamble`/`!slots`), **redeems** (spend points → effects, e.g. a TTS line via our `/say`), **give/tip** (`!give`).
- Storage = **pure-Go SQLite** (`modernc.org/sqlite`, no CGo) for money-like integrity.

## Architecture (extends `bot/`)

Keep the existing anonymous IRC read + TTS routing; add:
- `twitch/` client: OAuth (code flow + refresh), Helix **send message** + **Get Chatters** (std-lib `net/http`).
- `store/` : SQLite — tables `points(user_id, login, balance, watch_secs)`, `commands(name, response, cooldown, min_role, count)`, `timers(name, message, interval, min_lines)`, `redeems(name, cost, effect)`.
- `commands/` engine: dispatch chat → built-in (TTS `!tts`/`!etts`, controls), **custom** (reply via Helix, with variable substitution: `$user`, `$args`/`$1..`, `$count`, `$random`, `$touser`, `$uptime`), **admin** (`!addcom`…), **points** (`!points`/`!gamble`/`!redeem`/`!give`/`!leaderboard`).
- `points/` : watch-time accrual loop (ticker → Get Chatters → credit balances); gamble/redeem/give as atomic SQLite transactions.
- `timers/` : ticker posting interval messages (respect a min-chat-lines gate).

Router grows to: TTS commands → existing `/say`; custom/points/admin commands → reply via Helix.

## Work breakdown (incremental)

1. **Auth + reply**: Twitch app, OAuth localhost consent, token store + refresh, Helix send; prove the bot can reply in chat.
2. **Custom commands + timers**: SQLite store, `!addcom/!editcom/!delcom`, variable substitution, cooldowns/roles, interval timers. Migrate the SE commands we set up (`!commands`, `!voices`, `!ttshelp`, `!discord`, `!socials`, `!schedule`) into the bot.
3. **Loyalty points**: watch-time accrual (Get Chatters), `!points`/`!leaderboard`, `!gamble`, `!give`, then `!redeem` wired to TTS `/say`. Tune rates/odds via config.
4. **Retire SE**: once 1–3 are live and stable, remove the SE custom commands + points; keep SE only for overlays/alerts if still used (out of scope here).

## Open questions / defaults to pin at build time

- Redeem catalog + costs (primary redeem = a TTS line; define others).
- Point accrual rate, active-chat bonus, gamble odds/limits (config).
- Reading chat: keep anon IRC (simplest) vs read as the bot — anon IRC is fine and unchanged.
- Timers authored via chat (`!addtimer`) vs config — default config for v1.
- Dependency stance: bot goes from **zero-dep → one dep** (`modernc.org/sqlite`). Confirm that's acceptable (it is the one place money-like data justifies it).

## Risks / notes

- Bot account must be **modded** for Get Chatters; one-time OAuth consent per account.
- Points are money-like — do balance mutations in SQLite transactions; back up the DB file.
- Not a Streamer.bot clone — no trigger→action engine, OBS control, or external WS API (those are the "full platform" rung, deferred).

## References

- Twitch: [Send/Receive Chat](https://dev.twitch.tv/docs/chat/send-receive-messages/), [Get Chatters](https://dev.twitch.tv/docs/api/reference/#get-chatters), [OAuth code flow](https://dev.twitch.tv/docs/authentication/getting-tokens-oauth/), [refresh](https://dev.twitch.tv/docs/authentication/refresh-tokens/), [scopes](https://dev.twitch.tv/docs/authentication/scopes/), [IRC migration](https://dev.twitch.tv/docs/chat/irc-migration/).
- Streamer.bot model (reference only): [triggers](https://docs.streamer.bot/api/triggers), [sub-actions](https://docs.streamer.bot/api/sub-actions), [WebSocket API](https://docs.streamer.bot/api/websocket), [macOS caveats](https://docs.streamer.bot/get-started/installation/macos).
- Go: `modernc.org/sqlite` (pure-Go); `nicklaw5/helix` optional (we can hand-roll the few REST calls std-lib).

## Progress

### 2026-07-04 — Stage 1 (auth + reply) implemented

Motivated by the WIP `!sfx` command needing to *reply* in chat (the bot was read-only).
Built the authenticated-reply foundation, std-lib `net/http` only (no new deps — SQLite
is deferred to Stages 2–4):

- **`twitch/` package** — OAuth Authorization Code flow (`AuthCodeURL`/`Exchange`/refresh/
  `Validate`), Helix **Send Chat Message** with **401 → refresh → retry**, and a
  0600 JSON token `Store`. Tested against `httptest` (never live Twitch).
- **`cmd/bot-auth`** — one-time consent: prints the URL, catches `?code=` on a localhost
  listener (with `state` CSRF check), exchanges + validates, saves the token. Wired as
  `mise run bot:auth`.
- **Bot** — `parse.go` now surfaces `id`/`user-id`/`room-id` (already on the wire via the
  tags cap); a `Chat` seam (interface, faked in tests) mirrors the `TTS` seam; `main.go`
  builds a `chatSender` when creds + a saved token exist, else nil (bot still runs
  read-only). The `!sfx` branch now **replies** with the sorted sound list (threaded to
  the caller, shared TTS cooldown).
- **Deploy** — `TWITCH_CLIENT_ID`/`_SECRET` flow shell → plist → process like `TTS_TOKEN`;
  `bot.tokens.json` git-ignored.

Account-agnostic: Helix sends as whichever account completes consent (dedicated bot
account recommended; main account works too). Manual prerequisites remain the user's:
register a Confidential Twitch app (redirect `http://localhost:3000`), export the
client id/secret, run `mise run bot:auth` once.

`go build/vet/test ./...` clean. Live reply-in-chat is unverifiable here (needs the
user's Twitch app + browser consent). **Deferred:** Stage 2 (custom commands + timers)
and Stage 3 (loyalty points), each adding `modernc.org/sqlite` on this `Chat`/`Sender`
seam.

### 2026-07-07 — Stage 2 (custom commands + timers) implemented

Shipped in two commits.

- **Store** (`store/` package): the blessed dep `modernc.org/sqlite` (pure-Go, no CGo). A
  `commands(name, response, cooldown, min_role, count)` table with `Add`/`SetResponse`/`Delete`/
  `Get`/`List`/`IncCount`. Stage 3's points/redeems will join the same DB (no migration).
- **Custom commands** (`bot/commands.go` + `bot/substitute.go`): `!addcom`/`!editcom`/`!delcom`
  (mod-only, guarded against shadowing built-ins), custom dispatch with variable substitution
  (`$user`/`$args`/`$1..$9`/`$touser`/`$count`/`$random`), per-command **global** cooldowns (mods
  exempt) and `min_role` gating. Slots into `router.go` after the built-ins (which win). A fresh DB
  is seeded with starter commands (ttshelp/socials/schedule).
- **Dynamic built-ins:** `!commands` (lists stored commands) and `!voices` (via a new **`GET /voices`**
  on the TTS server — `VoiceMap.List()` — keeping the bot decoupled from `voices.toml`).
- **Timers** (`bot/timers.go`): `timers.toml` `[[timer]]` (name/message/interval/min_lines); each posts
  on its interval **only if ≥ min_lines** chat messages arrived since its last post. The bot counts
  lines + caches the broadcaster id from room-id tags, so timers post (plain `Chat.Send`) with no
  triggering message.

Decisions this pass (grilled): SQLite now; `!commands`+`!voices` dynamic (server `/voices`); core
variable set (**`$uptime` deferred** — needs Helix Get Streams); timers **config-defined** for v1
(chat-managed `!addtimer` deferred); command responses are threaded replies, timers plain sends;
per-command cooldowns are global. `go build/vet/test ./...` clean (store + substitute + dispatch +
timer-gate tests); `GET /voices` and the seed/startup path smoke-verified. Live chat needs the user's
Twitch auth. **Next:** Stage 3 — loyalty points (watch-time accrual, `!points`/`!gamble`/`!redeem`/
`!give`/`!leaderboard`) on this same store.

### 2026-07-14 — Stage 3 sub-step A (marks economy core) implemented

Grilling reshaped Stage 3 from the original sketch. The currency is **"marks"**; there is **no manual
`!redeem`** — instead **marks are spent implicitly by `!tts`/`!sfx`** and **earned two ways**: our own
**watch-time accrual** (live-only) *and* viewers **converting Twitch Channel Points**. Auth moves to a
**single broadcaster token** (Channel Points need it). Still **no WebSocket** — all Twitch reads poll.

- **Store** (`store/points.go`): an append-only **`ledger(user_id, delta, reason, ref, ts)`** (balance =
  `SUM(delta)`) + a **`users(user_id, login, display, last_seen)`** identity table. Methods: `Balance`,
  `Spend`/`Transfer` (atomic `BEGIN IMMEDIATE`, can't overdraw), `Credit` (idempotent on `ref` — a unique
  index makes channel-point crediting crash-safe), `UpsertUser`, `ResolveLogin`, `Leaderboard`. Same
  `bot.db` as Stage 2.
- **Twitch client** (`twitch/api.go`): `GetChatters`, `IsLive` (Get Streams — also unblocks `$uptime`),
  and channel-point `EnsureReward`/`GetRedemptions`/`FulfillRedemptions`. Refactored `helix.go` to share
  one `do` (401→refresh→retry) wrapper across send + the new GET/PATCH calls.
- **Economy runner** (`bot/economy.go`): one goroutine, two loops — **accrual** (every `accrual_interval`,
  if live, credit each present viewer `accrual_rate`) and **conversion** (poll the bot-managed "Convert to
  Marks" reward every `poll_interval`; credit `reward_grant`, mark FULFILLED). Any scope/affiliate failure
  logs once and disables that loop; the bot keeps running.
- **Charging** (`bot/router.go`): `!tts`/`!sfx` check affordability → run the effect → **debit on success**
  (failed effect = no charge). **Everyone pays** (mods/broadcaster included); the per-user cooldown stays.
  Broke → a polite refusal. **Read commands** (`bot/commands.go`): `!marks`/`!m [@user]` + `!leaderboard`.
- **Config/wiring**: opt-in **`points.toml`** (`currency_name`, accrual, costs, reward, poll; + game knobs
  for B). `cmd/bot-auth` gains `moderator:read:chatters`, `channel:read:redemptions`,
  `channel:manage:redemptions` — **user must re-run `mise run bot:auth` as the broadcaster**. **Safety
  valve:** if the token lacks those scopes (or points.toml is absent), the economy stays **disabled** and
  `!tts`/`!sfx` remain **free** — the bot never becomes unusable.

`go build/vet/test ./...` clean (store ledger/idempotency/leaderboard, twitch httptest suite, economy
accrual/conversion/disable, charge path + read commands, config defaults). Smoke-verified: bot boots,
creates the ledger/users tables, and correctly logs "economy configured but disabled" for the existing
token (which predates the new scopes). Live accrual/conversion needs the broadcaster re-auth + an
affiliate channel. **Next:** Stage 3 sub-step B — `!gamble` (coinflip) + `!give` on this same store
(`store.Transfer` already in place).

### 2026-07-14 — Stage 3 sub-step B (gamble + give) implemented

- **`bot/games.go`**: `!gamble <amount|all>` — a coinflip double-or-nothing at `gamble_win_chance`
  (default 0.47, house edge), with a `gamble_min_bet` floor (default 10) and `all` support; win nets
  `+bet`, loss forfeits it, reply shows the outcome + new balance. `!give @user <amount>` — resolves the
  recipient in the `users` table (else "haven't seen"), blocks self-gives, and moves marks via the atomic
  `store.Transfer` (can't overdraw). Both are economy built-ins (dispatched only when the economy is
  enabled, guarded against `!addcom` shadowing) and share the standard per-user cooldown (mods exempt).
- Config gained `gamble_win_chance` / `gamble_min_bet` defaults in `LoadEconomyConfig`.

`go build/vet/test ./...` clean (gamble win/lose/all/min-bet/over-balance; give transfer/self-block/
unseen/over-balance; games inert when the economy is off). This completes the SE **chatbot-layer**
replacement (commands + timers + points). **Remaining:** Stage 4 — actually retire the SE custom
commands/points once this has run live for a bit (overlays/alerts stay on SE, out of scope). Deferred
niceties: chat-managed `!addtimer` (+ timers table), `$uptime` (now trivial via `IsLive`), reward tiers.

### 2026-07-14 — Stage 3C (owner grant + free/paid toggle) implemented

Two broadcaster-only economy controls requested by the owner.

- **`!grant @user <amount>`** (`bot/admin.go`): mints marks on a positive amount, removes on a negative
  one **clamped at 0** (never negative), via a new atomic `store.Grant`. Resolves the target
  **local `users` table first, then Helix `Get Users`** (new `twitch.GetUsers`, no new scope) so it works
  for anyone; an unknown login replies "no such user". The resolved name is upserted so a freshly-granted
  user shows on the leaderboard. Independent of the charge mode.
- **`!free` / `!paid`** (`bot/admin.go`): toggle whether `!tts`/`!sfx` charge — modeled on the
  `!pause`/`!clear` queue controls. **Persisted** in a new `settings(key,value)` table
  (`store/settings.go`); restored on startup (default paid). **Free mode waives only `!tts`/`!sfx` cost** —
  accrual keeps running and `!gamble`/`!give`/`!marks` keep using real marks. Implemented as a `charging`
  flag on the Router; `economyActive` gained `&& r.charging`. No lock needed (all in the sequential IRC
  handler).

`go build/vet/test ./...` clean (store Grant mint/clamp + settings round-trip; twitch Get Users;
grant mint/remove/unseen-resolve/unknown/broadcaster-only; free waives tts but games/accrual still charge;
persist + broadcaster-only). Smoke-verified: bot boots "economy enabled (paid)" as the broadcaster and
creates the `settings` table.
