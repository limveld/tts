# 05 — Bot callouts

Status: resolved
Blocked by: 04

Split by **actionability**, not importance — chat reaches players 1–2s after the
event, the overlay 5–10s after (stream delay).

## Chat

Round open + join banner; seats locked (roster + key count); personal state
changes that alter what you should do next (key grabbed, spiked and where your
key dropped, stuck N cycles); big beats (last key taken, exit found, winner,
placement crawl).

**Coalesce to at most one line per tick**, the way `bot/gamble.go:265` already
batches joins. A cycle where three things happen is one message.

## Overlay

Continuous play-by-play as a **feed inside the maze panel**, plus the persistent
HUD (issue 06). Toasts are reserved for spikes and finishes only.

This issue originally said to put the play-by-play through the `notify` toasts.
That is not viable and was changed during implementation — see the comments.

## Budget

At an 8s tick, ~8 messages/30s sits well inside Twitch's 20/30s floor — rate
limits are not the constraint. The constraint is not burying human conversation
in a sub-10-viewer chat. Target roughly one chat line per 2–3 ticks.

## Comments

`announce` in `bot/maze.go` now returns chat lines *and* toasts, and logs a terse
line to the panel feed. Three sinks, split by how fast they reach a player and how
much room they have.

### The toast channel cannot carry a play-by-play

This issue asked for the continuous play-by-play to go through the existing
`notify` toasts. It can't. `showNextNotify` in `overlay.js` is **serialised**:
each toast holds the screen for 5000ms plus a 500ms leave, so the channel carries
roughly one item every 5.5 seconds. A cycle with five players routinely produces
three or four events, and the cycle is 8 seconds. The queue would grow without
bound and the "news" would arrive minutes after the board had moved on — worst of
all near the end, where it matters most.

So the play-by-play lives in a feed inside the maze panel, which renders with the
board and cannot lag it. Toasts are kept for the two beats worth interrupting the
screen for: **spikes and finishes**. Those are bounded by the board itself — at
most (traps + players) per round, about eight, or ~44s of toast time in a three
minute round.

`TestMazeToastsAreRareByConstruction` pins that allowlist, because the constraint
is a hard property of the transport rather than a matter of taste.

### What chat says, and what it doesn't

- **At most one line of texture per cycle**, joined with ` · `, no matter how much
  happened. Measured over played-out rounds at 5 players: **0.53 chat lines per
  cycle**, comfortably inside the "one line per 2-3 ticks" target.
  `TestMazeChatVolumeStaysWithinBudget` asserts it stays under 1.0 rather than
  leaving it as prose in a PRD.
- **Bonks never reach chat.** They are by far the most frequent event, they say
  only that someone guessed at a wall, and the board already shows the move was
  lost. They stay in the panel feed.
- **A spike and its key drop are one sentence.** The engine emits two events, but
  to a reader that is one thing happening, and saying the player's name twice in
  one line reads like a bug.
- **A bounce is announced once per player.** Someone standing at the door without
  a key is a moment the first time and noise the fourth.
- **The last key is called out specially.** That is the moment somebody's round is
  quietly over, and they should hear about it rather than work it out.

### The feed

Capped at `mazeFeedLines` (5), newest last, older entries faded by the renderer.
It is deliberately **not persisted**: a restart losing the log costs nothing, and
keeping it out of the round record keeps the stored document about the game rather
than about its narration.

### Verified

Every rule above was mutation-tested — broken one at a time to confirm the suite
fails. The panel feed and the "no keys left on the board" footer were checked in a
real browser against the real SSE transport; the toast was confirmed through the
rendered DOM, since a 5-second toast expires before a screenshot round-trip lands.
