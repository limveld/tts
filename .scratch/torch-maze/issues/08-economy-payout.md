# 08 — Payout and !mazewins

Status: resolved
Blocked by: 04, 07

Curved payout on settle: winner `maze_reward`, 2nd half, 3rd a quarter. Same
`Credit` call as `bot/wordle.go:269`, guarded by `r.economy && reward > 0`.

This is what makes the `placement_cycles` window a real race rather than a
victory lap — without it, everyone who lost the race to the door has no reason to
keep walking.

- `maze_reward` in `points.toml` alongside `wordle_reward`; add the field to
  `EconomyConfig` in `bot/economy.go`.
- `MazeAddWin` on the winner only; `!mazewins` mirrors `showWordleWins`. The
  name is already reserved in `isBuiltin` (issue 04) but not yet dispatched —
  add the case to the switch in `bot/commands.go`.
- No entry fee (PRD decision 15).

## Comments

`maze_reward` added to `EconomyConfig` and `points.toml` (default 100); `MazeWins`
declared in `bot/maze.go` and added to the bot's `Store` contract; `!mazewins`
dispatched. Payout is a halving curve floored at one mark — 100/50/25/12 — so
finishing at all beats not finishing, which is the only thing that gives the
placement scramble a reason to exist. Only the winner is tallied; placements are
already recorded in the ledger, and a leaderboard counting "times I came third"
answers a question nobody asks.

### Finishers are paid when they finish, not when the round settles

The first version paid at settle, and writing the tests exposed what that means:
a mod running `!skipgame` during the placement scramble would take a win that had
already happened away from whoever earned it. Wordle does not have this problem
because it pays at the moment of the solve.

So the payout moved onto the finish itself, and the chat line for a finish is
written by the award path rather than by `announce` — one line, and the marks and
the sentence cannot disagree. `TestMazeSkipCannotUnpayAWinner` pins it.

### placement_cycles was too short for the feature to work at all

**Every single played-out round ended with exactly one finisher.** The runners-up
need around 10 cycles to reach the exit behind the winner and the window was 6, so
second place never happened, the scramble this whole issue's payout curve exists
to reward never occurred, and everything below first place was unreachable code.
Nothing failed; the game had just quietly stopped having a feature.

The 6 was "about 30 seconds" at the 5-second tick the design later abandoned, and
nobody revisited it when the tick moved to 8s.

Raised to 12, which measurement shows is nearly free: the round ends as soon as no
unfinished player holds a key and none remain on the board, so a window longer
than the field needs never runs out. Windows of 10, 12 and 14 all produce four
finishers in the same 19 cycles; 6 produces one in 15. 12 over 10 for headroom,
since the measurement uses perfect-information routing where players clump and
real fogged play will spread finishers further.

`TestMazePlacementWindowProducesPlacements` now guards it. It is the kind of
regression that needs a test precisely because it is silent.

### Verified

Seven mutations across the payout, the tally, the economy guard, the pay-on-finish
ordering and the window size — all caught. Run under `mise run test:all`, since a
bare `go test ./...` skips the Postgres half of this repo.
