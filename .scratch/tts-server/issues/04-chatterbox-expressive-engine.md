# Add Chatterbox as a second, expressive TTS engine (`!etts`)

Status: proposal — deferred (implement later)
Type: task
Created: 2026-07-03

## Summary

Add Resemble AI's **Chatterbox** as a second TTS engine for **emotional/expressive** lines
(its `exaggeration` knob), invoked via a separate **`!etts`** command. kokoro stays the fast
default (`!tts`). Additive: the Go server, queue, VLC playback, and bot are engine-agnostic
(text → WAV → speaker), so the work is mostly routing some jobs to a different synthesizer.

Deferred proposal — captured for later; not scheduled. Do the **feasibility spike** (below)
before committing to the integration.

## Feasibility findings (macOS)

- **Runs on Apple Silicon, but not fast.** No NVIDIA → CPU (~10–30s/line) or MPS (~2–3× faster, buggy). Merged PR #410 added Mac `map_location`; the key MPS **float32 fix (PR #509) is still unmerged**; **memory leak #218** grows over long streams; #147 is an MPS unsupported-op crash.
- **Chosen path: [devnen/Chatterbox-TTS-Server](https://github.com/devnen/Chatterbox-TTS-Server)** — ready-made server, supports MPS, **already float32-patched**, HTTP API with full params (`voice`, `exaggeration`, `cfg_weight`), downloads ~3 GB weights on first run.
- **Dependency isolation:** Chatterbox pins `torch==2.6.0` / `transformers==5.2.0`, conflicting with kokoro's stack → runs in its **own venv/service**.
- **Unavoidable:** every clip is **Perth-watermarked** (fine here); output **24 kHz** (same as kokoro); MIT license.
- **Complexity:** low–moderate on our code (additive). Real effort/risk is operational: devnen setup on the Mac, tuning the preset, babysitting the memory leak.

## Decisions (from grilling)

- Emotion-driven; **keep both** engines (kokoro default, Chatterbox for `!etts`).
- Route via a **separate `!etts`** command (configurable name).
- **devnen HTTP server** on **MPS**; Go calls its HTTP API.
- **Default voice** (no reference clip); **fixed dramatic preset** (`exaggeration ~0.7`, `cfg_weight ~0.3`).
- **Everyone eligible + long cooldown** (~180s, configurable).
- **One shared queue/worker/player** — a slow `!etts` blocks the queue (~10–30s) then plays. Accepted.

## Architecture

```
!tts  → bot → Go POST /say {engine:"kokoro"}     → queue → kokoro sidecar (stdio) → WAV → VLC
!etts → bot → Go POST /say {engine:"chatterbox"} → queue → devnen server (HTTP)   → WAV → VLC
```
One queue, one worker, one VLC player; the worker branches on `engine`.

## Work items

1. **devnen Chatterbox server** (new service, not Go): install in its own venv (Python 3.11, torch 2.6); `config.yaml` `device: mps`; bind `127.0.0.1:8090`; confirm the synth endpoint + param names (custom `/tts` + OpenAI-compatible `/v1/audio/speech`). Deploy as a launchd agent + `chatterbox:*` mise tasks (mirror `deploy/service.sh`). Mitigate memory leak #218 via periodic restart.
2. **Go server (`server/`)**: add optional `engine` field to `/say` (default `kokoro`); `QueueItem` carries `Engine`. New `server/chatterbox.go` HTTP client → devnen → temp WAV (reuse existing temp-file/cleanup contract). `queue.go` worker branches: `kokoro` → sidecar `Synthesize`; `chatterbox` → HTTP; then the **existing** `player.Play`. Flags: `-chatterbox-url`, `-chatterbox-exaggeration`, `-chatterbox-cfg`, `-chatterbox-max-chars` (~150).
3. **Bot (`bot/`)**: add `!etts` (`-cmd-etts`) → POST `/say` with `engine=chatterbox`; own cooldown (`-etts-cooldown` ~180s), everyone-eligible, reuse `sanitize`.
4. **Docs/discoverability**: add `!etts` to `docs/voices.md`/README; Twitch panel + SE entry is an optional follow-up (browser task).

## Verification

1. **Spike first:** stand up devnen; `curl` its endpoint with the dramatic preset → WAV → play; **measure real latency + RSS growth over ~20 calls** on this Mac. If unusably slow/leaky, revisit (MLX-audio path, or gate to mods).
2. `go build ./... && go vet ./... && go test ./...` clean; add an engine-routing unit test (`httptest` fake chatterbox, assert the right synth path).
3. End-to-end: kokoro server + devnen server + bot; `!tts hi` (fast) and `!etts this is dramatic` (slow, dramatic); confirm both play and the queue serializes.

## Risks

- `!etts` slow on Mac, blocks the shared queue — accepted; long cooldown limits frequency.
- Memory leak #218 — validate the periodic-restart mitigation over a long session.
- Watermark on every Chatterbox clip — unavoidable.

## Reference

Full design notes: `~/.claude/plans/linear-moseying-whistle.md` (session-scoped copy).
Repo: https://github.com/resemble-ai/chatterbox · Server: https://github.com/devnen/Chatterbox-TTS-Server
