# PRD: Expressive TTS engine (Chatterbox) + native reply-capable bot (StreamElements replacement)

Status: done
Type: prd
Created: 2026-07-04
Updated: 2026-07-17

Covers two grilled-and-deferred features, tracked in detail at:
- [`issues/04-chatterbox-expressive-engine.md`](issues/04-chatterbox-expressive-engine.md) — **wontfix**
  (Chatterbox abandoned; expressive need met by Polly instead)
- [`issues/07-amazon-polly-tts-engine.md`](issues/07-amazon-polly-tts-engine.md) — **done** (the expressive
  engine, as shipped)
- [`issues/05-native-bot-streamelements-replacement.md`](issues/05-native-bot-streamelements-replacement.md) — **done**
- [`issues/08-sfx-volume-and-trim.md`](issues/08-sfx-volume-and-trim.md) — **done** (per-clip SFX volume/trim)

They are documented together here because they share the same TTS server + bot, the same
queue/player/overlay path, and the same test seams; they landed independently.

## Delivered (2026-07-17)

**Both problems are solved and in production — the bot track exactly as designed, the expressive track via a
different engine.**

**Native reply-capable bot (issue 05) — fully delivered; StreamElements retired.** An authenticated,
reply-capable Twitch bot, 100% macOS/Go, one dependency (`modernc.org/sqlite`):
- Chat-managed custom commands (`!addcom`/`!editcom`/`!delcom` with variables, per-command cooldowns +
  min-role) and activity-gated timers.
- A full **"marks" loyalty economy**: watch-time accrual (Helix Get Chatters, live-gated) **plus
  Channel-Point→marks conversion** (a polled, bot-managed reward); `!marks`/`!m` + `!leaderboard`; a
  **multiplayer pot `!g` gamble** (timed buy-in, winner-takes-pot); `!give`; owner **`!grant`** and a
  **`!free`/`!paid`** charge toggle. All balance mutations are atomic over an append-only ledger.
- Informational `!uptime` / `!followage` (migrated from SE's defaults); a "slow down" cooldown notice.
- **SE cutover complete:** audit found SE's custom commands off, no timers, and the SE bot already muted →
  chatbot retired. SE overlays/alerts stay (out of scope).

**Intentional divergences from the original plan** (decided during the build, see issue 05 progress log):
- **Auth is the broadcaster's own account**, not a separate bot account — required to read Channel-Point
  redemptions (and it covers Get Chatters as the broadcaster).
- **No manual `!redeem`.** Marks are *spent implicitly* on `!tts`/`!sfx` (per-use cost, owner-toggleable
  free/paid) and *earned* via accrual **+ Channel-Point conversion** — the conversion path is new vs. the plan.
- **`!gamble` → `!g`** is a **multiplayer pot** (2+ players, timed, winner takes all), not a solo coinflip.
- Engine choice is by **server-side voice codes** (`!ttsk` … → kokoro/Polly per `voices.toml`), not a
  separate `!etts` command.
- Added beyond the plan: `!grant`, `!free`/`!paid`, `!uptime`, `!followage`, per-clip SFX volume/trim.

**Expressive engine — shipped as Amazon Polly (issue 07), not Chatterbox (issue 04 → wontfix).** Chatterbox
was integrated but its speech quality on Apple Silicon was poor (MPS crash / CPU-only tradeoffs), so it was
abandoned. The "dramatic/better voices" need is instead met by **Amazon Polly running concurrently with
kokoro**, selected per message via the voice codes above. The `!etts` command and the Chatterbox-specific
decisions below did **not** ship; the `Synthesizer` seam and per-request engine routing they describe **did**
(carrying Polly instead).

The original plan below is retained as the historical design record.

## Problem Statement

- **Flat delivery.** The kokoro voices are clear but emotionally flat. As a streamer I want
  *dramatic, expressive* TTS for punchline/hype moments — something kokoro fundamentally
  can't do — without losing the fast, everyday TTS I already have.
- **Passive bot + StreamElements dependency.** My bot only listens; it can't talk in chat, so
  I still rely on StreamElements for custom commands, timed announcements, and a loyalty-points
  economy. I want the bot to reply and own all of that natively on my Mac (in Go), so I can
  drop StreamElements' chatbot. Streamer.bot isn't an option (Windows/.NET, C# only).

## Solution

- **Chatterbox as an opt-in expressive engine.** Keep kokoro as the fast default (`!tts`); add
  Chatterbox behind a separate `!etts` command tuned to a fixed "dramatic" setting. The heavy
  Chatterbox synthesis runs as an external HTTP server (devnen), so it can even live on another
  machine; its audio plays through the same queue/overlay as everything else.
- **A reply-capable, self-hosted bot.** Authenticate the bot as a dedicated account so it can
  post to chat, add chat-managed custom commands, timed announcements, and a watch-time
  loyalty-points economy (balances, leaderboard, gambling, redeems, peer transfers) — then
  retire StreamElements' chatbot. `!redeem` spends points on effects wired into the existing
  TTS pipeline.

## User Stories

### Expressive engine (Chatterbox / `!etts`)

1. As a viewer, I want `!etts <message>` to speak my message with exaggerated emotion, so that hype/dramatic moments land harder.
2. As a viewer, I want plain `!tts` to stay fast (kokoro), so that ordinary TTS isn't slowed down by the heavy engine.
3. As a viewer, I want `!etts` to work with the same sanitization (URLs/emotes/length) as `!tts`, so that it behaves predictably.
4. As a broadcaster, I want the "drama" level tuned once (a fixed exaggeration/pace preset), so that chat gets a consistent expressive voice without extra syntax.
5. As a broadcaster, I want `!etts` to run alongside kokoro rather than replace it, so that I keep fast TTS for the common case.
6. As a broadcaster, I want `!etts` gated by a longer per-user cooldown than `!tts`, so that the slow/heavy engine isn't spammed.
7. As a broadcaster, I want `!etts` everyone-eligible by default but configurable, so that I can tighten access if needed.
8. As a broadcaster, I want a shorter max length on `!etts`, so that a slow clip can't run absurdly long.
9. As a broadcaster, I want a slow `!etts` line to queue behind other clips and play in order, so that playback never overlaps.
10. As a broadcaster, I want to host the Chatterbox engine on a separate/beefier machine, so that its weight doesn't burden my streaming Mac.
11. As a moderator, I want `!skip` to cut an in-progress `!etts` clip, so that a bad/long dramatic line can be stopped.
12. As a broadcaster, I want `!etts` audio to play through the same OBS overlay as `!tts`, so that capture/setup is identical.
13. As a broadcaster, I want the engine chosen per request (kokoro vs Chatterbox), so that both coexist behind one server/queue.

### Native reply-capable bot (StreamElements replacement)

14. As a broadcaster, I want the bot to reply in chat as a dedicated bot account, so that responses look like a bot, not me.
15. As a broadcaster, I want to authorize the bot once (a single OAuth consent) and have it stay logged in, so that I don't re-auth every session.
16. As a moderator, I want to add a custom command from chat with `!addcom !name <response>`, so that I can create commands live mid-stream.
17. As a moderator, I want to edit a command with `!editcom`, so that I can fix a response without downtime.
18. As a moderator, I want to remove a command with `!delcom`, so that stale commands can be cleaned up.
19. As a viewer, I want `!<command>` to get the bot's reply, so that I can pull up info (Discord, socials, schedule, voice codes, etc.).
20. As a broadcaster, I want custom-command responses to support variables (e.g. the caller's name, arguments, a counter, a random pick, uptime), so that commands feel dynamic.
21. As a broadcaster, I want per-command cooldowns and a minimum role (everyone/sub/mod), so that commands can't be spammed or misused.
22. As a broadcaster, I want timed/interval announcements posted to chat, so that recurring reminders run automatically.
23. As a broadcaster, I want timers gated by a minimum number of chat lines since the last post, so that announcements don't fire into a dead chat.
24. As a viewer, I want to earn loyalty points by watching the stream, so that being present is rewarded.
25. As a viewer, I want `!points` to show my balance, so that I know what I have.
26. As a viewer, I want `!leaderboard` to show the top holders, so that there's friendly competition.
27. As a viewer, I want `!gamble <amount>` to bet points against the bot, so that I can risk/grow my balance.
28. As a viewer, I want `!redeem` to spend points on an effect (e.g. a TTS line), so that points feel valuable and tie into the stream.
29. As a viewer, I want `!give <user> <amount>` to transfer points to someone, so that I can tip or reward others.
30. As a broadcaster, I want point balances to survive restarts and be crash-safe, so that a gamble or transfer can't corrupt/lose points.
31. As a broadcaster, I want the bot to be a moderator so it can read the chatter list for watch-time accrual, so that lurkers earn points too.
32. As a broadcaster, I want to migrate the commands I already set up in StreamElements into the bot, so that nothing is lost in the switch.
33. As a broadcaster, I want to fully retire StreamElements' chatbot once commands/timers/points are live, so that I stop depending on a third party.
34. As a broadcaster, I want everything to run cleanly on macOS in Go with no Windows dependency, so that Streamer.bot's platform lock-in is irrelevant.
35. As a broadcaster, I want the bot to authenticate to a token-protected TTS server, so that reply and TTS both work when the server requires auth.
36. As a bot operator, I want rejections (cooldown/blocked/ineligible) handled cleanly, so that abuse and spam don't reach TTS or chat.

## Implementation Decisions

### Expressive engine

- The TTS server selects a synthesis backend **per request** via an `engine` field on the
  `/say` request (default `kokoro`); the queue item carries the engine and the worker routes
  to the chosen synthesizer, then hands the resulting clip to the existing player/overlay.
- Introduce a **`Synthesizer` seam** — a small interface implemented by the existing kokoro
  sidecar and by a new Chatterbox client — mirroring the recently added `Player` interface.
  This is the single seam the feature is built and tested at.
- Chatterbox runs as the **external devnen HTTP server** (Apple-Silicon MPS, float32-patched);
  the Go server calls it over HTTP with a **fixed dramatic preset** (exaggeration ~0.7,
  cfg_weight ~0.3) and Chatterbox's **default voice** (no reference clip). Output is 24 kHz
  (matches kokoro); every clip is Perth-watermarked (unavoidable); it runs in its own venv/
  service due to a hard `torch` pin conflict with kokoro.
- The bot adds an **`!etts`** command that POSTs `/say` with `engine=chatterbox`, everyone-
  eligible, a **longer cooldown (~180s)** and a **tighter max-chars**, reusing existing
  sanitization. One shared queue/worker/player: a slow `!etts` blocks the queue (accepted;
  the long cooldown limits frequency).
- Because playback is engine-agnostic, `!etts` audio plays through the **same OBS Browser
  Source overlay / VLC** path already shipped, and remote hosting is supported unchanged.

### Native bot

- **Sending:** authenticate a **dedicated bot account** and send via the Twitch **Helix Send
  Chat Message** endpoint (scope `user:write:chat`). Auth is a one-time **Authorization Code**
  flow with an `http://localhost` redirect; register the app as **Confidential** so the refresh
  token persists indefinitely; refresh on HTTP 401. **Reading** chat stays on the existing
  anonymous IRC connection (no EventSub at this scope). Expose a **`Sender` seam** (interface)
  for posting to chat — real Helix implementation, fake in tests.
- **Custom commands:** chat-managed CRUD (`!addcom`/`!editcom`/`!delcom`, mod/broadcaster
  only), stored in the datastore. Responses support **variable substitution** (caller name,
  arguments, a per-command counter, a random pick, uptime). Each command carries a **cooldown**
  and a **minimum role** (everyone/sub/mod).
- **Timers:** interval-posted messages with a **minimum-chat-lines** gate so they don't fire
  into a silent chat.
- **Loyalty points:** watch-time accrual via **Helix Get Chatters** polling (the bot account
  must be a moderator to call it); per-user persisted balances. Commands: `!points`,
  `!leaderboard`, `!gamble`, `!redeem` (spends into the TTS `/say` pipeline), `!give`. All
  balance mutations are **atomic transactions**.
- **Persistence:** **pure-Go SQLite** (`modernc.org/sqlite`, no CGo) — the single new
  dependency for the otherwise std-lib bot — with tables for points, commands, timers, and
  redeems. This is the money-like store's correctness boundary.
- **Router:** the existing chat router grows to dispatch TTS (`!tts`/`!etts`), controls,
  custom commands, points commands, and admin (`!addcom`…), reusing the existing sanitizer.
- **Scope of Twitch integration:** no EventSub (sub/cheer/raid bonuses are out); no external
  WebSocket/HTTP control API; no trigger→action engine. Everything is HTTPS + IRC, clean on
  macOS/Go.

## Testing Decisions

- **Test external behavior at the highest seam, not internals.** Drive the public boundary and
  assert observable effects (HTTP calls made, chat messages sent, balances changed) — never
  reach into private state.
- **Expressive engine:** test at the **`/say` HTTP boundary** through the **`Synthesizer`
  seam** — drive `/say` with `engine=chatterbox` against a **fake devnen server** (`httptest`)
  and a fake `Player`, asserting the Chatterbox path is taken with the right params, and that
  `engine=kokoro`/default still routes to the sidecar synthesizer. Prior art: the server's
  `Player`/`Engine` interfaces and the bot's `httptest`-based integration test.
- **Native bot:** test at **`Router.Handle`** with a **fake `Sender`** (assert the exact chat
  replies) and a **temporary SQLite store** (assert command CRUD and points balances after
  `!gamble`/`!give`/`!redeem`). Feed canned IRC lines through parse → router as the existing
  tests do. Prior art: `bot/router_test.go` (fakeTTS) and `bot/integration_test.go` (raw IRC →
  `httptest`).
- **Twitch is always faked in tests.** OAuth, Helix send, and Get Chatters sit behind
  interfaces so tests use fakes; do not test against live Twitch.
- Points math (win/lose odds, transfer conservation, insufficient-balance rejection) is
  covered as pure/store-level unit tests with a temp DB.

## Out of Scope

- Full **Streamer.bot-style trigger→action platform**, OBS control, and an external
  WebSocket/HTTP API for overlays/tools (the "platform" rung).
- **On-screen alerts/overlays and StreamElements overlay graphics** (follow/sub alerts, chat
  box, goal bars) — those are browser-source overlays, a separate and larger effort than the
  chatbot layer.
- **EventSub event reactions** (bonus points on subs/cheers/raids, alert triggers) — deferred;
  would add a WebSocket dependency and extra scopes.
- Chatterbox **streaming/Turbo**, **reference-clip voice cloning**, per-message intensity
  levels, and the **MLX-audio** path.
- Migrating StreamElements' non-chatbot features (song requests, giveaways).

## Further Notes

- Both features **compose with the already-shipped OBS Browser Source overlay**: any engine's
  audio plays through it, and remote hosting is where offloading Chatterbox's synthesis pays
  off.
- **Sequencing:** the Chatterbox engine is small and additive (one `Synthesizer` seam + a
  command) and can land independently of — and before — the larger native-bot work.
- macOS/Go feasibility is confirmed for both (HTTPS/IRC only); the heaviest risks are
  operational (Chatterbox latency/memory on Apple Silicon; the loyalty-points economy's
  money-like correctness).
- Detailed, already-grilled decisions live in the two linked issues (04, 05).
