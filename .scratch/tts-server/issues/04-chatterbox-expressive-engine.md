# Add Chatterbox as an alternative, expressive TTS engine (startup-selectable)

Status: ready-for-human
Type: task
Created: 2026-07-03
Updated: 2026-07-06

## Summary

Add Resemble AI's **Chatterbox** as an alternative TTS engine for **emotional/expressive**
delivery (its `exaggeration` knob). The engine is **selected once at server startup** via
`-engine kokoro|chatterbox` — there is **no second chat command**; the existing `!tts` uses
whichever engine the server was launched with. kokoro stays the default. Additive: the Go
queue, playback, and bot are engine-agnostic (text → WAV → speaker), so the work is a
`Synthesizer` seam plus an HTTP client to the external devnen server.

**Code-first pass is implemented and landed** (see *Implemented* below). What remains is a
manual **feasibility spike** on this Mac (stand up devnen, confirm the leak, tune the unload
cadence, measure latency) — hence `ready-for-human`.

> Design pivot (2026-07-06): dropped the original per-request `!etts` command in favor of a
> startup flag. One command, one cooldown, one code path, bot untouched. See Comments.

## Feasibility findings (macOS)

- **Runs on Apple Silicon, but not fast.** No NVIDIA → CPU (~10–30s/line) or MPS (~2–3× faster, buggy). Merged PR #410 added Mac `map_location`; the key MPS **float32 fix (PR #509) is still unmerged**; **memory leak #218** grows over long streams; #147 is an MPS unsupported-op crash.
- **Chosen path: [devnen/Chatterbox-TTS-Server](https://github.com/devnen/Chatterbox-TTS-Server)** — ready-made server, supports MPS, **already float32-patched**, HTTP API with full params (`voice`, `exaggeration`, `cfg_weight`), downloads ~3 GB weights on first run.
- **Dependency isolation:** Chatterbox pins `torch==2.6.0` / `transformers==5.2.0`, conflicting with kokoro's stack → runs in its **own venv/service** (a manual prerequisite; our Go server only speaks HTTP to it).
- **Unavoidable:** every clip is **Perth-watermarked** (fine here); output **24 kHz** (same as kokoro → no player/overlay changes); MIT license.
- **Complexity:** low on our code (additive). Real effort/risk is operational: devnen setup on the Mac, tuning the preset, handling the memory leak.

## Decisions (from grilling, as implemented)

- **Keep both** engines; **select at startup** via `-engine` (not a second `!etts` command).
- **devnen HTTP server** on **MPS**; Go calls its HTTP API. devnen is a **manual prerequisite**.
- **Default voice** (no reference clip); **fixed dramatic preset** (`exaggeration ~0.7`, `cfg_weight ~0.3`).
- **One shared queue/worker/player** — chatterbox is slow (~10–30s) then plays. Accepted.
- **Memory leak #218** handled in-Go by a **flag-gated `/api/unload` cadence** (`-chatterbox-unload-every`, default off), **not** an OS periodic-restart/watchdog. No `deploy/`/launchd changes.

## Architecture

```
!tts → bot → Go POST /say {text} → queue → Synthesizer (chosen at startup) → WAV → player
                                             ├─ kokoro:     local Python sidecar (stdio)
                                             └─ chatterbox: devnen server (HTTP /tts)
```
One queue, one worker, one player. The active engine is a single `Synthesizer` on the queue;
only the selected engine is constructed at startup (chatterbox mode needs no venv/sidecar;
kokoro mode needs no devnen).

## Implemented (this pass — server only, bot untouched)

- **`server/synth.go`** — new `Synthesizer` interface (`Synthesize` + `Ready`); kokoro's `*Engine` already satisfies it.
- **`server/chatterbox.go`** (new) — `chatterboxClient` implements `Synthesizer`: POSTs `/tts` `{text, voice_mode:"predefined", predefined_voice_id?, exaggeration, cfg_weight, output_format:"wav"}`, streams the WAV to the temp file, cancelable via request context (so `!skip` works). Includes the best-effort `/api/unload` cadence.
- **`server/queue.go`** — holds a single `synth Synthesizer` (swapped from the concrete `*Engine`); `process()`/`Status()` updated. No per-item engine field, no request-shape change.
- **`server/main.go`** — startup switch on `-engine`; builds only the selected engine. Flags: `-engine` (`kokoro`), `-chatterbox-url` (env **`CHATTERBOX_URL`**), `-chatterbox-voice`, `-chatterbox-exaggeration` (0.7), `-chatterbox-cfg` (0.3), `-chatterbox-unload-every` (0).
- **`mise.toml`** — `server:serve:chatterbox` task (reads `CHATTERBOX_URL`).
- **`docs/chatterbox.md`** — run instructions, flags/env table, devnen setup, memory-reset note.
- **Tests** — `server/chatterbox_test.go`: a fake devnen (`httptest`) asserting the `/tts` body/preset, the WAV-written-and-played path through a real Queue+Server, and the unload cadence (fires every N; never when 0). `server/server.go` and all of `bot/` unchanged.

## Remaining (manual, needs the model)

- Stand up devnen (own venv Python 3.11, `torch 2.6`; `config.yaml` `device: mps`; bind `127.0.0.1:8004`; ~3 GB on first run) and confirm its `/tts` + `/api/unload` semantics against the running server.
- Run the **#218 minimal repro** (~20 generations, log `psutil` RSS) on 0.1.7/MPS; if RSS climbs, set `-chatterbox-unload-every` (e.g. 15–25) and confirm devnen reloads after `/api/unload`.
- End-to-end + latency: `CHATTERBOX_URL=http://127.0.0.1:8004 mise run server:serve:chatterbox`; `!tts this is dramatic` plays expressively. If unusably slow, lower `-max-chars` or revisit (MLX-audio).

## Verification

1. `go build ./... && go vet ./... && go test ./...` clean — done (chatterbox client + queue-through + unload-cadence tests pass; existing suites unchanged).
2. Smoke (no model): `-engine chatterbox` without a URL → fatal; against an `httptest` stub, `POST /say` → 202 and the stub receives `/tts` — done.
3. Spike (above) — pending.

## Risks

- Chatterbox slow on Mac, blocks the shared queue — accepted; low request rate limits impact.
- Memory leak #218 — validate the `/api/unload` cadence over a long session; tune `-chatterbox-unload-every`.
- Watermark on every Chatterbox clip — unavoidable.

## Reference

- Docs: `docs/chatterbox.md`. Impl plan: `~/.claude/plans/impl-users-rtukpe-claude-plans-linear-mo-piped-hamming.md`.
- Repo: https://github.com/resemble-ai/chatterbox · Server: https://github.com/devnen/Chatterbox-TTS-Server

## Comments

### 2026-07-06 — design pivot + implementation

- **Dropped the per-request `!etts` command** (and the `/say` `engine` field, per-item `QueueItem.Engine`, separate `-etts-cooldown`, and all bot changes). Replaced with a **startup `-engine` flag** so there's one command, one cooldown, one code path; switching engines is a server restart. This shrank the change to server-only.
- **Added env-driven config:** `-chatterbox-url` defaults from **`CHATTERBOX_URL`** (mirrors `TTS_TOKEN` → `-token`), so the `mise run server:serve:chatterbox` task and the launchd service can enable chatterbox via the environment.
- **Memory leak #218:** implemented as the flag-gated `/api/unload` cadence in `server/chatterbox.go` (`-chatterbox-unload-every`, default off), replacing the earlier "launchd periodic restart" idea — no `deploy/` changes.
