# Chatterbox expressive engine

The TTS server can synthesize with one of two engines, chosen **once at startup**:

- **kokoro** (default) — the local Python sidecar. Fast, clear, emotionally flat.
- **chatterbox** — Resemble AI's [Chatterbox](https://github.com/resemble-ai/chatterbox)
  via the [devnen Chatterbox-TTS-Server](https://github.com/devnen/Chatterbox-TTS-Server),
  **vendored as the `chatterbox-server` git submodule**. Slower, but dramatic/expressive
  (an `exaggeration` knob).

There is no separate chat command — `!tts` uses whichever engine the server was launched
with. To switch engines, restart the server with a different `-engine` (or `TTS_ENGINE`).

## Quick start (as a service)

```sh
mise run chatterbox:setup                            # one-time: build the Py3.10 venv + deps (heavy)
TTS_ENGINE=chatterbox mise run server:service:install # installs BOTH agents; devnen starts with the server
```

`TTS_ENGINE=chatterbox` is baked into the server's launchd plist, and `service.sh` co-starts
the `com.rtukpe.chatterbox` agent alongside `com.rtukpe.tts-server` (and co-stops it on
`server:service:stop`/`uninstall`). `CHATTERBOX_URL` defaults to `http://127.0.0.1:8004`.

## Running in chatterbox mode (dev / foreground)

```sh
mise run chatterbox:serve   # terminal 1: the devnen server on 127.0.0.1:8004
# terminal 2:
CHATTERBOX_URL=http://127.0.0.1:8004 mise run server:serve:chatterbox
# or directly:
./bin/tts-server -engine chatterbox -chatterbox-url http://127.0.0.1:8004
```

Chatterbox mode needs **no** kokoro venv or sidecar (kokoro's `-python`/`-sidecar` are ignored).

### Flags

| Flag | Env | Default | Meaning |
|------|-----|---------|---------|
| `-engine` | `TTS_ENGINE` | `kokoro` | `kokoro` or `chatterbox` |
| `-chatterbox-url` | `CHATTERBOX_URL` | `""` | devnen base URL (**required** for `-engine chatterbox`) |
| `-chatterbox-voice` | — | `""` | `predefined_voice_id`; empty = the devnen server's default |
| `-chatterbox-exaggeration` | — | `0.7` | drama; higher = more expressive |
| `-chatterbox-cfg` | — | `0.3` | `cfg_weight` |
| `-chatterbox-unload-every` | — | `0` | POST `/api/unload` every N generations to reclaim memory (0 = never) |

Chatterbox is slower than kokoro and slows further with long text — lower `-max-chars`
(e.g. `-max-chars 150`) when running it.

## The vendored devnen server (`chatterbox-server` submodule)

The devnen server is vendored as a submodule and installed by `mise run chatterbox:setup`,
which mirrors devnen's own Apple-Silicon install (`deploy/chatterbox-setup.sh`): a **Python
3.10** venv at `chatterbox-server/venv`, `requirements.txt` (torch 2.5.1 w/ MPS), the
MPS-patched `chatterbox-v2` fork, and a patched `config.yaml` (`device: mps`, `host:
127.0.0.1`, `port: 8004`). The ~3 GB model downloads on first server run. The Go server just
speaks HTTP to it.

The submodule pins a specific commit (`ignore = dirty`, so the built `venv`/`config.yaml`
don't dirty the parent). If you bump it, re-check `chatterbox-server/start.py`'s install steps
against `deploy/chatterbox-setup.sh` — the exact pins are version-sensitive.

`mise run chatterbox:service:*` (install/start/stop/restart/status/logs) manage the devnen
launchd agent standalone; normally you don't need them — the server service co-manages it.

## Memory reset (`-chatterbox-unload-every`)

Chatterbox has an upstream memory leak (chatterbox #218 and related, unfixed as of writing).
Leave `-chatterbox-unload-every` at `0` until you've confirmed RSS climbs during a spike, then
set it (e.g. `15`–`25`): after every N generations the server best-effort POSTs `/api/unload`
to devnen, which reloads the model lazily on the next request. A failed unload is logged, not
fatal. The reset adds a one-time cold reload to the next generation — negligible at this
engine's low request rate.
