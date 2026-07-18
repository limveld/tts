# Full-screen overlay: merge audio + Wordle + gamble panel + depth rating

Status: done — all 5 stages shipped (2026-07-18)
Type: task
Created: 2026-07-18

PRD: [`../PRD-overlay-upgrade.md`](../PRD-overlay-upgrade.md)

## Summary

Collapse the three separate overlay pieces — the audio-only TTS overlay (`server/overlay.go`), the
standalone Wordle chat overlay (`raw/wordle-chat-overlay.html`), and the Deep-of-Night depth widget
(`raw/rating-widget/…`) — into **one full-screen OBS browser source at `/overlay`**. The overlay becomes a
**render-only** target driven entirely by the server's SSE; all game logic moves into the **bot** and is
**pushed to the server**, which broadcasts over SSE and **caches last-known state** so a source reload
re-renders immediately. Adds a live **gamble panel** (pot/players/countdown) and shrinks depth to an icon +
points number. Wordle wins tie into the **marks** economy.

## Decisions (from grilling)

- **Serving:** one full-screen page at `/overlay`, from an embedded `web/overlay/` dir (`go:embed`),
  token-protected via `?token=`. Replaces the inline `overlayHTML`.
- **Data flow:** single channel = server SSE. Overlay drops its own Twitch WS/`?channel=`. Events:
  `play`/`stop` (audio, unchanged) + new `gamble` / `depth` / `wordle`.
- **Ownership:** all games bot-owned. Bot computes + persists state and POSTs to `POST /overlay/state`;
  server broadcasts + caches last-known and replays `gamble`/`depth`/`wordle` on connect (audio not replayed).
- **Layout:** top-left column = Wordle board, gamble panel stacked below it (only during a round);
  bottom-right = `[depth-tier.png] <points>`; invisible `<audio>`; 1920×1080 transparent.
- **Gamble panel:** pot + player count + live countdown; result (winner/cancelled) flash → fade.
- **Depth:** bot-owned `!don +/-N` (broadcaster/mods), persisted in the store (no `localStorage`), pushed via
  SSE; overlay shows `[icon] <points>`, tier from the prototype thresholds; **no progress bar / no title**.
- **Wordle:** bot-owned Go engine, `!wordle` (start when idle) + `!guess <word>`; one shared 6-row board;
  unlimited guesses/user; win or 6 misses clears the board until `!wordle` restarts; renders the prototype's
  tiles/keyboard from SSE, scaled to fit.
- **Wordle wins:** solver gets a configurable marks reward (`store.Grant`) + a persisted win tally →
  `!wordlewins` leaderboard; bot announces start/solve/loss in chat.

## Architecture

```
Twitch chat ──IRC──> bot (existing reader)
  !wordle/!guess/!g/!don ─> bot game engines (Go) ──persist──> SQLite (bot.db)
                                   │ push state (HTTP POST, bearer token)
                                   ▼
                   server  POST /overlay/state {kind, data}
                                   │ broadcast + cache last-known
                                   ▼  SSE  (play/stop | gamble | depth | wordle)
                   OBS Browser Source  /overlay  (full-screen, render-only)
```

## Work breakdown (each independently shippable)

1. **Shell:** `//go:embed web/overlay/*`; serve a full-screen `/overlay` page and move the existing audio
   `play`/`stop` logic into it unchanged. (`server/overlay.go`, new `server/web/overlay/index.html`.)
2. **Transport:** authed `POST /overlay/state {kind, data}` → SSE `event:` broadcast; last-known cache per
   kind + replay on new SSE connect; a fire-and-forget bot push client (mirrors `TTSClient` in `bot/tts.go`,
   reuses `TTSURL`+`TTSToken`). (`server/overlay.go`, `server/server.go`, new `bot/overlay.go`.)
3. **Gamble panel:** emit `{phase, buyIn, players, pot, endsAt, winner?}` from `bot/gamble.go` on
   open/join/resolve/cancel; overlay renders pot/players/countdown + result flash.
4. **Depth:** `!don +/-N` (broadcaster/mods) → persisted points (clamp ≥ 0) → push; overlay bottom-right
   `[icon] <points>`; push current value on bot startup. (`bot/commands.go`/`bot/depth.go`, `store/`.)
5. **Wordle (largest, last):** Go engine — embedded word lists, duplicate-letter scoring, round lifecycle,
   marks reward + win tally, `!wordle`/`!guess`/`!wordlewins`, chat announcements, board+tally persistence,
   push on change; overlay renders the ported board. (`bot/wordle.go`, `store/`, overlay render.)

## Persistence (SQLite, same `bot.db`, reuse `store/`)

- Depth points — a `settings` key (or small row).
- Wordle current-round state — a single JSON row; `wordle_wins(user_id, login, display, wins)` leaderboard.

## Config

- `wordle_reward` (marks on solve) and an optional `!guess` anti-spam cooldown in `points.toml` (or a new
  `overlay.toml`). Depth thresholds/icons stay static in the overlay page.

## Tests

- **Server:** `POST /overlay/state` → correct SSE event; **replay of last-known** `gamble`/`depth`/`wordle`
  to a freshly-connected client; embed serving of `/overlay` (httptest).
- **Bot:** Wordle scoring table incl. duplicate letters; round lifecycle (start/guess/win/6-miss/clear);
  win-tally + marks reward; `!don` clamp; gamble event payloads — temp store + fakes, no live Twitch/SSE.
  Push client asserts payloads vs a fake server.
- **Manual/live:** OBS full-screen source @1920×1080; `!wordle`+`!guess`, a `!g` gamble, `!don +200`; reload
  the source mid-round → re-renders from cache; audio still plays.

## Out of scope

- Audio/synthesis pipeline + VLC path; other OBS overlays/alerts; Wordle hard-mode / anti-cheat beyond dict
  validation / multi-board; depth beyond `[icon] <points>` (no bar, no title, no `localStorage`).

## Progress log

- **Stage 1 — shell (done):** replaced the inline `overlayHTML` with an embedded
  `server/web/overlay/` dir (`//go:embed`), served full-screen at `/overlay`; audio
  play/stop ported unchanged into `overlay.js`. httptest covers page + asset serving
  and the `?token=` gate.
- **Stage 2 — transport (done):** authed `POST /overlay/state {kind,data}` broadcasts
  an SSE event and caches last-known per kind; new SSE clients get `gamble/depth/wordle`
  replayed (audio not). `bot/overlay.go` `OverlayClient` pushes via `TTSURL`+`TTSToken`,
  serialized (ordered) + non-blocking. Tests: broadcast, replay-on-connect, unknown-kind
  400, push-client payload + ordering.
- **Stage 3 — gamble panel (done):** `gamble.go` emits `{phase,buyIn,players,pot,endsAt,
  winner,cancelled}` on open/join/resolve/cancel; overlay renders pot/players/countdown +
  result flash→fade, with a delayed hidden push to clear stale state.
- **Stage 4 — depth (done):** `!don +N/-N/N` (broadcaster+mods) adjusts a persisted,
  clamped (0..10000) points value; pushes `{points,tier}`; overlay renders
  `[depth-tier.png] <points>` bottom-right. Value pushed on startup. 5 PNGs embedded.
- **Stage 5 — Wordle (done):** Go engine — `!wordle`/`!guess`/`!wordlewins`, shared 6-row
  board, dup-letter scoring, marks reward + persisted win tally, JSON round persistence
  (survives restart), chat announcements; overlay renders ported tiles + keyboard. Word
  lists embedded (500 answers / 4883 valid). `wordle_reward` in points.toml.

Verified end-to-end against a live server: `/overlay` + assets + depth PNG serve,
`POST /overlay/state` caches + replays to a fresh SSE client, auth 401 / unknown-kind 400.
Full `go build/vet/test ./...` green; `go test -race ./bot ./server` clean.

Follow-ups (not blocking): self-host the Cinzel/JetBrains Mono fonts (currently Google
Fonts CDN, falls back to system offline); optional `!guess` anti-spam cooldown.

## References

- Current overlay + SSE hub: `server/overlay.go` (`broadcast`/`clients`/`Play`/`Done`, `overlayHTML`).
- Bot HTTP client pattern: `bot/tts.go` (`TTSClient`). Gamble round timers: `bot/gamble.go`.
- Store helpers to reuse: `store.Grant`/`Spend`/`Leaderboard`/settings (`store/points.go`, `store/settings.go`).
- Prototypes to port: `raw/wordle-chat-overlay.html` (board/keyboard render + word list),
  `raw/rating-widget/deep-of-night-depth-widget.html` (thresholds) + `raw/rating-widget/images/depth-{1..5}.png`.
