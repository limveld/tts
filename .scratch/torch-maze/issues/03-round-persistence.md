# 03 — Round persistence and restart resume

Status: resolved
Blocked by: 02

Follow the shared shape in `bot/rounds.go`: add `mazeGame`, marshal a record,
`saveRound` / `loadRoundInto` / `clearRound`.

## Scope

Persist the **board, the ruleset, and all mutable state** — cycle, positions, held
and dropped keys, sprung traps, revealed/frontier sets, queues, deadline,
placements.

This issue originally said to store the seed alone and regenerate the board on
load. That was corrected during implementation; see the PRD and the comment at
the top of `internal/maze/persist.go` for why.

On boot, restore an in-flight round and resume ticking from the persisted cycle
against the persisted deadline.

## Notes

Match the existing convention: persistence failures are logged, never returned —
a live round must not die because the store hiccuped. A record that fails to
unmarshal is dropped, not resurrected.

## Comments

Built as `internal/maze/persist.go`: `RoundState` (JSON-serialisable), plus
`(*Round).State()` and `Restore(RoundState) (*Round, error)`. Bot-side, `bot/rounds.go`
gains `mazeGame`; the save/load call sites land with the Router-side round object
in issue 04. No store migration is needed — `game_rounds` is already a generic
per-game row with an opaque document.

**The seed-only plan was wrong and is reversed.** Regeneration reproduces the
original board only if the generator config *and* code are byte-identical to when
the round started, and neither survives a `maze.toml` edit or a redeploy. The
failure would be silent (players resumed inside walls) and would strike exactly
when someone deploys mid-stream. The board is a few hundred bytes; that was never
a saving worth a failure mode.

**Enums are stored as strings, never as their iota values**, and cells as chat
coordinates ("C4"). Inserting a new `Phase` or `EndReason` constant later would
otherwise silently reinterpret every stored round — a bug that only appears in
production and cannot be diagnosed from the record. It also means a stored round
reads cleanly in psql, which is the same reason the schema keeps `room_id` and
`ends_at` as real columns.

`Restore` validates hard, including that the doubled wall storage agrees with
itself on both sides of every wall. Twenty corrupt-record cases are covered. Each
rejection is a deliberate choice to lose one round rather than resume a game whose
state cannot be trusted and then pay out on it — matching the convention already
set in `bot/rounds.go`.

### On the tests, because two of them were initially wrong

`TestRestoredRoundPlaysIdentically` restores at *every* cycle and compares one
tick, and additionally restores once after the first finish and plays through to
the end. Both halves are needed: the per-cycle probe catches state dropped at a
particular moment, the long continuation catches state whose effect lands several
cycles later (the placement-window deadline is set on the winning cycle and not
read again until the window closes).

It drives play **greedily, not randomly**. The first version used random walkers
and passed while persistence dropped both the submission timestamps and the
placement deadline — random players reach the exit in about one round in a
hundred, so it almost never saw a finish, a placement window or a contested key.

`assertSameRound` compares through the **public surface**, not `State()`.
Comparing `State()` to `State()` is self-referential: anything `State()` forgets
is equally absent from both sides. Fog exposed this — frontier cells affect only
rendering, so neither a `State()` comparison nor any gameplay divergence test can
notice them going missing.

All thirteen persisted fields were verified by deliberately dropping each one and
confirming the suite fails. If you add a field to `RoundState`, do that too — it
is the only check that the tests are actually watching it.
