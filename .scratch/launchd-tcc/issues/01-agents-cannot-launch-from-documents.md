# launchd agents can't start binaries or scripts under ~/Documents

Status: ready-for-human
Type: task
Created: 2026-08-13

## Summary

Every launchd service in this repo is currently unable to start, because macOS blocks
launchd-spawned processes from reading anything under `/Users/rtukpe/Documents`. The repo lives
there, so `WorkingDirectory`, the binaries in `bin/`, the config TOMLs, `bot.tokens.json` and
`bot.db` are all behind the block.

Discovered on 2026-08-13 while restarting `com.rtukpe.tts-bot` to deploy the token-refresh fix (see
[`../../token-refresh/issues/01-serialize-token-refresh.md`](../../token-refresh/issues/01-serialize-token-refresh.md)).
The already-running processes were unaffected — they were started on 2026-08-11, before the block
took effect — so this stayed invisible until the first restart.

**Not caused by a code change.** `bin/pg-partition`, untouched since 2026-08-11 21:57, hangs
identically.

## Symptoms

Two distinct shapes, depending on how the agent is invoked:

- **Program is a binary under `~/Documents`** (`tts-bot`, `pg-partition`, and `tts-server` once it
  restarts): the process starts, never reaches `main`, and sits forever at RSS ~128 KB with no log
  output. `sample` shows it wedged in dyld before any Go code runs:

  ```
  dyld4::prepare
    dyld4::JustInTimeLoader::makeLaunchLoader
      dyld4::Loader::getOnDiskBinarySliceOffset
        dyld4::SyscallDelegate::withReadOnlyMappedFile
          dyld4::SyscallDelegate::mapFileReadOnly
            dyld3::open → __open        <-- blocked indefinitely
  ```

  launchd's `KeepAlive` then respawns it into the same wedge.

- **Program is a script run through `/bin/bash`** (`com.rtukpe.tts-pg-backup`): fails fast instead of
  hanging, exit code 126, since 05:00 today.

  ```
  shell-init: error retrieving current directory: getcwd: cannot access parent directories: Operation not permitted
  /bin/bash: /Users/rtukpe/Documents/tts/deploy/pg-backup.sh: Operation not permitted
  ```

## Reproduction

A throwaway agent with no connection to this project reproduces both:

```xml
<key>ProgramArguments</key>
<array><string>/bin/sh</string><string>-c</string>
  <string>head -1 /Users/rtukpe/Documents/tts/go.mod</string></array>
```

```
$ launchctl bootstrap gui/501 /tmp/tcc-test.plist
head: /Users/rtukpe/Documents/tts/go.mod: Operation not permitted
```

Point the same agent at `/Users/rtukpe/Documents/tts/bin/pg-partition -h` and it produces no output
at all — the dyld hang above. The identical command from an interactive shell works fine, which is
the tell: the shell's parent app holds a Documents grant and launchd agents do not.

## Diagnosis

`~/Documents` is one of macOS's TCC-protected folders. A launchd agent is its own TCC subject and
has no grant, and there is no foreground app to attribute a consent prompt to — so the request never
resolves and the `open()` blocks rather than returning `EPERM`. `/bin/bash` and `/bin/sh` are Apple
binaries with no grant either, which is why the script-based agent gets a clean denial instead.

macOS 26.5.2 (25F84). What flipped the permission is unknown; the last successful launchd run of
`pg-partition` was 09:40 today, and `pg-backup` was already failing at 05:00.

## Options

1. **Grant Full Disk Access to each binary** — System Settings → Privacy & Security → Full Disk
   Access, `+`, then ⌘⇧G to type the path (bare binaries don't show in the picker otherwise):
   `bin/tts-bot`, `bin/tts-server`, `bin/pg-partition`, plus `/bin/bash` for `pg-backup.sh`.
   Cheapest, but TCC keys on code identity and `go build` produces ad-hoc-signed binaries whose
   identity changes on every build — so this plausibly needs redoing after each `mise run *:build`.
   Worth testing that assumption before relying on it.
2. **Move the runtime out of `~/Documents`** — e.g. `~/Library/Application Support/tts/` — and point
   `deploy/service.sh` at it. `~/Library` is not TCC-protected, so this is the durable fix. It has to
   move the whole runtime set, not just the binaries: `WorkingDirectory`, `bin/`, the config TOMLs,
   `connections.json`, `bot.tokens.json` and `bot.db`. The source tree can stay where it is.
3. Sign the binaries with a stable identity so a single Full Disk Access grant survives rebuilds.
   Combines with option 1 and removes its main drawback.

Option 2 is the recommendation: it removes the whole class of problem rather than re-granting
against it.

## Current state

`com.rtukpe.tts-bot` is **unloaded** (deliberately — with `KeepAlive` set it would spin on wedged
processes), and the bot is running as a plain shell-launched process instead:

```
6642 ./bin/tts-bot -channel rtukpe -tts-url http://127.0.0.1:8080
```

That is a stopgap. It has no `KeepAlive` and will not come back after a reboot. `tts-server`
(pid 70539) is still up from before the block and will hit the same wall whenever it restarts.

## Acceptance

- `mise run bot:service:start` brings the bot up and it logs `twitch: chat replies enabled` within a
  few seconds.
- `com.rtukpe.tts-pg-backup` completes its next scheduled run without `Operation not permitted`.
- A rebuild (`mise run bot:build`) followed by a service restart still starts cleanly — the check
  that decides whether option 1 alone is sufficient.
