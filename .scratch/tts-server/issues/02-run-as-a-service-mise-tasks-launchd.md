# Run the TTS server as a service + mise tasks

Status: done
Type: task
Created: 2026-07-02

## Summary

Add `mise` tasks for everyday use and a macOS **launchd LaunchAgent** so the TTS server
can run as an always-on background service (auto-start at login, restart on crash).

## Docker vs native service — decision

**Native launchd, not Docker.** Docker Desktop on macOS runs containers inside a Linux VM
with no access to the host's CoreAudio output (VLC can't reach the Mac speakers) and no
Apple-Silicon MPS GPU. The server's whole job is to play audio on this machine, so a
container can't fulfill it. A per-user LaunchAgent runs in the GUI/Aqua session and has
both audio and GPU access — the right fit. (A LaunchDaemon runs as root outside the audio
session and is wrong here.)

## What was added

- **`mise.toml` `[tasks]`**: `setup` (venv + deps), `build`, `serve`, and
  `service:{install,uninstall,start,stop,restart,status,logs}`.
- **`deploy/com.rtukpe.tts.plist.template`** — LaunchAgent: absolute `ProgramArguments`
  (`tts -addr … -python .venv/bin/python -sidecar …`), `PATH` incl. `/opt/homebrew/bin`
  (espeak-ng), `PYTORCH_ENABLE_MPS_FALLBACK=1`, `RunAtLoad`+`KeepAlive`,
  `ProcessType=Interactive`, logs to `~/Library/Logs/tts-server.{out,err}.log`.
- **`deploy/service.sh`** — renders the template into `~/Library/LaunchAgents/` and drives
  launchd via modern verbs (`bootstrap`/`bootout`/`kickstart`/`print`) in `gui/$(id -u)`.
- `.gitignore`: added `.DS_Store`.

## Usage

```
mise run setup            # one-time: create venv + install kokoro
mise run build            # build ./tts
mise run serve            # foreground dev run
mise run service:install  # go live: load + start the always-on agent
mise run service:status   # state + /healthz
mise run service:logs     # follow the error log
mise run service:stop     # unload;  service:uninstall to remove entirely
```

Override the listen address with `TTS_ADDR=127.0.0.1:9000 mise run service:install`.
Add auth by uncommenting `-token`/`TTS_TOKEN` in the plist template.

## Decisions (with user)

- **Tooling only** — the service was NOT enabled/loaded; the user runs
  `mise run service:install` when ready.
- **Skip Docker.**

## Acceptance criteria

- [x] `mise tasks` lists all tasks with descriptions.
- [x] `mise run build` produces `./tts`.
- [x] Rendered plist passes `plutil -lint`; all `__REPO__/__ADDR__/__LOGDIR__` substituted.
- [x] `mise run service:status` reports "not loaded" (nothing auto-enabled).

## Comments

### 2026-07-02 — done (tooling only, not enabled)

Verified: 10 mise tasks listed; `mise run build` → 8.3 MB `tts`; `deploy/service.sh render`
→ valid plist (`plutil -lint: OK`) with all placeholders resolved to absolute paths;
`service:status` → "not loaded" + healthz "not responding"; `launchctl print` confirms the
agent is not registered. Not committed (left for review).

### 2026-07-07 — added `mise run reload`

One command to apply edits to `sfx.toml`/`voices.toml` **or Go code**: rebuilds both binaries
(mtime-gated via `server:build`/`bot:build`), runs `sfx:fetch`, then restarts both launchd
services. Rule: `reload` for config/code changes (no reinstall); `*:service:install` only when the
**plist** changes (flags/args, `EnvironmentVariables`, `WorkingDirectory`, template, or `service.sh`
render). Auto-reload (in-process file-watch/hot-swap) was considered and **rejected** — config is
edited off-air, so a restart blip is free and watching would be over-engineering.
