# Per-clip SFX volume + trim (start/stop point)

Status: done
Type: task
Created: 2026-07-07

## Summary

Soundboard clips (issue 06) can now set a per-clip **`volume`** (0–100 percent, reduce-only) and
**`start`/`end`** seconds to trim playback to a segment of the file (e.g. play only seconds 12–18
of a long clip). Static per-clip config in `sfx.toml`.

```toml
[sounds.mbappe]
file = "dictator-mbappe-meme.mp3"
volume = 50          # 0-100 percent, reduce-only (default 100)
# start = 12         # trim to seconds [start, end)
# end = 18
```

## Decisions (grilled)

- **Static per-clip config** in `sfx.toml` (not live control or chat args).
- **Volume 0–100 percent, reduce-only** — maps to the overlay's `<audio>.volume`; no Web Audio
  boost (only need to *reduce* loud clips).
- **Both players:** OBS overlay (production) applies exact volume + seek-to-`start` + stop-at-`end`;
  VLC applies the trim (`--start-time`/`--stop-time`), but VLC's CLI has no simple linear volume, so
  **volume is overlay-only** (VLC logs and ignores it).
- **SFX-only** — TTS clips play at 100%, untrimmed.

## Files

`sfxlib/sfxlib.go` (`Clip.Volume/Start/End` + validation); `server/`: `sfx.go` (board keeps
metadata), `queue.go` (`QueueItem` fields + `EnqueueSFX` + a `Playback` descriptor replacing
`Play(ctx,id,path)`), `server.go` (`handleSfx`), `player.go` (`Playback` + VLC flags), `overlay.go`
(SSE `{volume,start,end}` + JS: `audio.volume`, seek on `loadedmetadata`, `timeupdate` stop-at-end).
`sfx.toml` (mbappe `volume = 50` as a live example).

## Verification

`go build/vet/test ./...` green (sfxlib validation + a queue-through test asserting volume/start/end
reach the player). VLC trim timed (6.5 s clip → 2.5 s for `start=1`/`stop=3`); overlay SSE carries
`{"volume":40,"start":1,"end":3}`; overlay JS syntax-checked with `node --check`.

## Follow-ups / deferred

- Volume **boost >100%** (Web Audio `GainNode`) — not needed (reduce-only).
- A **global/master** SFX (or TTS) volume knob — easy add if wanted.
- Applying edits is one command: `mise run reload` (see issue 02).
