# 02 — Game state and tick engine

Status: resolved
Blocked by: 01

The authoritative round state and its cycle resolution. Still no chat or overlay
coupling — drive it from a test harness that feeds moves and asserts state.

## Scope

Round state: cycle number, per-player (position, held key, bear-trap counter,
move queue, finish placement), fog sets (revealed cells, frontier cells),
remaining keys on map, sprung traps, deadline.

Mutation happens only inside the tick, so one goroutine owns the state and chat
events land on a channel — same shape as the other games.

## Resolution order (per PRD)

1. Decrement bear-trap timers; anyone hitting 0 is freed (moves next cycle).
2. Pop one move per free player: wall → bonk (no-op, **queue cleared**), locked
   exit without key → bounce (no move, queue cleared, *nothing revealed* — the
   exit's position was always visible and the player never entered the cell, so
   the "exit revealed" in the original draft of this issue was a leftover from
   before fog was narrowed to topology only), else step.
3. Resolve destination: pick up key, spring trap (fires once then despawns), or
   exit if holding a key.
4. Reveal destination wall mask; mark reachable neighbours as frontier.
5. Clear expired state, emit events, reset countdown.

Players stack freely. Two players landing on one key in the same cycle tie-break
by earliest buffered-input timestamp.

## Rules to get right

- Spikes: teleport to start, drop held key on the spike tile (revealed,
  re-grabbable), clear queue. **No elimination.**
- Bear trap: immobilise `bear_trap_cycles`, keep held key, clear queue.
- Key count at lock: `N >= deficit_min_players ? max(keys_min, N - key_deficit)
  : max(keys_min, N)`. Surplus key slots removed before first render.
- Round ends on: first exit + `placement_cycles`, or `max_cycles`, or
  `max_seconds` — whichever trips first.

## Tests

- A spiked key-holder's key returns to the board; total keys in play only ever
  decreases through the exit.
- Bear-trapped player keeps their key and cannot move for exactly N cycles.
- A trap springs once and is inert thereafter.
- Queue is consumed one per tick and fully replaced by a new submission.
- Key count resolves correctly at N = 1..5.
- Round terminates under each of the three end conditions.

## Comments

Built as `internal/maze/round.go` in the same package as the generator, which it
queries constantly for walls, neighbours and distance fields. Public surface is
`NewRound`, `Join`, `Submit`, `Tick`, `ParsePath`, and read-only views
(`Revealed`, `Frontier`, `KeysOnMap`, `TrapSprung`, `PlayerBy`, `Placements`,
`Deadline`).

**The engine contains no randomness at all.** Everything variable comes from the
seeded board or from player input, both of which are persisted — so a round
resumed after a restart replays identically. Issue 03 depends on that; do not add
a coin flip in here.

Decisions made during implementation that were not in the issue text:

- **A trap fires for every player who steps on it in the same cycle**, not just
  whoever the tie-break visits first, and despawns afterwards. They stepped on it
  at the same instant; sparing the second because their message arrived later is
  a worse outcome than springing it twice.
- **Submitting while bear-trapped is allowed.** The trap already cleared the
  queue; letting someone line up their escape is friendlier than swallowing input
  with no explanation. They still lose the full count of cycles.
- **`EndNobodyCanFinish` short-circuits the placement window.** Keys re-enter play
  only when a key-holder hits spikes, so once no racer holds one and none are on
  the board, no finish is possible and the round is running down a clock nobody
  can beat. Fires in ~6% of simulated rounds.
- **Single letters are WASD, spelled-out words are compass/arrow.** The two
  schemes collide on exactly one letter — `w` is up, not west — and guessing
  would send a player the opposite way from what they meant.

Two tests carry more weight than the rest:

- **`TestKeyConservation`** plays 200 randomly-driven rounds and asserts every
  cycle that keys on board + keys held + keys spent equals the count at lock. A
  key may only exist in one of those three places, and every trap, drop and
  pickup path has to preserve it. Random play exercises the messy paths well
  (192 spikes, 103 bear traps, 37 key drops over 200 rounds) but almost never
  reaches the door, hence the next one.
- **`TestGreedyPlaythrough`** plays 300 rounds under competent routing, which is
  the only test that exercises the whole chain at once and is what backs the
  cycle-cap claim recorded in PRD decision 10.

Not done, deliberately: no persistence (issue 03), no chat or overlay coupling
(issues 04-06). `RoundConfig` is populated by its caller until issue 09 reads
`maze.toml`.
