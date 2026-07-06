# Chatterbox expressive engine

The TTS server can synthesize with one of two engines, chosen **once at startup**:

- **kokoro** (default) — the local Python sidecar. Fast, clear, emotionally flat.
- **chatterbox** — Resemble AI's [Chatterbox](https://github.com/resemble-ai/chatterbox)
  via the external [devnen Chatterbox-TTS-Server](https://github.com/devnen/Chatterbox-TTS-Server).
  Slower, but dramatic/expressive (an `exaggeration` knob).

There is no separate chat command — `!tts` uses whichever engine the server was launched
with. To switch engines, restart the server with a different `-engine`.

## Running in chatterbox mode

Via mise (reads `CHATTERBOX_URL` from the environment):

```sh
CHATTERBOX_URL=http://127.0.0.1:8004 mise run server:serve:chatterbox
```

Or directly:

```sh
./bin/tts-server -engine chatterbox -chatterbox-url http://127.0.0.1:8004
```

Chatterbox mode needs **no** Python venv or sidecar (kokoro's `-python`/`-sidecar` are
ignored). It does need a running devnen server (see below).

### Flags

| Flag | Env | Default | Meaning |
|------|-----|---------|---------|
| `-engine` | — | `kokoro` | `kokoro` or `chatterbox` |
| `-chatterbox-url` | `CHATTERBOX_URL` | `""` | devnen base URL (**required** for `-engine chatterbox`) |
| `-chatterbox-voice` | — | `""` | `predefined_voice_id`; empty = the devnen server's default |
| `-chatterbox-exaggeration` | — | `0.7` | drama; higher = more expressive |
| `-chatterbox-cfg` | — | `0.3` | `cfg_weight` |
| `-chatterbox-unload-every` | — | `0` | POST `/api/unload` every N generations to reclaim memory (0 = never) |

Chatterbox is slower than kokoro and slows further with long text — lower `-max-chars`
(e.g. `-max-chars 150`) when running it.

## devnen prerequisite (manual)

The devnen server is a manual one-time setup; the Go server just speaks HTTP to it.

```sh
git clone https://github.com/devnen/Chatterbox-TTS-Server
cd Chatterbox-TTS-Server
python3.11 -m venv .venv && ./.venv/bin/pip install -r requirements.txt
# In config.yaml set:  tts_engine.device: mps      (Apple Silicon GPU)
./.venv/bin/python server.py      # first run downloads ~3 GB; bind it to 127.0.0.1:8004
```

## Memory reset (`-chatterbox-unload-every`)

Chatterbox has an upstream memory leak (chatterbox #218 and related, unfixed as of writing).
Leave `-chatterbox-unload-every` at `0` until you've confirmed RSS climbs during a spike, then
set it (e.g. `15`–`25`): after every N generations the server best-effort POSTs `/api/unload`
to devnen, which reloads the model lazily on the next request. A failed unload is logged, not
fatal. The reset adds a one-time cold reload to the next generation — negligible at this
engine's low request rate.
