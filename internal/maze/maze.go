// Package maze is the Torch Maze chat game's board and its round engine. This
// file is the generator; round.go is the engine that plays a board out. Neither
// half knows anything about chat, the overlay or the store — a round is driven
// by Join, Submit and Tick, and reports what happened as Events.
//
// A board is a grid of cells carrying wall bitmasks, not a grid of wall tiles.
// The overlay expands it into chunky blocks for the 8-bit look, but nothing back
// here reasons about wall tiles: the generator carves cells, BFS validates
// cells, fog reveals cells, and A1-F6 coordinates address cells. That split is
// what lets this package be tested against a 36-element array instead of a
// bitmap.
//
// Generate is a pure function of (seed, Config) that never rejects and never
// retries, which is deliberate on two counts. The round record persists only the
// seed plus the mutable state and regenerates the board on restart, so a
// generator that could return a different map for the same seed would silently
// corrupt a resumed round. And placement is constructive rather than
// rejection-sampled: with a start, an exit, four keys and three traps on 36
// cells roughly a quarter of the board is special, so a reject-and-reroll loop
// either runs unbounded or gives up and emits a map violating its own
// constraints — the worst outcome, because it is rare and therefore untested.
//
// See .scratch/torch-maze/PRD.md for the decisions behind the numbers.
package maze

import (
	"fmt"
	"math/rand"
	"strconv"
)

// Dir is one of the four orthogonal steps between adjacent cells.
type Dir uint8

const (
	North Dir = iota
	East
	South
	West
)

var dirs = [4]Dir{North, East, South, West}

// delta is the (dx, dy) step per direction. Y grows downward — row 0 is the top
// row, matching how the overlay draws it — so North is -1 on Y.
var delta = [4][2]int{
	North: {0, -1},
	East:  {1, 0},
	South: {0, 1},
	West:  {-1, 0},
}

func (d Dir) opposite() Dir { return (d + 2) % 4 }

// bit is d's position in a cell's wall mask. A set bit is a wall.
func (d Dir) bit() uint8 { return 1 << uint(d) }

// allWalls is a cell with every side closed, the state every cell starts in.
const allWalls uint8 = 1<<4 - 1

// Cell is a board position. X is the column, Y the row, both zero-based.
type Cell struct{ X, Y int }

// String is the chat-facing coordinate — column letter, one-based row ("C4").
// Callouts name locations with it, so it has to match the labels the overlay
// draws on the board.
func (c Cell) String() string { return string(rune('A'+c.X)) + strconv.Itoa(c.Y+1) }

// TrapKind distinguishes the two hazards. Both spring once and then despawn, so
// neither can ever make a round unwinnable — which is why placement carries no
// solvability constraint at all.
type TrapKind uint8

const (
	// Spike flings the player back to the start tile and drops any held key on
	// the spike cell. It does not eliminate.
	Spike TrapKind = iota
	// BearTrap immobilises the player for a few cycles. A held key stays held.
	BearTrap
)

func (k TrapKind) String() string {
	if k == Spike {
		return "spike"
	}
	return "bear"
}

// Trap is one hazard and where it sits.
type Trap struct {
	At   Cell
	Kind TrapKind
}

// Config is the generator's half of maze.toml.
type Config struct {
	Size      int // cells per side
	LoopWalls int // interior walls removed for openness, beyond the two-route guarantee

	// Keys is how many key slots to place. The generator always places the
	// round's maximum (max_seats-1); the engine removes the surplus when seats
	// lock and the real head count is known, before keys are ever rendered.
	Keys       int
	KeyBandMin int // keys sit in this BFS-distance band from start
	KeyBandMax int

	Spikes    int
	BearTraps int
}

func (c Config) validate() error {
	switch {
	case c.Size < 3:
		return fmt.Errorf("size %d: need at least 3", c.Size)
	case c.Keys < 0:
		return fmt.Errorf("keys %d: negative", c.Keys)
	case c.Spikes < 0 || c.BearTraps < 0:
		return fmt.Errorf("traps %d/%d: negative", c.Spikes, c.BearTraps)
	case c.KeyBandMin < 1:
		return fmt.Errorf("key band min %d: must be at least 1", c.KeyBandMin)
	case c.KeyBandMax < c.KeyBandMin:
		return fmt.Errorf("key band %d-%d: inverted", c.KeyBandMin, c.KeyBandMax)
	}
	if want := c.Keys + c.Spikes + c.BearTraps + 2; want > c.Size*c.Size {
		return fmt.Errorf("size %d holds %d cells, placement needs %d", c.Size, c.Size*c.Size, want)
	}
	return nil
}

// Map is a generated board. Everything on it is fixed for the round: the mutable
// state (who holds which key, which traps have sprung, what has been revealed)
// belongs to the engine, so a Map can be regenerated from its seed rather than
// persisted.
type Map struct {
	Size int
	Seed int64

	walls []uint8 // one mask per cell, indexed y*Size+x; a set bit is a wall

	Start Cell
	Exit  Cell
	Keys  []Cell
	Traps []Trap

	// Door is the optional internal shortcut door. v1 never places one, but the
	// field exists so the board format and the renderer already carry the
	// concept — adding it later is then a generator change, not a format change.
	Door *Cell
}

// Generate builds the board for seed. The same seed and config always produce
// the same board; that is load-bearing for restart-resume, not a convenience.
func Generate(seed int64, cfg Config) (*Map, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	rnd := rand.New(rand.NewSource(seed))

	m := &Map{Size: cfg.Size, Seed: seed, walls: make([]uint8, cfg.Size*cfg.Size)}
	for i := range m.walls {
		m.walls[i] = allWalls
	}

	m.carve(rnd)
	m.Start = Cell{rnd.Intn(m.Size), rnd.Intn(m.Size)}
	m.braid(rnd, cfg.LoopWalls)
	m.Exit = m.guaranteeTwoRoutes(rnd)

	dist := m.Distances(m.Start)
	m.Keys = m.placeKeys(rnd, dist, cfg.Keys, cfg.KeyBandMin, cfg.KeyBandMax)
	m.Traps = m.placeTraps(rnd, dist, cfg.Spikes, cfg.BearTraps)
	return m, nil
}

// --- grid primitives --------------------------------------------------------

func (m *Map) idx(c Cell) int { return c.Y*m.Size + c.X }

func (m *Map) cellAt(i int) Cell { return Cell{i % m.Size, i / m.Size} }

// InBounds reports whether c is on the board.
func (m *Map) InBounds(c Cell) bool {
	return c.X >= 0 && c.Y >= 0 && c.X < m.Size && c.Y < m.Size
}

// Neighbor is the cell one step in direction d, whether or not a wall separates
// them. ok is false at the board edge.
func (m *Map) Neighbor(c Cell, d Dir) (n Cell, ok bool) {
	n = Cell{c.X + delta[d][0], c.Y + delta[d][1]}
	return n, m.InBounds(n)
}

// Walls is c's wall mask — the bit for each direction that is closed. The engine
// hands this to the overlay when a cell is revealed, and reads it to answer
// "which way can this player go".
func (m *Map) Walls(c Cell) uint8 { return m.walls[m.idx(c)] }

// Open reports whether a player standing on c can step in direction d.
func (m *Map) Open(c Cell, d Dir) bool {
	if _, ok := m.Neighbor(c, d); !ok {
		return false
	}
	return m.walls[m.idx(c)]&d.bit() == 0
}

// openWall knocks out the wall between c and its neighbour in direction d, on
// both cells. Wall state is stored twice — once per side — so every read is a
// single array lookup; keeping the two copies in step is this function's whole
// job, and nothing else may touch m.walls.
func (m *Map) openWall(c Cell, d Dir) {
	n, ok := m.Neighbor(c, d)
	if !ok {
		return
	}
	m.walls[m.idx(c)] &^= d.bit()
	m.walls[m.idx(n)] &^= d.opposite().bit()
}

// --- carve ------------------------------------------------------------------

// carve cuts a perfect maze with a recursive backtracker, iteratively so a large
// board can't blow the stack. The backtracker is chosen for its long winding
// corridors; the flip side is that it leaves very few dead ends (~10% of cells),
// which is exactly why braiding here is a connectivity guarantee rather than the
// "percentage of dead ends removed" dial the design originally called for — on
// 36 cells that percentage would have had nothing to act on.
func (m *Map) carve(rnd *rand.Rand) {
	visited := make([]bool, len(m.walls))
	start := Cell{rnd.Intn(m.Size), rnd.Intn(m.Size)}
	visited[m.idx(start)] = true
	stack := []Cell{start}

	var candidates []Dir
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		candidates = candidates[:0]
		for _, d := range dirs {
			if n, ok := m.Neighbor(cur, d); ok && !visited[m.idx(n)] {
				candidates = append(candidates, d)
			}
		}
		if len(candidates) == 0 {
			stack = stack[:len(stack)-1]
			continue
		}
		d := candidates[rnd.Intn(len(candidates))]
		n, _ := m.Neighbor(cur, d)
		m.openWall(cur, d)
		visited[m.idx(n)] = true
		stack = append(stack, n)
	}
}

// --- braid ------------------------------------------------------------------

// closedWalls lists every interior wall still standing, as (cell, direction)
// pairs counted once each: only North and West are considered, so the same wall
// isn't offered twice from opposite sides. The order is positional and therefore
// stable, which is what keeps a seeded pick reproducible.
func (m *Map) closedWalls() []struct {
	C Cell
	D Dir
} {
	var out []struct {
		C Cell
		D Dir
	}
	for i := range m.walls {
		c := m.cellAt(i)
		for _, d := range [2]Dir{North, West} {
			if _, ok := m.Neighbor(c, d); !ok {
				continue
			}
			if m.walls[i]&d.bit() != 0 {
				out = append(out, struct {
					C Cell
					D Dir
				}{c, d})
			}
		}
	}
	return out
}

// braid removes n interior walls at random, preferring walls that open a dead
// end. This is openness for its own sake — the two-route guarantee is a separate
// step — and it runs first because removing walls can only shorten distances,
// so doing it before the exit is chosen keeps the exit a true farthest cell.
func (m *Map) braid(rnd *rand.Rand, n int) {
	for ; n > 0; n-- {
		walls := m.closedWalls()
		if len(walls) == 0 {
			return
		}
		// Prefer a wall on a dead end: those removals create the alternate
		// routes that matter, where an arbitrary wall may just widen a junction.
		var preferred []int
		for i, w := range walls {
			if m.openings(w.C) == 1 {
				preferred = append(preferred, i)
			}
			if nb, ok := m.Neighbor(w.C, w.D); ok && m.openings(nb) == 1 {
				preferred = append(preferred, i)
			}
		}
		pick := walls[rnd.Intn(len(walls))]
		if len(preferred) > 0 {
			pick = walls[preferred[rnd.Intn(len(preferred))]]
		}
		m.openWall(pick.C, pick.D)
	}
}

// openings counts how many directions c can be left by. One means a dead end.
func (m *Map) openings(c Cell) int {
	n := 0
	for _, d := range dirs {
		if m.Open(c, d) {
			n++
		}
	}
	return n
}

// guaranteeTwoRoutes settles the exit and the alternate-route guarantee
// together, and returns the exit.
//
// The two can't be settled separately because each depends on the other. The
// exit is the farthest cell from start, which moves whenever a wall comes down;
// the guarantee — two vertex-disjoint routes from start to exit — is defined
// against whichever cell ends up being the exit. Fixing one first leaves the
// other only accidentally true. So this iterates: pick the exit, look for a cell
// whose removal would cut it off from start, open a wall around that cell, and
// pick again.
//
// It terminates because every iteration removes a wall and a grid with no
// interior walls has no cut vertices.
func (m *Map) guaranteeTwoRoutes(rnd *rand.Rand) Cell {
	for {
		dist := m.Distances(m.Start)
		exit := m.farthest(rnd, dist)
		cut, found := m.cutVertex(m.Start, exit)
		if !found {
			return exit
		}
		if !m.bypass(rnd, m.Start, exit, cut) {
			return exit // fully open board; nothing left to remove
		}
	}
}

// farthest is the cell at maximum distance from the root of dist, ties broken by
// a seeded pick so the exit isn't biased toward one corner of the board.
func (m *Map) farthest(rnd *rand.Rand, dist []int) Cell {
	best, tied := -1, []int(nil)
	for i, d := range dist {
		switch {
		case d > best:
			best, tied = d, []int{i}
		case d == best:
			tied = append(tied, i)
		}
	}
	return m.cellAt(tied[rnd.Intn(len(tied))])
}

// cutVertex finds a cell other than from and to whose removal disconnects them.
// Two vertex-disjoint routes exist exactly when no such cell does (Menger), so
// this doubles as the guarantee's test.
//
// The scan is naive — one BFS per candidate cell — where Tarjan's articulation
// points would be a single pass. On 36 cells that is a few thousand steps once
// per generated board, and the naive version is the one whose correctness is
// obvious on inspection.
func (m *Map) cutVertex(from, to Cell) (Cell, bool) {
	for i := range m.walls {
		v := m.cellAt(i)
		if v == from || v == to {
			continue
		}
		if m.distancesExcept(from, i)[m.idx(to)] < 0 {
			return v, true
		}
	}
	return Cell{}, false
}

// bypass opens a wall that routes around cut, so from and to stop depending on
// it. It looks for a wall directly joining from's side of cut to to's side —
// that is the removal which actually resolves this cut — and falls back to any
// remaining wall so the caller's loop always makes progress. false means the
// board has no interior walls left.
func (m *Map) bypass(rnd *rand.Rand, from, to, cut Cell) bool {
	side := m.distancesExcept(from, m.idx(cut))
	walls := m.closedWalls()

	var targeted []int
	for i, w := range walls {
		nb, ok := m.Neighbor(w.C, w.D)
		if !ok || w.C == cut || nb == cut {
			continue
		}
		// One end reachable from `from` without cut, the other not: opening this
		// wall bridges the two sides cut was the only link between.
		if (side[m.idx(w.C)] >= 0) != (side[m.idx(nb)] >= 0) {
			targeted = append(targeted, i)
		}
	}
	switch {
	case len(targeted) > 0:
		w := walls[targeted[rnd.Intn(len(targeted))]]
		m.openWall(w.C, w.D)
	case len(walls) > 0:
		w := walls[rnd.Intn(len(walls))]
		m.openWall(w.C, w.D)
	default:
		return false
	}
	return true
}

// --- distances --------------------------------------------------------------

// Distances is the BFS distance field rooted at from: one entry per cell,
// indexed like the board, -1 for unreachable.
func (m *Map) Distances(from Cell) []int { return m.distancesExcept(from, -1) }

// distancesExcept is Distances with one cell treated as removed, which is how
// the cut-vertex scan asks "is to still reachable without v".
func (m *Map) distancesExcept(from Cell, blocked int) []int {
	dist := make([]int, len(m.walls))
	for i := range dist {
		dist[i] = -1
	}
	if fi := m.idx(from); fi != blocked {
		dist[fi] = 0
		queue := []Cell{from}
		for len(queue) > 0 {
			c := queue[0]
			queue = queue[1:]
			for _, d := range dirs {
				if !m.Open(c, d) {
					continue
				}
				n, _ := m.Neighbor(c, d)
				ni := m.idx(n)
				if ni == blocked || dist[ni] >= 0 {
					continue
				}
				dist[ni] = dist[m.idx(c)] + 1
				queue = append(queue, n)
			}
		}
	}
	return dist
}

// --- placement --------------------------------------------------------------

// placeKeys puts n keys in an equal-distance band around the start, spread as
// far from each other as that band allows.
//
// Both halves matter and they are not the same thing. Equal distance from start
// is what makes the shortfall a scramble: with keys at varied distances the
// nearest ones go first and whoever picked a far one loses to geometry rather
// than to racing. Spreading them within the band is angular separation, so the
// field fans out across the board instead of stampeding one corridor — it does
// not reintroduce the varied-distance problem, because the band pins the
// distance.
func (m *Map) placeKeys(rnd *rand.Rand, dist []int, n, bandMin, bandMax int) []Cell {
	if n <= 0 {
		return nil
	}
	taken := map[int]bool{m.idx(m.Start): true, m.idx(m.Exit): true}

	// Widen the band outward until it holds enough cells. A 6x6 board with a
	// tight band and an awkward start can otherwise come up short, and this
	// generator does not get to fail.
	var pool []int
	for lo, hi := bandMin, bandMax; ; lo, hi = lo-1, hi+1 {
		pool = pool[:0]
		for i, d := range dist {
			if d >= lo && d <= hi && !taken[i] {
				pool = append(pool, i)
			}
		}
		if len(pool) >= n || (lo <= 1 && hi >= m.Size*m.Size) {
			break
		}
	}
	if len(pool) == 0 {
		return nil
	}

	keys := make([]Cell, 0, n)
	first := pool[rnd.Intn(len(pool))]
	keys = append(keys, m.cellAt(first))
	taken[first] = true

	for len(keys) < n {
		best, tied := -1, []int(nil)
		for _, i := range pool {
			if taken[i] {
				continue
			}
			near := m.Size * m.Size
			for _, k := range keys {
				if d := manhattan(m.cellAt(i), k); d < near {
					near = d
				}
			}
			switch {
			case near > best:
				best, tied = near, []int{i}
			case near == best:
				tied = append(tied, i)
			}
		}
		if len(tied) == 0 {
			break
		}
		pick := tied[rnd.Intn(len(tied))]
		keys = append(keys, m.cellAt(pick))
		taken[pick] = true
	}
	return keys
}

func manhattan(a, b Cell) int { return abs(a.X-b.X) + abs(a.Y-b.Y) }

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// placeTraps puts hazards on cells that lie on a shortest path from the start to
// a key or to the exit.
//
// Uniform scattering would waste the mechanic. With objectives visible from the
// first cycle players walk near-optimal routes covering maybe a dozen of the
// board's cells, so most of a uniformly placed set would sit in corners nobody
// has a reason to enter and would never fire at all. Cells adjacent to the start
// are excluded so nobody is punished before they have made a real decision.
func (m *Map) placeTraps(rnd *rand.Rand, dist []int, spikes, bears int) []Trap {
	want := spikes + bears
	if want <= 0 {
		return nil
	}
	off := map[int]bool{m.idx(m.Start): true, m.idx(m.Exit): true}
	for _, k := range m.Keys {
		off[m.idx(k)] = true
	}

	objectives := append([]Cell{m.Exit}, m.Keys...)
	onRoute := make([]bool, len(m.walls))
	for _, o := range objectives {
		to := m.Distances(o)
		total := dist[m.idx(o)]
		if total < 0 {
			continue
		}
		for i := range onRoute {
			if dist[i] >= 0 && to[i] >= 0 && dist[i]+to[i] == total {
				onRoute[i] = true
			}
		}
	}

	pool := make([]int, 0, len(m.walls))
	for i := range onRoute {
		if onRoute[i] && !off[i] && dist[i] > 1 {
			pool = append(pool, i)
		}
	}
	// Fall back to any cell a player might plausibly cross. Only a degenerate
	// board gets here, but "generator never fails" has to hold on those too.
	if len(pool) < want {
		for i := range m.walls {
			if !off[i] && dist[i] > 1 {
				pool = append(pool, i)
			}
		}
	}

	rnd.Shuffle(len(pool), func(a, b int) { pool[a], pool[b] = pool[b], pool[a] })
	traps := make([]Trap, 0, want)
	seen := map[int]bool{}
	for _, i := range pool {
		if len(traps) == want {
			break
		}
		if seen[i] {
			continue
		}
		seen[i] = true
		kind := Spike
		if len(traps) >= spikes {
			kind = BearTrap
		}
		traps = append(traps, Trap{At: m.cellAt(i), Kind: kind})
	}
	return traps
}
