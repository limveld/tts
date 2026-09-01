# 01 — Seeded map generator

Status: resolved
Blocked by: —

Pure, dependency-free package: seed in, map out. No bot, store or overlay deps,
so it is testable in isolation and is the right first thing to build.

## Scope

`internal/maze` (or `bot/maze/`): `Generate(seed int64, cfg GenConfig) (Map, error)`.

`Map` holds a `size x size` array of 4-bit wall masks (N/E/S/W), plus start,
exit, key cells, trap cells (typed spike/bear), and an optional internal door
slot (unused in v1, see PRD "out of scope").

## Pipeline

1. **Carve** — recursive backtracker over `size x size` cells.
2. **Braid** — remove random interior walls until start and exit are
   **2-vertex-connected**, then remove `loop_walls` more.
3. **Place constructively** from one BFS distance field rooted at start:
   - exit = argmax distance
   - keys = `max_seats - 1` cells in the equidistant band
     `[key_band_min, key_band_max]`, fanned into distinct directions
   - traps = cells on a shortest path from start to a key or the exit, excluding
     start/exit/key cells and cells adjacent to start

## Tests

- Same seed produces a byte-identical map (this is what makes restart-resume in
  issue 03 work without persisting the map).
- Every cell is reachable from start.
- Start and exit have two vertex-disjoint paths (the decision-5 postcondition).
- Exit is at maximum BFS distance from start.
- All keys fall inside the configured band; no key on start/exit.
- Every trap lies on some shortest path to an objective — this is what stops the
  mechanic silently no-opping (PRD decision 6).
- Fuzz across many seeds and both `map_size` values; no panics, all invariants hold.

## Notes

Do **not** add a "no trap blocks the only route" check. Since spikes no longer
kill and traps despawn after one trip, no trap can make a round unwinnable.

## Comments

Built as `internal/maze` (package `maze`), alongside `internal/partition` as the
repo's other pure, dependency-free package. `Generate(seed, Config) (*Map, error)`
plus `Distances`, `Walls`, `Open`, `Neighbor` for the engine to consume.

Every test listed above is in place and passing, over 300-500 seed sweeps at both
board sizes, plus config validation, degenerate configs, and both wall-storage
invariants (walls agree on both sides; the outer boundary is never carved).

Two things worth carrying forward:

- **Pipeline ordering differs from the PRD's original prose**, and the PRD has
  been amended to match. `loop_walls` removals run first; the exit and the
  two-route guarantee are then settled together in a fixpoint loop, because
  fixing either first leaves the other only accidentally true. See PRD decision 5.

- **`TestCutVertexDetectsPerfectMaze` is the load-bearing one.** A cut-vertex
  checker that always answered "none" would make `TestTwoDisjointRoutes` pass
  silently, so the checker is also run against a freshly carved unbraided maze —
  a tree, where every internal cell on a route must be a cut. Do not delete it.

`Map.Door` exists and is always nil: the internal shortcut door is out of scope
for v1, but the board format carries it so adding it later is a generator change
rather than a format change.

Not yet done, and deliberately: nothing reads `maze.toml` yet (issue 09), so the
generator's Config is populated by its caller. `render()` in the test file is the
same wall-mask-to-wall-tile expansion the overlay will do in JS (issue 06) and is
worth reading before writing that.
