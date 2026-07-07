# Amazon Polly as a cloud TTS engine (voice codes across kokoro + Polly)

Status: done
Type: task
Created: 2026-07-06
Updated: 2026-07-07

## Summary

Add **Amazon Polly** (cloud TTS, natural Neural/Generative voices) as an engine alongside the
local kokoro sidecar, and drive voice selection with the existing `!tts<code>` chat codes across
**both** engines at once. This is the "better quality" rung that **replaces the removed Chatterbox
engine** (issue 04, now `wontfix`).

## Motivation

kokoro is fast/free but robotic; Chatterbox (issue 04) shipped but sounded poor and was removed.
Polly's Neural voices are far more natural, it's a cloud API (no local model/venv/submodule), and
per-message cost is negligible (Neural $16/1M chars ≈ $0.003/msg; free tier 1M chars/mo for 12 mo).

## What shipped (arc)

1. **Polly engine** behind a `Synthesizer` seam (`aws-sdk-go-v2`; MP3 @ 24 kHz), startup-selectable
   via `-engine polly`. Chatterbox kept temporarily.
2. **Removed Chatterbox** entirely — submodule, `deploy/chatterbox-*`, service co-management, tests
   (see issue 04).
3. **AWS IAM setup** (via Chrome): dedicated user `tts-polly-bot`, `AmazonPollyReadOnlyAccess`,
   access key written to `~/.aws` (never in the repo/plist).
4. **Concurrent engines + server-side voice map:** kokoro (always) + Polly (optional) run side by
   side; `voices.toml` per-engine `[kokoro]`/`[polly]` sections map each `!tts<code>` to an
   `(engine, voice)`. The bot forwards the **raw code** (voice-agnostic; `bot/voices.go` deleted).
   Bare/unknown codes → **weighted random** over `weight > 0` entries (`weight = 0` = explicit-only,
   keeping paid Polly voices off the free random pool). Per-voice Polly tier override; **graceful
   degradation** when Polly creds are missing (logs `polly DISABLED`, kokoro keeps serving).
5. **Kevin** pinned to the `neural` tier (it's neural-only under the section's `standard` default).

## Key decisions (grilled)

- MP3 @ 24 kHz (native) via an `Ext()` addition to the `Synthesizer` seam (kokoro → `.wav`, polly → `.mp3`).
- `aws-sdk-go-v2`, not hand-rolled SigV4; creds/region via the AWS default chain (`~/.aws`), no plist secrets.
- Concurrent engines with per-request routing — reinstated the `synths` map + `QueueItem.Engine`;
  kokoro always available, Polly optional (enabled by a `[polly]` section + resolvable creds).
- Server-side `voices.toml` (per-engine sections, merged, codes unique across sections); weighted
  random; optional per-voice tier override for generative voices.
- Engine/voice config **collapsed into `voices.toml`** — dropped `-engine`/`TTS_ENGINE`, `-voice`,
  and the `-polly-*` flags and their plist/mise plumbing.

## Files

`server/`: `synth.go` (`Synthesizer` + `Ext()`), `polly.go`, `voices.go` (loader + weighted
resolver), `queue.go` (`synths` map + `QueueItem.Engine`), `server.go` (`handleSay` resolves the
`code`), `main.go` (build kokoro always + Polly optional). `bot/`: `router.go`/`tts.go`/`main.go`
send the raw code; `bot/voices.go` deleted. `voices.toml`; `docs/polly.md`, `docs/voices.md`.

## Verification

`go build/vet/test ./...` green. Live: `!ttsk`→Kevin, `!ttsr`→Brian (Polly), `!ttsa`→af_nicole
(kokoro), bare `!tts`→weighted random — one server, both engines; graceful Polly-disabled fallback;
Kevin neural fix confirmed (no more `EngineNotSupportedException`).

## Follow-ups / deferred

- Generative-tier voices (the per-voice `engine` override supports it; none configured yet).
- A global/master TTS volume (not built).
- Per-message Polly voice availability varies by region/tier — validated on the first real call.
