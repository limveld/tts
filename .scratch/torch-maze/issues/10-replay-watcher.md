# 10 — replay watcher

Status: ready-for-agent
Blocked by: —

Re-run a stored round through the overlay at any speed. ADR-0004 closes with this
being "the obvious next thing", deliberately left out of the change that built the
archive: *the data is what is irrecoverable, and a viewer can be built from it at
any time.*

The archive has since proved its worth — the first live rounds produced the
"cycle 25 / 60" deadline bug and a spike-drop collision, both of which had to be
reasoned about from chat narration because nothing could re-run the game.

## What already exists — do not rebuild it

The store half is **done**, in both backends, with conformance cases:

- `MazeRoundLog(n)` — newest-first summaries
- `MazeRoundByID(id)` — one round, including the `Input` document
- `MazeRoundEvents(id)` — its events in emission order
- `store/storetest/storetest.go:56-59` pins all four as the backend contract

The engine half is done too: `internal/maze` contains **no randomness at all**, so
a board plus a ruleset plus the ordered submissions is a complete description of a
game. `maze.NewRound`, `Round.Join`, `Round.Submit` and `Round.Tick` are the whole
replay API.

And the transport is done: the overlay is a pure render target that draws whatever
lands on `POST /overlay/state`. It does not know or care whether a live bot or a
replay tool sent it.

So this issue is not "build a replay system". It is a reader, a reconstruction, a
clock, and one refactor.

## Blocker 1 — `Initial` is the *final* state, not the initial one

`mazeRound.archive` does `st := mr.round.State()` **at archive time**, then stores
it as `mazeReplay.Initial`. The field's own doc comment says it is "the board and
rules it started under… Restore it, replay the moves a cycle at a time, and the
engine produces the identical game". It is not, and it does not.

Verified against a stored 35-turn round:

```
initial_phase:  "done"     keysOnMap: []      sprung: [0, 2]
initial_cycle:  35         player: B2, place 1, finishedCycle 35
```

`maze.Restore` on that yields a round that has already ended. Feeding it the 33
stored moves does nothing at all — every player is placed and the phase is
terminal. Anything written against the comment rather than the data will appear to
work (no error, no panic) and silently replay an empty game. **Fix this first, or
the watcher is built on sand.**

Nothing is actually lost, which is why this is a bug and not a disaster. The
immutable half is intact and sufficient:

| carried | where | correct? |
| --- | --- | --- |
| board | `Initial.Map` | yes — immutable |
| ruleset | `Initial.Cfg` | yes — immutable |
| generator config | `Gen` | yes |
| every move, ordered, with submission times | `Moves` | yes |
| seat → user identity | `Initial.Players[]` | yes |
| round start instant | `Initial.StartedAt` | yes |

Two ways out. Prefer the first:

1. **Store the real opening state.** Capture `State()` once in `startMaze`, keep it
   on `mazeRound`, and archive that. It must also ride in `mazeRec` so it survives
   a mid-round restart — the same reasoning that already puts the accumulating log
   there. Costs a few hundred bytes held for the round's lifetime, and makes the
   document mean what it says.
2. **Rename the field and reconstruct on read.** Call it `Final`, and have the
   replayer build the opening state itself: `maze.NewRound(Map, Cfg, StartedAt)`,
   then `Join` each player **in seat order** to reproduce seat numbers. Cheaper,
   but it leaves every future consumer to rediscover that the obvious thing is
   wrong.

Either way, add a conformance case asserting a replayed round reaches the same
end state as the stored one. That is the assertion this whole table exists for and
nothing currently makes it.

## Blocker 2 — the payload builder is trapped in `package main`

`mazeBoard` and `mazePayload` live in `bot/`, which is `package main`, so no other
binary can use them. Copying them into the tool is the one thing that must not
happen: a second copy of the render payload will drift from the first, and the
divergence shows up as a replay that does not look like the game.

**Extract to `internal/mazeview`**: the `mazeBoard`/`mazeCell`/`mazeTrap`/
`mazePlayer` types, `mazeSeat` (the palette — already load-bearing, since chat
tells a player their colour and a second copy could disagree), and a builder over
`(*maze.Round, display, tickMS, cycleMsLeft, endsAtCycle, roundID, feed)`.
`mazePayload` reads almost nothing else off `mazeRound` today, so the seam is
close to where the code already is. `bot` then imports it, and so does the tool.

## The watcher itself

`cmd/maze-replay`, in the shape of the existing `cmd/` tools:

```
-round <id>     replay one round        -speed 2.0   multiplier on the stored tick
-last           the most recent round   -from 12     skip to a turn
-list           print recent rounds and exit
-url / -token   default to the same env the bot uses
```

Loop: read the round → decode `Input` → reconstruct the opening state → for each
turn, `Submit` that turn's moves in stored `At` order (the engine tie-breaks a
contested key on submission time, so order matters), `Tick`, build the payload,
push, sleep `tick / speed`. Finish with a hidden push, as the bot does.

Two things worth doing properly:

- **Refuse to run while a live round is on the stage.** There is one `maze` slot
  in the overlay's state cache, so a replay and a live round fight over the board
  and both look broken. Check `game_rounds` for a maze row and refuse with a clear
  message. This is not hypothetical — two overlapping rounds were driven into the
  same slot during the UX pass and the board flickered between two games.
- **Narration.** `maze_events` already holds what the round emitted, so the feed
  can be rebuilt from stored events rather than re-derived. If `logFeed` moves to
  `internal/mazeview` alongside the payload, the replay feed is the real one.

## Known gaps in what is stored

- **The resolve beat is not archived.** `tick_ms` is a column; `resolve_seconds`
  has no equivalent, so a replay cannot reproduce a round's real pacing — only its
  input window. Either add a column (migration in both trees, plus `storetest` and
  `cmd/store-migrate`) or fold it into the `Input` document. Decide before the
  watcher hard-codes an assumption.
- **`display` is not archived.** Panel and full are different games to watch. The
  tool should take it as a flag and default to `full`.

## Is the archive trustworthy?

Yes, and this is worth writing down because the watcher depends on it.

The only write path in either backend is a single `INSERT … ON CONFLICT DO
NOTHING` per table (`maze_rounds` by `id`, `maze_events` by `round_id, seq`).
There is no `UPDATE`, no `DELETE` and no `TRUNCATE` against either table anywhere
outside a migration rollback. The tables are deliberately unpartitioned — ADR-0002
and ADR-0003 partition to enable retention, and permanence is the entire point
here — so nothing prunes them either.

So a stored round is immutable once written, and a re-run of one is reproducible
by construction. That is exactly the property a replay tool needs, and it is the
reason this is worth building.
