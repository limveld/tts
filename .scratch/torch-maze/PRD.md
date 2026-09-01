# PRD: Torch Maze

Status: ready-for-agent

A chat-driven, tick-based maze race for the stream overlay. Up to 5 chatters each
drive their own sprite through a fogged 6x6 maze, scrambling for scarce keys and
racing to a locked exit while hidden traps fire once and clear.

Design frozen after a grilling pass. Every numbered decision below records the
reasoning, because several of them overturn an earlier assumption and the reason
is the part that will otherwise be lost.

## The loop

Someone types `!maze`. The bot claims the shared board stage, generates a seeded
map, and shows the fogged board with a join banner. For `join_cycles` the world
is frozen and the first `max_seats` distinct users to type `!go ...` are seated.
Seats lock, the key count resolves from the actual head count, surplus keys are
removed before they are ever rendered, and the clock starts.

Every cycle the engine drains one move from each player's queue, resolves it, and
redraws. First player through the exit door wins; the round stays live for
`placement_cycles` so the rest scramble for 2nd/3rd/4th, then settles, pays out,
and releases the stage.

## Decisions

### 1. No elimination. Spikes fling you back to start and drop your key.

The conversation ended deadlocked on whether a spiked key-holder drops the key
(risking a silently unwinnable round for someone) or takes it to the grave
(contradicting "keys are held till the door"). Both horns were symptoms of
permanent elimination, which does not belong here: on a blind board a trap is
unavoidable by definition, so early elimination is a coin flip, and it benches a
viewer for minutes on a channel with fewer than ten of them.

Removing death dissolves the contradiction. A key leaves play only through the
exit. "Held till the door" is now literally true. Spikes are still savage --
every tile of progress plus your key -- without removing anyone.

### 2. First exit wins; the round stays live for `placement_cycles`.

Ending instantly would make key scarcity cosmetic (being locked out and being
4th would be the same outcome). Running to the cycle cap gives a dull tail once
the fog is lifted. A short scramble for placements keeps scarcity meaningful and
the finish punchy. Placements pay (see 15), so the window is a real race.

**The default was wrong and was corrected while building issue 08: 12 cycles, not
6.** The original 6 was "about 30 seconds" at the 5-second tick this document
later abandoned, and nobody revisited it when decision 10 moved the tick to 8s.
Measured over played-out five-player rounds, a window of 6 produced **exactly one
finisher every single time** — the runners-up need around 10 cycles — so the
scramble never happened and the payout curve below first place was unreachable
code.

Raising it is nearly free, for a reason worth remembering: the round ends as soon
as no unfinished player holds a key and none remain on the board, so a window
longer than the field needs never actually runs out. Windows of 10, 12 and 14 all
produce four finishers in the same 19 cycles. 12 is chosen over 10 for headroom,
since the measurement comes from perfect-information routing where players clump
and real fogged play will spread finishers further.

### 3. Cells in Go, tiles in JS.

Game state is a `map_size` x `map_size` grid of 4-bit wall masks. That is what
the generator carves, BFS validates, fog reveals, the round record persists, and
what A1-F6 coordinates address so callouts can name locations. The renderer
expands it to a chunky (2n+1) block grid at draw time purely for the 8-bit look.
Backend never reasons about wall tiles; the generator is testable against a
36-element array instead of a bitmap.

### 4. Fog hides topology, not objectives.

**This overturns the original reading and it is the most consequential change.**
If a cell only reveals when stepped on, a key reveals at the instant it is picked
up -- nobody ever sees a key sitting on the board, so the `N-1` scramble that the
whole design is built around cannot physically occur. Objectives visible from
cycle 0 also make the board legible to spectators every cycle, which on a small
stream is the difference between watchable tension and five dots wandering a
black screen.

So: exit and key positions are marked from the start; walls and routes are
fogged and reveal as people explore; traps stay completely hidden.

Three render states per cell: unknown (black), frontier (an opening leads here so
it is known reachable -- dim, walls unknown), revealed (full walls and contents).
Standing on a cell reveals its own wall mask, so a player reading the board never
bonks by surprise.

### 5. Braid to a 2-connectivity guarantee, not a percentage.

**The original `braid_pct: 20` does nothing.** A recursive backtracker produces
few dead ends by design -- around 10% of cells, so roughly four on a 36-cell map.
Removing 20% of four removes zero or one. The generator would have emitted an
essentially perfect maze: one route, five players in single file, no overtaking,
which is precisely what braiding existed to prevent.

Replaced with a postcondition: start and exit must have **two vertex-disjoint
routes**. Algorithm-independent, trivially assertable in a test, and it
guarantees the thing actually wanted (a second player can take a different road).

**Ordering, settled during implementation.** The `loop_walls` removals happen
*first*, and the guarantee is established last, jointly with the exit. The three
depend on each other: the exit is the farthest cell from start, which moves
whenever a wall comes down, and the guarantee is defined against whichever cell
ends up being the exit. Establishing the guarantee first and then removing more
walls would preserve it (removing walls can never create a cut vertex) but would
leave the exit no longer a farthest cell. Doing it the other way round makes both
exactly true of the same board. The generator therefore iterates: pick the exit,
look for a cell whose removal would cut it off from start, open a wall around
that cell, pick again. It terminates because every pass removes a wall and a
fully open grid has no cut vertices.

Measured over 500 seeds on the shipping 6x6 config: exit distance p10=9, p50=11,
p90=13; nearest key p50=4. A route through a key is ~13-16 moves, so about two
minutes at an 8s tick -- which is what `max_cycles: 36` was sized against. That
measurement is now a test (`TestBoardPacing`), because a change that quietly
collapsed board difficulty would otherwise leave the game tuned for a board it no
longer generates while every other test still passed.

### 6. Constructive placement from one BFS distance field.

Rejection sampling fights the seed guarantee (unbounded retries; a bounded retry
that gives up emits a map violating its own constraints) and at ~28% special-cell
density the acceptance rate is poor. Instead, one BFS from start, then select by
rank:

- **exit** = argmax distance (longest possible round)
- **keys** = an equidistant band `[key_band_min, key_band_max]`, fanned into
  different directions
- **traps** = cells lying on a shortest path to a key or the exit

Cannot fail, fully seed-deterministic, each rule a one-line assertion.

Two notes worth keeping. First, since spikes no longer kill and traps despawn,
**no trap can make a round unwinnable** -- the original "assert no trap blocks the
only route" validation is unnecessary. Second, traps must be placed on traffic
routes or the mechanic silently no-ops: with objectives visible players walk
near-optimal paths covering ~12-15 of 36 cells, so uniformly scattered traps
would leave roughly two of three sitting where nobody has a reason to go.

**Keys are equidistant, not maximally separated.** An earlier draft called for
minimum pairwise spacing; that is wrong. Spread keys let players self-assign to
distinct targets, so `N-1` produces one quiet loser arriving late at an empty
tile rather than a scramble. An equal-distance band makes everyone arrive within
a cycle or two of each other and the shortfall resolve simultaneously.

### 7. The overlay shows moves are locked, never which direction.

Showing direction arrows adds a feint/intercept layer that hands every contested
key to whoever has the lowest IRC latency, and makes optimal play "input at
T-minus-1s", which is exactly when lag differences bite hardest. A ready
indicator plus an "n/5 locked" counter gives full input confirmation with no
counterplay window, so moves resolve genuinely simultaneously.

### 8. Movement is `!go <path>`, queue capped at 3.

Two blockers the design conversation did not account for.

`bot/router.go:86` drops any message not starting with `!`, so bare `wasd` never
reaches a handler. And `!w !a !s !d` is unavailable anyway: **`!d` and `!c` are
already sound effects** in `sfx.toml`, resolved at `bot/router.go:110` *before*
the command engine. `!m` is marks, `!g` is gamble, `!r` is `!don`. That namespace
is also user-editable at runtime, so any single-letter maze command is one
`!addcom` away from breaking.

The real blocker: **Twitch rejects a message identical to the sender's previous
one within 30 seconds** (`msg_duplicate`, non-mod/non-VIP). Tile movement on an
8s tick means walking a straight corridor sends the same command repeatedly --
silently dropped, with no feedback explaining why. Corridors run 2-4 cells, so
this is most of the game, not an edge case. *Verify live on the channel, but
design around it regardless.*

`!go wwd` submits up to `queue_max` moves consumed one per tick; any new `!go`
replaces the whole queue. Repeats stop being identical so the filter never fires;
typing drops to once per ~24s; and it adds a real commit-vs-react tension.
"Last input wins" survives at queue granularity. Word aliases (`!go up`,
`!go north`) accepted.

The cap matters: uncapped, once the fog lifts somebody types the whole remaining
solution in one message and the race is over.

**A bonk clears the rest of the queue.** Queued moves step into fogged cells
whose walls are unknown, so over-committing risks a wasted tick and a reset --
commit-vs-react self-balances with no tuning knob. Being spiked or bear-trapped
also clears the queue.

### 9. A frozen `join_cycles` window, then seats lock.

"No lobby" and "keys = joined - 1" could not both hold: key positions are chosen
by the generator, which runs before anyone can join, so there was no moment for
seats to lock against. And a player joining on cycle 12 spawns at start, twelve
cycles behind a half-lit board -- their join is a formality.

Round opens, generator places `max_seats - 1` key slots, the fogged board appears
with a join banner, movement frozen for `join_cycles`. At lock: N is known,
surplus key slots are removed before keys are ever rendered, first real tick
fires. Latecomers are seated next round. ~16s of otherwise-dead announce time,
not the 30s lobby that was rejected.

### 10. Tick is 8s, not 5s, because of stream delay.

Never considered in the original design, and it determines playability. Players
read the board *through the stream*: Twitch low-latency is ~3-5s, standard 8-15s,
plus 1-2s for `!go` to arrive over IRC. The full see-decide-type-receive loop is
realistically **5-10 seconds**. At a 5s tick that is one to two entire cycles of
staleness, and no UI fixes it because it is the transport.

The design already absorbs most of this: persistent fog never becomes wrong (only
incomplete), objectives never move, and path queues mean a decision every three
ticks. What stays stale is other players' positions and which keys remain -- so
the failure mode is sprinting for a key taken two cycles ago, which is at least
legible and decent drama.

At 8s the round trip fits inside one cycle. `max_cycles: 36` holds the round near
five minutes. **Measure the actual OBS-to-viewer delay once and tune from that.**

**The cycle cap was checked against played-out rounds, not just board geometry.**
Over 300 seeded rounds at five players, cycles to first win came out p50=16,
p90=24, max=33 against the 36-cycle cap — about 2.1 minutes at the median and 3.2
at p90. Repeating it with players making random moves 15/30/45% of the time
barely moved the numbers (p90 = 21/23/28), because the first win is a *minimum
over five players*: one player's sloppiness is absorbed by the other four. That
robustness is a genuinely nice property of racing several people at once.

One caveat on that measurement, worth keeping in mind at playtest: the simulated
players route with perfect information, and the slip model makes their mistakes
*independent*. Real fog is shared — everybody is ignorant of the same walls at the
same time — so it should lengthen rounds somewhat more than independent slips do.
The headroom looks sufficient either way, but this is the number to re-check on
stream.

### 11. Chat carries actionable state; the overlay carries play-by-play.

Rate limits are not the binding constraint at an 8s tick (~8 messages/30s sits
well inside the 20/30s floor). Chat *readability* is: a line every tick for five
minutes is ~35 bot messages into a sub-10-viewer chat.

But chat is fresher than video (1-2s vs 5-10s), so callouts a player needs in
order to act belong there. Split by actionability:

- **Chat**: round open/lock, personal state changes that alter your next move
  (key grabbed, spiked, stuck N cycles), and big beats (last key taken, exit
  found, winner, placements). Coalesced to at most one line per tick, the way
  `bot/gamble.go:265` already batches joins.
- **Overlay**: continuous toasts via the existing `notify` channel, plus the
  persistent HUD.

### 12. One renderer, `display: "panel" | "full"`, pushed from the bot.

Panel mode sits with the other games; full mode claims the stage for a dedicated
maze scene. Since the overlay holds no state of its own, the mode rides in the
pushed payload so a mod command flips it mid-round without a restart.

Panel mode carries a legibility caveat: a 6x6 grid with five sprites at
corner-panel scale is hard to read on a phone, which is how most of Twitch
watches. Full mode is the default for an actual round.

### 13. Keys = N - 1 for N >= 3; N keys at N <= 2.

`N-1` is a fixed absolute deficit but a variable proportional one: it locks out
20% of the field at N=5, 33% at N=3, and 50% at N=2. The design was reasoned
about at N=5, which is the least representative count for this channel.

At N=2 strict `N-1` gives one key: both players sprint from the same tile to the
same visible key, and the loser rides out ~60% of a two-minute round hoping the
leader eats a spike -- odds made worse by trap despawn, since the player who is
*ahead* trips traps first, so the chaser tends to walk a board the leader already
de-mined, or springs traps while carrying nothing.

So the deficit applies only at N >= `deficit_min_players`:

    if N >= deficit_min_players: keys = max(keys_min, N - key_deficit)
    else:                        keys = max(keys_min, N)

Giving 5->4, 4->3, 3->2, 2->2, 1->1.

### 14. Two spikes, one bear trap, stuck 2 cycles.

`traps: 3` never said the mix, and the mix is the comeback dial: spikes are the
only mechanism returning a key to the board, so spike density *is* the rate at
which a keyless player gets back into contention.

The hazards are asymmetric by an order of magnitude. A bear trap costs 2 cycles.
Spikes send you to the start of a map whose exit sits at maximum BFS distance --
roughly ten cells, or nearly a third of a 36-cycle round. That asymmetry is a
feature: the start tile is revealed and the fog persists, so being flung back
early costs almost nothing while being flung back late is devastating. Spikes
self-scale with round progress and bite hardest exactly when a keyless player
most needs a drop.

Bear trap duration drops from 3 cycles to 2 as a knock-on of decision 10 -- at 8s
a cycle, 3 cycles is 24 seconds of a player sitting frozen with nothing to do.

### 15. Curved payout plus `!mazewins`.

Winner takes `maze_reward`, 2nd half, 3rd a quarter. Same `Credit` plumbing as
`bot/wordle.go:269` with a different number, and it is what makes decision 2's
placement window a real race rather than a victory lap with spectators. No entry
fee -- seats fill by typing `!go` in a 16s window, and a marks cost there would
mean refusing people mid-join and suppressing exactly the casual participation
auto-join exists for.

## Integration points

| Concern | Where |
| --- | --- |
| Stage arbitration | `bot/board.go` -- add `boardMaze`, claim/release, `!skipgame` case, `boardBusyMsg` entry |
| Command dispatch | `bot/commands.go` -- `!maze`, `!go`, `!mazewins`; add all three to `isBuiltin` so `!addcom` cannot shadow them |
| Round persistence | `bot/rounds.go` -- add `mazeGame`; save/load/clear via the shared `RoundStore` |
| Overlay transport | `bot/overlay.go` `Push("maze", ...)`; add `"maze"` to `stateKinds` in `server/overlay.go:48` |
| Overlay assets | `server/web/overlay/` (embedded, served by `server/overlay.go:167`) |
| Wins tally | `store/{sqlite,postgres}/maze.go` + migrations in both trees + `store/storetest/tallies.go` + `cmd/store-migrate` |
| Rewards | `points.toml` alongside `wordle_reward` |
| Game rules | new `maze.toml`, following `sfx.toml` / `timers.toml` / `notifications.toml` |

**Restart resume stores the board, not just its seed.** The round record carries
the board, the ruleset in force, and all mutable state (cycle, positions,
held/dropped keys, sprung traps, revealed and frontier cells, queues, deadline).

An earlier draft of this document said to store the seed alone and regenerate,
on the grounds that the generator is deterministic. That is true but it is the
wrong trade, and it was corrected while building issue 03. Regeneration
reproduces the original board only if the generator *config* and the generator
*code* are both byte-identical to when the round started, and neither is
guaranteed: `maze.toml` gets edited between restarts, and a redeploy can change
the carve. The failure would be silent and severe — players resumed standing
inside walls, keys and traps relocated — and it would happen precisely when
someone was deploying a tweak mid-stream, which is when restarts are most likely.
The whole board is a few hundred bytes of JSON, so "cheaper" was never a real
saving. The seed still rides along, for display and rematches.

The ruleset is persisted for the same reason: a round resumed after a restart
plays by the rules it started under, so editing `maze.toml` mid-round cannot
change the scoring of a game already in progress.

Enums cross the wire as strings and cells as their chat coordinates ("C4"), so
inserting a new `Phase` or `EndReason` constant later cannot silently
reinterpret already-stored rounds, and a stored round is legible in psql without
decoding anything.

## Config surface

`maze.toml` (game rules):

    tick_seconds        = 8      # measure your stream delay and tune
    max_cycles          = 36     # ~4.8 min
    max_seconds         = 320    # wall-clock safety cap
    join_cycles         = 2      # frozen join window (~16s)
    placement_cycles    = 12     # scramble for 2nd/3rd after the win (see decision 2)
    max_seats           = 5

    map_size            = 6      # 6 or 7
    loop_walls          = 4      # extra walls removed beyond the 2-connectivity guarantee
    seed                = ""     # empty = random per round; set for rematch / seed-of-day

    key_deficit         = 1
    deficit_min_players = 3
    keys_min            = 1
    key_band_min        = 4      # keys placed in this BFS band from start
    key_band_max        = 6

    spikes              = 2      # back to start, drop key
    bear_traps          = 1
    bear_trap_cycles    = 2

    queue_max           = 3      # moves per !go
    display             = "full" # "panel" | "full"

`points.toml` (payouts): `maze_reward`.

## Tick resolution order

1. Decrement bear-trap timers; anyone hitting 0 is freed (moves next cycle).
2. Pop one move from each free player's queue and apply: wall -> bonk (no-op,
   queue cleared), locked exit without key -> bounce (exit revealed), else step.
3. Resolve destination: pick up key (one per player), spring trap (fires once,
   then despawns, tile stays revealed), or exit if holding a key.
4. Reveal the destination cell's wall mask; mark reachable neighbours as frontier.
5. Clear expired state, fire coalesced callouts, push overlay state, reset
   countdown.

Players stack freely -- no PvP, so simultaneous resolution is order-independent.
The only contested case is two players landing on the same key in one cycle;
tie-break by earliest buffered-input timestamp.

## Deliberately out of scope for v1

- The 0-1 internal shortcut door. The map format should carry it, but the
  strategic tension it adds ("spend your key on the shortcut and you need another
  for the exit") is not worth the balance risk before the base loop is proven.
- Co-op / voting mode.
- Enemies.
- Auto-looping rounds. `!maze` runs exactly one round; it holds the shared stage
  and should not monopolise it.
- Fog regrowth.

## Open items for playtest

- Actual OBS-to-viewer delay on this channel -> `tick_seconds`.
- Whether `msg_duplicate` fires as expected (decision 8).
- Whether back-to-start is too punishing late in a round (decision 14 alternative:
  1 spike + 2 bear traps).
- Whether `map_size = 7` is better once players learn 6x6.
