# Native Go bot: make it reply-capable + replace StreamElements

Status: proposal — deferred (exploration)
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
