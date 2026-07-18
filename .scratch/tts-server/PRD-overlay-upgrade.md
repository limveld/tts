# PRD: Full-screen overlay upgrade (Wordle + gamble panel + depth rating)

Status: done — delivered 2026-07-18 (all 5 stages; see the tracking issue)
Type: prd
Created: 2026-07-18

Tracked in detail at [`issues/09-overlay-upgrade.md`](issues/09-overlay-upgrade.md).

## Problem Statement

The stream's on-screen elements are three disconnected pieces, only one of which is integrated with the
bot/server:

- **Audio TTS overlay** — a tiny audio-only page served inline by the Go server (`server/overlay.go`,
  `const overlayHTML`) over SSE (`play`/`stop`). Integrated, but audio-only.
- **Wordle** (`raw/wordle-chat-overlay.html`) — a standalone OBS overlay that opens its own Twitch chat
  WebSocket and runs the game entirely client-side.
- **Deep-of-Night depth widget** (`raw/rating-widget/…`) — a standalone widget that parses `!don +/-N` from
  chat, maps points → depth (1–5) with local PNGs, and shows an icon + title + progress bar, persisting to
  `localStorage`.

Running three browser sources is fragile and none of the game state is owned by the bot (so nothing
persists, survives a source reload, or ties into the marks economy). The broadcaster wants a **single
full-screen overlay** that hosts the games top-left, keeps playing TTS audio invisibly, adds a **live
gambling panel**, and shrinks the depth rating to just an icon + number.

## Solution

**One full-screen OBS browser source at `/overlay`**, served from an embedded `web/overlay/` directory,
that renders purely from the server's **SSE** stream. All game logic moves into the **bot** (which already
reads chat); the bot computes + persists state and **pushes it to the server**, which broadcasts it over
SSE and **caches the last-known state to replay to any reconnecting overlay**. The overlay becomes a pure,
stateless render target — no Twitch connection of its own.

This folds the two standalone widgets into the same **bot-owned, SSE-pushed** model the marks economy
already uses, and lets Wordle wins tie into marks and a persisted leaderboard.

## User Stories

1. As a broadcaster, I want a single full-screen browser source for all overlay elements, so that OBS setup
   is one source instead of three.
2. As a broadcaster, I want TTS/SFX audio to keep playing through that overlay with no visible UI, so that
   the existing audio path is unchanged.
3. As a viewer, I want to play Wordle in chat with `!wordle` (start) and `!guess <word>`, so that chat can
   collectively solve a shared board.
4. As a viewer, I want the Wordle board rendered top-left with the familiar tiles/keyboard, so that it reads
   at a glance on stream.
5. As a viewer, I want solving the Wordle to award marks and count toward `!wordlewins`, so that winning
   matters and ties into the economy.
6. As a broadcaster, I want the Wordle board + win tally to survive a bot restart and an OBS source reload,
   so that progress isn't lost.
7. As a viewer, I want a gambling panel that shows the pot, player count, and a countdown while a `!g` round
   is open, so that the stakes are visible on screen.
8. As a viewer, I want the gambling panel to flash the winner then fade when the round ends, so that the
   result is clear without clutter.
9. As a broadcaster, I want the depth rating collapsed to just a depth icon + the points number in the
   bottom-right (no progress bar), so that it's compact.
10. As a broadcaster, I want `!don +/-N` (me/mods only) to adjust the depth points, owned and persisted by
    the bot, so that it's consistent and survives restarts (no `localStorage`).
11. As a broadcaster, I want everything to keep working when the overlay source reloads mid-round, so that a
    refresh never leaves a blank/stale overlay.

## Implementation Decisions

- **Serving:** replace the inline `overlayHTML` with a `web/overlay/` directory embedded via `go:embed`,
  served at `/overlay` (+ a static subpath for assets), token-protected via `?token=` as today.
- **One data channel:** the overlay renders only from SSE. Event types on `/overlay/events`: `play`/`stop`
  (audio, unchanged) plus new `gamble`, `depth`, `wordle`.
- **Bot-owned games:** the bot handles `!wordle`/`!guess`/`!wordlewins`, `!g` (existing), and `!don`;
  computes state; persists it in `bot.db`; and POSTs it to a new authed **`POST /overlay/state`
  `{kind, data}`** endpoint. The server broadcasts the matching SSE event **and stores it as the
  last-known payload**, replaying `gamble`/`depth`/`wordle` to each newly-connected overlay (audio is
  transient, not replayed).
- **Layout:** top-left column = Wordle board with the gamble panel stacked below it (only while a round is
  open); bottom-right = `[depth-tier.png] <points>`; invisible `<audio>`. 1920×1080 transparent.
- **Wordle engine (Go):** one shared 6-row board; unlimited guesses per user; dictionary-validated 5-letter
  guesses; scoring with correct duplicate-letter handling; win **or** 6 misses clears the board until
  `!wordle` restarts; solve → configurable marks reward (`store.Grant`) + win-tally++; chat announcements.
  Word lists ported/embedded from the prototype (valid-guess dict) plus a curated answers pool.
- **Depth:** `!don +/-N` (broadcaster/mods) adjusts a persisted points value (clamp ≥ 0), pushed via SSE;
  overlay computes the tier icon from the prototype thresholds (0/1000/2000/4000/6000). No bar, no title.
- **Gamble panel:** `bot/gamble.go` pushes `{phase, buyIn, players, pot, endsAt, winner?}` on
  open/join/resolve/cancel; overlay renders pot + players + countdown, result flash → fade.
- **Push client:** a small fire-and-forget client on the bot (mirrors `TTSClient`, reuses `TTSURL`+
  `TTSToken`); overlay pushes are best-effort and never block chat handling.

## Testing Decisions

- **Server:** httptest that `POST /overlay/state` broadcasts the right SSE event and that a freshly-connected
  client receives the **replayed last-known** `gamble`/`depth`/`wordle` state; embed serving of `/overlay`.
- **Bot:** Wordle scoring as a table test (including duplicate-letter cases), the round lifecycle
  (start/guess/win/6-miss/clear), win-tally + marks reward, `!don` clamp, and the gamble event payloads —
  all against the temp SQLite store and fakes; never live Twitch or a live SSE client.
- **Overlay push client:** asserts the exact JSON payloads against a fake server.
- **Manual/live:** OBS full-screen source at 1920×1080; run `!wordle`+`!guess`, a `!g` gamble, and
  `!don +200`; reload the source mid-round and confirm it re-renders from the cache; audio still plays.

## Out of Scope

- The audio/synthesis pipeline and the VLC player path (untouched).
- Other OBS overlays/alerts (follow/sub/raid, goal bars) — separate browser-source graphics.
- Wordle hard-mode, anti-cheat beyond dictionary validation, multiple simultaneous boards.
- Depth beyond `[icon] <points>` — no progress bar, no tier title, no `localStorage`.

## Further Notes

- **Sequencing** (each independently shippable): (1) full-screen `embed.FS` shell hosting the existing
  audio, (2) `POST /overlay/state` + SSE event types + last-known cache/replay, (3) gamble panel, (4) depth,
  (5) the Wordle engine last (the largest piece).
- Reuses the existing SSE hub (`server/overlay.go`), the `TTSClient` HTTP pattern (`bot/tts.go`), and the
  store's `Grant`/`Spend`/`Leaderboard`/settings helpers — the overlay upgrade adds no new dependency.
- The prototype game code in `raw/` is ported (rendering for Wordle, thresholds/PNGs for depth), not
  rewritten from scratch.
