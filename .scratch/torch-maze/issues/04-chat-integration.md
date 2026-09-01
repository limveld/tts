# 04 — Chat commands, join window, stage arbitration

Status: resolved
Blocked by: 02

## Commands

- `!maze` — start a round (anyone, matching `!wordle`). Claims the board stage.
- `!go <path>` — join and/or queue up to `queue_max` moves. Letters (`wasd`),
  word aliases (`up`/`down`/`left`/`right`, `n`/`s`/`e`/`w`, `north`/…).
- `!mazewins` — leaderboard (issue 08).
- `!skipgame` — existing mod command; add the maze case.

**Add all three to `isBuiltin` in `bot/commands.go:288`** so `!addcom` cannot
shadow them.

## Why `!go` and not `!w`/`!a`/`!s`/`!d`

`!d` and `!c` are already SFX in `sfx.toml` and resolve at `bot/router.go:110`
*before* the command engine. `!m`/`!g`/`!r` are taken too, and the namespace is
user-editable at runtime. See PRD decision 8.

## Why a queue and not one move per message

Twitch drops a message identical to the sender's previous one within 30s
(`msg_duplicate`). Walking a straight corridor would silently stop working. A
path queue makes consecutive submissions differ by construction. **Verify this
fires as expected on the channel during playtest.**

## Join window

Round opens → generate → push fogged board with join banner → freeze for
`join_cycles` → first `max_seats` distinct users typing `!go` are seated →
lock, resolve key count, drop surplus keys → first tick.

Post-lock arrivals get a "seated next round" reply, not silence.

## Stage arbitration

`bot/board.go`: add `boardMaze`, claim on start, release on settle, add the
`!skipgame` case and a `boardBusyMsg` entry. Refuse to start if wordle or
connections holds the stage.

## Comments

Built as `bot/maze.go`, following the shape the other games use: mutate and
persist under `mazeMu`, snapshot what the outside world needs, unlock, then do
the chat and overlay I/O. Nothing that can block happens holding the mutex, and
`claimBoard`/`releaseBoard` are never nested inside it.

Wired in: `boardMaze` in `bot/board.go` (claim, release, `!skipgame`, busy
message); `!maze` and `!go` dispatch in `bot/commands.go`; `mazeCfg` and
`randInt63` on the Router; `loadMaze()` in `bot/main.go`.

This issue also picked up the two call sites its neighbours deliberately left
behind: the save/load from issue 03, and the overlay push from issue 06. A round
does not run without them.

### Decisions taken here

- **A successful `!go` says nothing.** Five players on an 8s cycle would put ~35
  confirmations into a chat with under ten people in it. The overlay carries a
  locked-in indicator instead; only failures and seating answer back.
- **Join confirmations are coalesced into the roster line at lock**, the way
  gamble already batches its joins — one message instead of five.
- **Submissions are stamped with arrival time at the bot.** `ChatMessage` carries
  no timestamp, and the IRC reader is sequential, so arrival order is delivery
  order — which is the order Twitch itself put the messages in.
- **`mazeConfig` lives on the Router**, defaulting to `defaultMazeConfig()`. That
  is where issue 09 should land `maze.toml`; nothing else needs to change.
- **`!mazewins` is reserved in `isBuiltin` but not dispatched.** It needs the
  store slice from issue 07, so issue 08 wires it. Reserving the name now stops
  anyone `!addcom`-ing it in the meantime.

### The render payload withholds three things

Worth knowing before writing the renderer (issue 06), because the payload cannot
show what it never receives:

- **Unsprung traps never leave the bot.** They are the only genuine surprise left
  once objectives are visible, and a payload carrying them would put every one a
  devtools panel away.
- **Unrevealed cells carry no wall data.** The fog hides the maze's shape, so
  hiding it in CSS would leave it readable in the page.
- **A queued move's direction is never sent** — only that one is locked. Seeing
  intent would let a player on a fast connection intercept one on a slow
  connection every cycle, which is the unfairness the tick model exists to remove.

Objectives are the deliberate exception and are always sent.

### On the tests

Cycles are driven by calling `tickMaze` directly after halting the round's real
ticker, so nothing waits on an 8-second clock. `mazeRound.halt` is a `sync.Once`
so that and force-ending cannot race into a double close.

Every guarantee above was mutation-tested — broken deliberately, one at a time,
to confirm the suite fails. Two gaps turned up that way and both were fixed
rather than accepted:

- **The stage-release path was completely untested**, because it runs behind a
  15-second `time.AfterFunc` that never fires in a fast test. A stage that is
  never released is a bot that can never start another game until a mod
  intervenes. `clearMaze` is now split out of the timer closure so it can be
  driven directly, and both it and the stale-clear guard are covered.
- **The generate-failure path was unreachable from a test**, which matters
  because the stage is claimed *before* the board is built — forgetting to hand
  it back would wedge every game in the bot. Router-held config made it
  reachable, which issue 09 wanted anyway.
