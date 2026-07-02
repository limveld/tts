# Twitch chat bot (drives the TTS server)

Status: done
Type: task
Created: 2026-07-02

## Summary

A standalone Go service (`bot/`) that reads Twitch chat (anonymous, read-only) and drives
the TTS server: `!tts <message>` speaks; mod-only `!skip`/`!pause`/`!resume`/`!clear`
control the queue. Built as part of the monorepo restructure (`server/`, `bot/`, `kokoro/`
siblings with shared root tooling).

## Decisions (from grilling)

- Separate **Go** bot, **anonymous** IRC over **TLS** using the standard library (no deps).
- `!tts` speaks text only; controls are standalone **mod-only** commands (names configurable).
- **Everyone** eligible (role-configurable via `-min-role`) + **30s per-user cooldown** (mods exempt).
- Sanitize: strip URLs, strip emotes (IRC `emotes` tag), collapse repeats, length cap (~200), word blocklist.
- Speak just the message (no attribution).
- **Random voice by default**; per-voice override via **short codes** `!ttsa`,`!ttsb`,… (configurable `code→voice` map, unknown → random). Table for pinning in `docs/voices.md`.
- All rejections are silent + logged (anon can't reply).

## Files

- `bot/`: `main.go`, `config.go`, `irc.go` (TLS IRC), `parse.go` (IRCv3), `router.go`,
  `sanitize.go`, `voices.go`, `cooldown.go`, `tts.go` + tests.
- `docs/voices.md` — pinnable code→voice reference.
- `mise.toml`: `bot:build`, `bot:test`, `bot:serve`, `bot:service:*`.
- `deploy/com.rtukpe.tts-bot.plist.template` + generalized `deploy/service.sh <server|bot>`.

## Run

```
mise run server:serve                          # TTS server (terminal 1)
TTS_CHANNEL=<your_channel> mise run bot:serve   # bot (terminal 2)
# or install always-on:  TTS_CHANNEL=<you> mise run bot:service:install
```

## Verification

- [x] `go build ./...` + `go vet ./...` clean; `go test ./bot/...` = 19 passing (parse, sanitize, voices, cooldown, router, and a raw-IRC→httptest integration test).
- [x] Live TLS IRC connect: `connected as justinfan…, joined #twitch`.
- [x] Bot plist renders + `plutil -lint` OK; `bot:service:install` refuses without `TTS_CHANNEL`.

## Comments

### 2026-07-02 — done

Monorepo restructure verified (server still plays end-to-end from `server/`). Bot logic
covered by 19 tests incl. the full pipeline via `httptest`; transport confirmed against a
live anonymous Twitch connection. Not committed (left for review). Live in-chat smoke
(typing `!tts` in your own channel) is the streamer's step.
