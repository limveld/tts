# TTS server with queue + VLC auto-play for a Twitch chat command

Status: done
Type: task
Created: 2026-07-02

## Summary

Build a local HTTP TTS server (Go, standard library) that a Twitch chat bot can
POST text to. The server synthesizes speech with the vendored **kokoro** engine,
**auto-plays** the audio through the **VLC CLI**, and serializes everything through
a **queue** with pause / resume / clear / skip controls so overlapping `!tts`
messages never talk over each other.

## Motivation

We want a `!tts <text>` Twitch chat command. When a viewer runs it, the message
should be spoken aloud on the streamer's machine automatically, one at a time, and
the streamer needs to be able to pause the flow, clear a spammy backlog, and skip
the current clip.

## Requirements (from the request)

- Accept text from an HTTP request, synthesize audio, and **auto-play it** (no manual step).
- Play via the **VLC CLI**.
- **Queue** TTS requests so they play sequentially.
- **Clear** the queue.
- **Pause** (and resume) the queue.
- Written in **Go**, using the **standard library** wherever possible.

## Design decisions (locked with user)

- kokoro is Python-only, so Go orchestrates a **persistent Python sidecar** that
  loads the model once and stays warm; Go owns HTTP, the queue, controls, and VLC.
- Extra controls beyond the required: **skip current clip**, a **status endpoint**,
  and **per-request voice override**. Pause only stops *new* items — the
  currently-playing clip finishes.
- Bind **127.0.0.1** by default with **optional token auth** (`-token` / `TTS_TOKEN`).
- Audio: mono / 16-bit / 24 kHz WAV via Python's stdlib `wave` (only python dep is `kokoro`).

Full implementation plan: `/Users/rtukpe/.claude/plans/linear-moseying-whistle.md`.

## HTTP API

- `POST /say` — `{"text","voice?"}` (or form/query) → enqueue; returns `{id, position}`.
- `POST /pause`, `POST /resume`, `POST /clear`, `POST /skip` → mutate queue, return status.
- `GET /status` — queue length, paused, engine-ready, current + pending.
- `GET /healthz` — liveness (no auth).

## Acceptance criteria

- [x] `go build ./...` and `go vet ./...` are clean.
- [x] `POST /say` speaks the text through VLC automatically.
- [x] Rapid `/say` calls play back-to-back without overlapping.
- [x] `/pause` stops new items (current finishes); `/resume` drains the backlog.
- [x] `/clear` empties the pending queue.
- [x] `/skip` stops the current clip and advances.
- [x] Per-request `voice` changes the speaker; optional `-token` enforces auth.

## Out of scope

- The Twitch chat bot itself (chat → `POST /say`). The server just exposes `/say`.

## Tasks

- [x] Set up environment (Go + Python 3.12 via **mise**; venv + `kokoro==0.9.4`).
- [x] Python kokoro sidecar (`pysidecar/tts_sidecar.py`) + `requirements.txt`.
- [x] Go server: `go.mod`, `main.go`, `synth.go`, `queue.go`, `player.go`, `server.go`.
- [x] Build and verify end-to-end (say / queue / pause / resume / clear / skip / voice / auth).

## Comments

### 2026-07-02 — progress

- Environment ready: Go 1.26.4 installed; `.venv` (Python 3.12.13) with kokoro 0.9.4,
  torch 2.12.1, numpy 2.5.0 — `kokoro import OK`. espeak-ng and VLC 3.0.23 already present.
- Written: `pysidecar/tts_sidecar.py` (persistent KPipeline, JSON stdin/stdout protocol,
  stdlib `wave` writer) + `pysidecar/requirements.txt`, `go.mod`, and `synth.go` (sidecar
  Engine with ready handshake, response dispatch, and auto-restart).
- Next: `queue.go`, `player.go`, `server.go`, `main.go`, then end-to-end verification.

### 2026-07-02 — completed & verified

All tooling now via **mise** (go 1.26.4, python 3.12.13); the redundant Homebrew Go was
removed and a repo `mise.toml` pins both. Go server built and `go vet` clean.

End-to-end verification (server on `127.0.0.1:8080`, engine ready in ~2s from cache):

- **say** — `POST /say` → `202 {id,position}`; log `job 1 playing` → `job 1 done` (VLC played and exited).
- **serialize** — two rapid says played back-to-back with no overlap (`job 7 done` before `job 8 playing`).
- **pause / enqueue-while-paused / status / clear / resume** — paused items queued without playing; `/status` showed the pending list; `/clear` returned `cleared:3`; `/resume` restored.
- **skip** — cancels the current job both during synthesis and mid-playback (`job N playback skipped`, VLC killed).
- **voice override** — `voice:"am_adam"` synthesized and played with the alternate speaker.
- **auth** — with `-token`, `/healthz` stays open (200); missing/wrong token → 401; correct `Authorization: Bearer` and `X-TTS-Token` → 200.
- **cleanup** — temp WAVs removed after each job; graceful shutdown on SIGTERM.

Files: `go.mod`, `main.go`, `synth.go`, `queue.go`, `player.go`, `server.go`,
`pysidecar/tts_sidecar.py`, `pysidecar/requirements.txt`, `mise.toml`. Not yet committed
(left for review; repo still has no commits).
