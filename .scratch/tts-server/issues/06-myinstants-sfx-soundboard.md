# myinstants SFX soundboard alongside TTS

Status: done
Type: task
Created: 2026-07-04

## Summary

Add a **sound-effects soundboard** beside the existing TTS: chat commands like
`!airhorn` / `!gonnacome` play a short clip (sourced from **myinstants.com**)
through the **same** serial queue / player / OBS overlay path that `!tts` uses.
Sounds are mapped to commands manually in a readable **TOML** file; one command
can map to **several clips**, and a random one plays each time.

## Motivation

We want quick soundboard reactions in chat without a third-party bot. Reusing the
TTS server's queue/overlay means SFX inherit skip/pause/clear, never overlap TTS,
and play through the OBS Browser Source with no extra capture setup.

## Design decisions (locked with user)

- **Host locally.** Download each mp3 once into `sfx/`; the server plays the local
  file. Immune to the myinstants 403/Cloudflare bot-gate (a plain `WebFetch` 403s;
  a real Chrome session loads fine), undocumented rate limits, per-play latency, and
  the clip being removed/renamed upstream. (Verified: a browser-UA + Referer GET of
  the `/media/sounds/*.mp3` CDN file downloads fine — Chrome is only needed to *find*
  the URL via the play button's `data-url`, which has a random suffix and can't be
  guessed from the page slug.)
- **Per-sound commands** — `!airhorn`, `!gonnacome`, one per TOML entry.
- **One command -> N clips (random).** Single clip (`file`/`url`) or an array
  (`[[sounds.<cmd>.clips]]`); the server picks one at random per invocation
  (mirrors the bot's existing `VoiceResolver`). Randomization lives on the server.
- **Everyone + shared TTS cooldown** — SFX reuse the existing per-user cooldown
  (mods/broadcaster exempt); no separate throttle.
- **BurntSushi/toml** — the module's first third-party dep; lets each TOML entry
  carry both the local `file` and the source `url` (nested table).
- **No transcoding.** VLC and the OBS overlay `<audio>` decode MP3 natively; only the
  `.wav` plumbing (temp filename, `Content-Type`, duration estimate) was generalized
  to carry the real extension.

Full implementation plan: `/Users/rtukpe/.claude/plans/linear-moseying-whistle.md`.
Grew out of the same server/bot as PRD `.scratch/tts-server/PRD.md`.

## HTTP API

- `POST /sfx` — `{"name":"airhorn"}` (or form) -> resolve in the soundboard, pick a
  random clip, enqueue; `202 {id, position}`, `404` unknown, `400` empty name.
  Shares the same bearer-token auth as `/say`.

## Config

- `sfx.toml` (repo root, committed) — read by the server (name->clips) and bot
  (registers a `!<name>` per sound). `[sounds.<cmd>]` with `file`+`url`, or
  `[[sounds.<cmd>.clips]]` for a random set.
- `sfx/` — downloaded mp3s, git-ignored (`sfx/*`, keep `.gitkeep`); `sfx.toml` is the
  reproducible source of truth.
- `mise run sfx:fetch` (`cmd/sfx-fetch`) downloads missing clips from their `url`
  (browser UA + `Referer` to clear the gate), idempotently.

## Acceptance criteria

- [x] `go build ./...` and `go vet ./...` are clean.
- [x] `POST /sfx {name}` enqueues and plays the resolved clip through the queue/player.
- [x] Unknown sound -> 404; missing name -> 400; missing/wrong token -> 401; GET -> 405.
- [x] A multi-clip command plays a random clip each time (drawn from its set).
- [x] `!skip` cancels an in-flight SFX clip (same path as TTS).
- [x] `!airhorn`-style chat commands route to `/sfx` and share the `!tts` cooldown.
- [x] MP3 (44.1 kHz stereo) plays without transcoding; overlay serves `audio/mpeg`.
- [x] `mise run sfx:fetch` downloads a real myinstants clip (Cloudflare cleared).

## Out of scope

- Instant "barge-in" playback (SFX ride the single serial queue and wait behind TTS).
- Hot-reload of `sfx.toml` (edit + restart server/bot; fine for a single-user stream).
- Hotlinking / lazy-cache from myinstants (local download was chosen for resilience).
- Volume/gain normalization across clips.

## Tasks

- [x] `sfxlib` shared TOML loader (single + multi-clip normalization) + BurntSushi dep.
- [x] Server: `QueueItem.Kind/Src`, `EnqueueSFX`, `processSFX`; `sfxBoard`; `/sfx`
      handler; generalize overlay/player `.wav` -> real extension; `-sfx-config` /
      `-sfx-dir` flags + wiring.
- [x] Bot: `-sfx-config` + command set; router SFX branch (shared cooldown);
      `TTS.SFX` interface + client `POST /sfx`; main wiring.
- [x] `cmd/sfx-fetch` downloader; starter `sfx.toml`; `sfx/.gitkeep`; `mise sfx:fetch`;
      gitignore `sfx/*`.
- [x] Tests: `sfxlib` loader; server `/sfx` (enqueue/play, 404, multi-clip random,
      skip); bot router SFX + shared cooldown; integration `!airhorn` -> `/sfx`.
- [x] Build, test, and verify end-to-end.

## Comments

### 2026-07-04 — completed & verified

Implemented across `sfxlib/`, `server/` (`sfx.go`, `queue.go`, `server.go`,
`overlay.go`, `player.go`, `main.go`), `bot/` (`config.go`, `router.go`, `tts.go`,
`main.go`), `cmd/sfx-fetch/`, plus `sfx.toml`, `mise.toml`, `.gitignore`.

- **Build/vet** clean. `sfxlib`, `server`, and the new bot SFX tests pass
  (enqueue->play, 404, multi-clip randomness, skip-cancels, shared cooldown,
  `!airhorn`->`/sfx`).
- **sfx:fetch** cleared Cloudflare — downloaded the real 23 KB 44.1 kHz stereo MP3
  (`im-gonna-come_6HehWm4.mp3`). Confirms a browser-UA GET is enough for the media
  files (Chrome only needed to resolve the `data-url`).
- **Live smoke test** (real server binary, stub interpreter, no audio): board loaded
  (`sfx: 1 sound command(s)`); `POST /sfx {gonnacome}` -> `202 {id:1,position:1}` ->
  worker logged `playing sfx "gonnacome"` -> dropped (no OBS client) -> `done`;
  unknown -> 404, no-auth -> 401, empty name -> 400, GET -> 405.

Note: 4 **pre-existing** bot test failures remain (`voices_test.go`,
`sanitize_test.go`) from the earlier `voices.go`/`sanitize.go` trims (voice list,
repeat cap) — unrelated to this work, left untouched. Not yet committed.
