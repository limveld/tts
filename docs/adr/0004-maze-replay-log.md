# ADR-0004: A permanent, unpartitioned replay log for the maze

Date: 2026-09-01
Status: Accepted

## Context

Torch Maze rounds were unrecoverable. The in-flight round lives in `game_rounds`
so a restart does not strand a race, and it is deleted the moment the round
clears — so once a game ended, the only trace was whatever the bot had said about
it in chat.

That turned out to matter sooner than expected. The first live round produced a
bug report ("I moved two squares") that was only diagnosable because
`chat_message` still held the commands and their timings. The game's own state —
where everyone actually was, which keys were where, what the board looked like —
was already gone, and had to be reasoned about backwards from the narration.

The engine makes a better answer cheap. `internal/maze` contains no randomness
at all: a round is a pure function of its board, its ruleset and the ordered
submissions. That property already exists for restart-resume, and it means a
round can be genuinely re-run rather than merely summarised.

## Decision

Two tables, `maze_rounds` and `maze_events`, written when a round ends and never
pruned.

`maze_rounds` holds the replay input: the board, both configs, and every move in
order, as one opaque document, with the summary columns lifted out beside it so a
round is legible in `psql` without decoding anything. `maze_events` holds what the
round emitted, so "how often does the last key decide it" is a query rather than a
re-simulation.

**Store the board, not the seed.** `internal/maze/persist.go` already argues this
for resume: regeneration reproduces the original board only if the generator
config *and* the generator code are byte-identical. For an archive read back years
and many redeploys later, the argument is stronger, and the board is a few hundred
bytes.

**Not partitioned.** This runs against ADR-0002 and ADR-0003, which both partition,
so it is worth being explicit. Volume is part of it — a round is at most a few
dozen cycles times five players, where `chat_message` takes that many rows in an
hour — but volume is not the decisive argument. Partitioning exists in this
codebase to enable retention, and permanence is the entire point of this table. A
partitioned table nothing is ever dropped from is all of the machinery — a default
child, a premake window, gopartman registration, the nightly agent, the
`chat_stats`-style "your total is this table plus a live count" seam in every query
— and none of the benefit.

**The archive is written where a round ends, not where it clears.** Rounds end
five different ways and only one of them produces an engine event: a finishing
tick, a moderator's `!skipgame` (which never reaches `PhaseDone` at all), and a bot
restarting onto a round that finished in the instant it died. The last is the
easiest to miss, because nothing about it looks like a round ending. The write is
idempotent rather than depending on an argument that no two paths can both fire.

**The accumulating log rides in the in-flight round document.** `persistMaze`
already writes that every cycle, so a bot that restarts mid-round still has the
first half of the game when it comes to archive it. Buffering in memory would
lose everything before the restart; writing per-cycle would put database I/O back
on the game's ticker, which a separate fix had just removed.

## Consequences

The table grows without bound. At a few hundred rows per round and a handful of
rounds a stream, that is thousands of rows a year — not a number worth managing.
If it ever becomes one, the fold-and-drop pattern from ADR-0002 applies, and the
replay documents are the part worth keeping.

Event kinds and end reasons are stored through explicit wire maps in
`internal/maze/persist.go`, never through their `String()` methods. Those are
display text and get reworded; `EventKind.String()` also ends in a default arm, so
a kind added later and forgotten would be silently recorded as `round-ended`. The
maps have no default, and a test asserts every kind has its own spelling.

Nothing reads the archive yet. A replay tool — re-run a stored round through the
overlay at any speed — is the obvious next thing and is deliberately not in this
change: the data is what is irrecoverable, and a viewer can be built from it at
any time.
