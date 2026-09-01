package maze

import (
	"math/rand"
	"sort"
	"strings"
	"testing"
)

// testConfig is the shipping 6x6 board: four key slots (max_seats-1), two spikes
// and one bear trap. Most tests sweep seeds against it, because the invariants
// that matter are the ones that have to hold on every board the game will ever
// deal, not on one hand-picked seed.
func testConfig() Config {
	return Config{
		Size:       6,
		LoopWalls:  4,
		Keys:       4,
		KeyBandMin: 4,
		KeyBandMax: 6,
		Spikes:     2,
		BearTraps:  1,
	}
}

const sweep = 300 // seeds per invariant

func mustGenerate(t *testing.T, seed int64, cfg Config) *Map {
	t.Helper()
	m, err := Generate(seed, cfg)
	if err != nil {
		t.Fatalf("Generate(%d): %v", seed, err)
	}
	return m
}

// TestGenerateIsDeterministic is the load-bearing one: the round record stores
// only the seed and regenerates the board on restart, so a seed that produced a
// different board on the second call would silently corrupt a resumed round
// rather than fail loudly.
func TestGenerateIsDeterministic(t *testing.T) {
	cfg := testConfig()
	for seed := int64(0); seed < sweep; seed++ {
		a := mustGenerate(t, seed, cfg)
		b := mustGenerate(t, seed, cfg)

		if a.Start != b.Start || a.Exit != b.Exit {
			t.Fatalf("seed %d: start/exit %v/%v vs %v/%v", seed, a.Start, a.Exit, b.Start, b.Exit)
		}
		for i := range a.walls {
			if a.walls[i] != b.walls[i] {
				t.Fatalf("seed %d: wall mask at %v = %04b vs %04b", seed, a.cellAt(i), a.walls[i], b.walls[i])
			}
		}
		if len(a.Keys) != len(b.Keys) {
			t.Fatalf("seed %d: %d keys vs %d", seed, len(a.Keys), len(b.Keys))
		}
		for i := range a.Keys {
			if a.Keys[i] != b.Keys[i] {
				t.Fatalf("seed %d: key %d at %v vs %v", seed, i, a.Keys[i], b.Keys[i])
			}
		}
		for i := range a.Traps {
			if a.Traps[i] != b.Traps[i] {
				t.Fatalf("seed %d: trap %d %v vs %v", seed, i, a.Traps[i], b.Traps[i])
			}
		}
	}
}

// TestDifferentSeedsDifferBoards guards the other side of determinism: a fixed
// board for every seed would also pass the test above.
func TestDifferentSeedsDifferBoards(t *testing.T) {
	cfg := testConfig()
	seen := map[string]int64{}
	for seed := int64(0); seed < 50; seed++ {
		m := mustGenerate(t, seed, cfg)
		key := string(m.walls) + m.Start.String() + m.Exit.String()
		if prev, dup := seen[key]; dup {
			t.Fatalf("seeds %d and %d produced the same board", prev, seed)
		}
		seen[key] = seed
	}
}

// TestEveryCellReachable checks the carve left a connected board. Braiding only
// removes walls, so a failure here means the backtracker itself is broken.
func TestEveryCellReachable(t *testing.T) {
	cfg := testConfig()
	for seed := int64(0); seed < sweep; seed++ {
		m := mustGenerate(t, seed, cfg)
		dist := m.Distances(m.Start)
		for i, d := range dist {
			if d < 0 {
				t.Fatalf("seed %d: %v unreachable from start %v", seed, m.cellAt(i), m.Start)
			}
		}
	}
}

// TestWallsAgreeBothSides pins the one invariant the doubled wall storage can
// break: every wall is recorded on both of the cells it separates, so "can I
// step east from here" and "can I step west from there" never disagree.
func TestWallsAgreeBothSides(t *testing.T) {
	cfg := testConfig()
	for seed := int64(0); seed < sweep; seed++ {
		m := mustGenerate(t, seed, cfg)
		for i := range m.walls {
			c := m.cellAt(i)
			for _, d := range dirs {
				n, ok := m.Neighbor(c, d)
				if !ok {
					continue
				}
				if m.Open(c, d) != m.Open(n, d.opposite()) {
					t.Fatalf("seed %d: wall between %v and %v disagrees", seed, c, n)
				}
			}
		}
	}
}

// TestBoardEdgesAreWalled checks nothing carved through the outer boundary.
func TestBoardEdgesAreWalled(t *testing.T) {
	cfg := testConfig()
	for seed := int64(0); seed < sweep; seed++ {
		m := mustGenerate(t, seed, cfg)
		for i := range m.walls {
			c := m.cellAt(i)
			for _, d := range dirs {
				if _, ok := m.Neighbor(c, d); ok {
					continue
				}
				if m.walls[i]&d.bit() == 0 {
					t.Fatalf("seed %d: %v is open through the board edge", seed, c)
				}
			}
		}
	}
}

// TestTwoDisjointRoutes is the braid postcondition from PRD decision 5: no
// single cell may sit on every route from start to exit, or the field walks the
// board in single file and overtaking is impossible.
func TestTwoDisjointRoutes(t *testing.T) {
	cfg := testConfig()
	for seed := int64(0); seed < sweep; seed++ {
		m := mustGenerate(t, seed, cfg)
		if cut, found := m.cutVertex(m.Start, m.Exit); found {
			t.Fatalf("seed %d: %v cuts start %v from exit %v%s", seed, cut, m.Start, m.Exit, render(m))
		}
	}
}

// TestExitIsFarthest checks the exit really is a farthest cell on the *finished*
// board, not on the board as it stood before braiding — which is why the exit
// and the two-route guarantee are settled in one loop rather than in sequence.
func TestExitIsFarthest(t *testing.T) {
	cfg := testConfig()
	for seed := int64(0); seed < sweep; seed++ {
		m := mustGenerate(t, seed, cfg)
		dist := m.Distances(m.Start)
		best := 0
		for _, d := range dist {
			if d > best {
				best = d
			}
		}
		if got := dist[m.idx(m.Exit)]; got != best {
			t.Fatalf("seed %d: exit %v at distance %d, farthest is %d", seed, m.Exit, got, best)
		}
		if m.Exit == m.Start {
			t.Fatalf("seed %d: exit sits on the start cell", seed)
		}
	}
}

// TestKeysAreEquidistant is the scramble guarantee from the Q13 review: keys sit
// in one distance band so the field arrives together and the shortfall is raced
// for, rather than decided by whoever happened to pick the nearest one.
func TestKeysAreEquidistant(t *testing.T) {
	cfg := testConfig()
	for seed := int64(0); seed < sweep; seed++ {
		m := mustGenerate(t, seed, cfg)
		if len(m.Keys) != cfg.Keys {
			t.Fatalf("seed %d: %d keys, want %d", seed, len(m.Keys), cfg.Keys)
		}
		dist := m.Distances(m.Start)
		lo, hi := m.Size*m.Size, 0
		for _, k := range m.Keys {
			d := dist[m.idx(k)]
			if d < lo {
				lo = d
			}
			if d > hi {
				hi = d
			}
		}
		// The band widens only when a tight one can't be filled, so allow slack
		// but not an arbitrary spread — a band wider than the configured one
		// means keys at meaningfully different distances.
		if spread, want := hi-lo, cfg.KeyBandMax-cfg.KeyBandMin+2; spread > want {
			t.Fatalf("seed %d: key distances span %d (%d..%d), want at most %d", seed, spread, lo, hi, want)
		}
	}
}

// TestKeysAreDistinctAndClear checks keys don't stack or land on the start or
// the exit — a key on the start would hand one out for free, and one on the exit
// would be unreachable without the key it is supposed to provide.
func TestKeysAreDistinctAndClear(t *testing.T) {
	cfg := testConfig()
	for seed := int64(0); seed < sweep; seed++ {
		m := mustGenerate(t, seed, cfg)
		seen := map[Cell]bool{}
		for _, k := range m.Keys {
			if seen[k] {
				t.Fatalf("seed %d: two keys on %v", seed, k)
			}
			seen[k] = true
			if k == m.Start || k == m.Exit {
				t.Fatalf("seed %d: key on %v (start %v, exit %v)", seed, k, m.Start, m.Exit)
			}
		}
	}
}

// TestTrapsSitOnTravelledRoutes is what stops the hazard mechanic silently
// no-opping. Players walk near-optimal paths to visible objectives, so a trap
// off every shortest path is one that will very likely never fire.
func TestTrapsSitOnTravelledRoutes(t *testing.T) {
	cfg := testConfig()
	for seed := int64(0); seed < sweep; seed++ {
		m := mustGenerate(t, seed, cfg)
		if want := cfg.Spikes + cfg.BearTraps; len(m.Traps) != want {
			t.Fatalf("seed %d: %d traps, want %d", seed, len(m.Traps), want)
		}
		fromStart := m.Distances(m.Start)
		for _, tr := range m.Traps {
			onRoute := false
			for _, o := range append([]Cell{m.Exit}, m.Keys...) {
				toObj := m.Distances(o)
				if fromStart[m.idx(tr.At)]+toObj[m.idx(tr.At)] == fromStart[m.idx(o)] {
					onRoute = true
					break
				}
			}
			if !onRoute {
				t.Fatalf("seed %d: trap at %v is on no shortest path to an objective%s", seed, tr.At, render(m))
			}
		}
	}
}

// TestTrapsAreDistinctAndClear checks traps don't stack, don't sit on an
// objective, and never sit next to the start — nobody should be punished before
// they have made a real decision.
func TestTrapsAreDistinctAndClear(t *testing.T) {
	cfg := testConfig()
	for seed := int64(0); seed < sweep; seed++ {
		m := mustGenerate(t, seed, cfg)
		dist := m.Distances(m.Start)
		occupied := map[Cell]bool{m.Start: true, m.Exit: true}
		for _, k := range m.Keys {
			occupied[k] = true
		}
		for _, tr := range m.Traps {
			if occupied[tr.At] {
				t.Fatalf("seed %d: trap at %v collides with start/exit/key", seed, tr.At)
			}
			occupied[tr.At] = true
			if d := dist[m.idx(tr.At)]; d <= 1 {
				t.Fatalf("seed %d: trap at %v is %d from start", seed, tr.At, d)
			}
		}
	}
}

// TestTrapKindSplit checks the configured mix, which is the comeback dial: spikes
// are the only hazard that returns a key to the board.
func TestTrapKindSplit(t *testing.T) {
	cfg := testConfig()
	for seed := int64(0); seed < sweep; seed++ {
		m := mustGenerate(t, seed, cfg)
		var spikes, bears int
		for _, tr := range m.Traps {
			if tr.Kind == Spike {
				spikes++
			} else {
				bears++
			}
		}
		if spikes != cfg.Spikes || bears != cfg.BearTraps {
			t.Fatalf("seed %d: %d spikes/%d bears, want %d/%d", seed, spikes, bears, cfg.Spikes, cfg.BearTraps)
		}
	}
}

// TestSizeSeven covers the other shipping board size against every invariant that
// isn't size-specific.
func TestSizeSeven(t *testing.T) {
	cfg := testConfig()
	cfg.Size = 7
	cfg.KeyBandMax = 7
	for seed := int64(0); seed < sweep; seed++ {
		m := mustGenerate(t, seed, cfg)
		if cut, found := m.cutVertex(m.Start, m.Exit); found {
			t.Fatalf("seed %d: %v cuts start from exit", seed, cut)
		}
		for i, d := range m.Distances(m.Start) {
			if d < 0 {
				t.Fatalf("seed %d: %v unreachable", seed, m.cellAt(i))
			}
		}
		if len(m.Keys) != cfg.Keys {
			t.Fatalf("seed %d: %d keys, want %d", seed, len(m.Keys), cfg.Keys)
		}
	}
}

// TestDegenerateConfigs checks the generator holds up on boards the game will
// never deal but a mistyped maze.toml might.
func TestDegenerateConfigs(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
	}{
		{"no keys or traps", Config{Size: 6, KeyBandMin: 1, KeyBandMax: 2}},
		{"smallest board", Config{Size: 3, LoopWalls: 0, Keys: 1, KeyBandMin: 1, KeyBandMax: 2, Spikes: 1}},
		{"packed board", Config{Size: 3, LoopWalls: 2, Keys: 4, KeyBandMin: 1, KeyBandMax: 2, Spikes: 2, BearTraps: 1}},
		{"no loop walls", Config{Size: 6, Keys: 4, KeyBandMin: 4, KeyBandMax: 6, Spikes: 2, BearTraps: 1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for seed := int64(0); seed < 100; seed++ {
				m := mustGenerate(t, seed, tc.cfg)
				if cut, found := m.cutVertex(m.Start, m.Exit); found {
					t.Fatalf("seed %d: %v cuts start from exit", seed, cut)
				}
				if len(m.Keys) > tc.cfg.Keys {
					t.Fatalf("seed %d: %d keys, want at most %d", seed, len(m.Keys), tc.cfg.Keys)
				}
			}
		})
	}
}

func TestConfigValidation(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
	}{
		{"tiny board", Config{Size: 2, KeyBandMin: 1, KeyBandMax: 2}},
		{"negative keys", Config{Size: 6, Keys: -1, KeyBandMin: 1, KeyBandMax: 2}},
		{"negative traps", Config{Size: 6, Spikes: -1, KeyBandMin: 1, KeyBandMax: 2}},
		{"zero band", Config{Size: 6, KeyBandMin: 0, KeyBandMax: 2}},
		{"inverted band", Config{Size: 6, KeyBandMin: 5, KeyBandMax: 2}},
		{"placement exceeds board", Config{Size: 3, Keys: 20, KeyBandMin: 1, KeyBandMax: 2}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Generate(1, tc.cfg); err == nil {
				t.Fatal("want an error, got nil")
			}
		})
	}
}

func TestCellString(t *testing.T) {
	cases := []struct {
		c    Cell
		want string
	}{
		{Cell{0, 0}, "A1"},
		{Cell{2, 3}, "C4"},
		{Cell{5, 5}, "F6"},
	}
	for _, tc := range cases {
		if got := tc.c.String(); got != tc.want {
			t.Errorf("Cell%v.String()=%q want %q", tc.c, got, tc.want)
		}
	}
}

func TestDirOpposite(t *testing.T) {
	for _, d := range dirs {
		if got := d.opposite().opposite(); got != d {
			t.Errorf("%d.opposite().opposite()=%d want %d", d, got, d)
		}
		if d.opposite() == d {
			t.Errorf("%d is its own opposite", d)
		}
	}
}

// TestCutVertexDetectsPerfectMaze keeps TestTwoDisjointRoutes honest. A checker
// that always answered "no cut vertex" would pass that test silently, so this
// runs it against a freshly carved, unbraided maze: that is a tree, every route
// between two cells is unique, and so any cell between them must be a cut. If
// this stops finding one, the guarantee elsewhere means nothing.
func TestCutVertexDetectsPerfectMaze(t *testing.T) {
	cfg := testConfig()
	for seed := int64(0); seed < 100; seed++ {
		m := &Map{Size: cfg.Size, Seed: seed, walls: make([]uint8, cfg.Size*cfg.Size)}
		for i := range m.walls {
			m.walls[i] = allWalls
		}
		m.carve(newTestRand(seed))
		m.Start = Cell{0, 0}
		dist := m.Distances(m.Start)
		far := m.cellAt(0)
		for i, d := range dist {
			if d > dist[m.idx(far)] {
				far = m.cellAt(i)
			}
		}
		if _, found := m.cutVertex(m.Start, far); !found {
			t.Fatalf("seed %d: no cut vertex in a perfect maze between %v and %v", seed, m.Start, far)
		}
	}
}

func newTestRand(seed int64) *rand.Rand { return rand.New(rand.NewSource(seed)) }

// render draws a board as ASCII. It exists for failure messages: an invariant
// that broke on seed 231 is nearly unreadable as a list of cells and obvious as
// a picture, and it is the same expansion from wall masks to wall tiles the
// overlay renderer does.
func render(m *Map) string {
	mark := map[Cell]string{m.Start: "S", m.Exit: "X"}
	for _, k := range m.Keys {
		mark[k] = "k"
	}
	for _, tr := range m.Traps {
		if tr.Kind == Spike {
			mark[tr.At] = "^"
		} else {
			mark[tr.At] = "b"
		}
	}
	var b strings.Builder
	b.WriteByte('\n')
	for y := 0; y < m.Size; y++ {
		for x := 0; x < m.Size; x++ {
			if m.Open(Cell{x, y}, North) {
				b.WriteString("+   ")
			} else {
				b.WriteString("+---")
			}
		}
		b.WriteString("+\n")
		for x := 0; x < m.Size; x++ {
			if m.Open(Cell{x, y}, West) {
				b.WriteString(" ")
			} else {
				b.WriteString("|")
			}
			if s, ok := mark[Cell{x, y}]; ok {
				b.WriteString(" " + s + " ")
			} else {
				b.WriteString("   ")
			}
		}
		b.WriteString("|\n")
	}
	b.WriteString(strings.Repeat("+---", m.Size) + "+\n")
	return b.String()
}

// TestBoardPacing guards the assumption the round-length config rests on. The
// 8-second tick and the 36-cycle cap were chosen against a board whose exit sits
// roughly a dozen steps away and whose nearest key is a short detour; a change
// that quietly collapsed either number would leave the game tuned for a board it
// no longer generates, and every other test here would still pass.
func TestBoardPacing(t *testing.T) {
	cfg := testConfig()
	var exitDist, keyDist []int
	for seed := int64(0); seed < 500; seed++ {
		m := mustGenerate(t, seed, cfg)
		d := m.Distances(m.Start)

		got := d[m.idx(m.Exit)]
		if got < 5 {
			t.Fatalf("seed %d: exit only %d steps from start — round would be over instantly%s",
				seed, got, render(m))
		}
		exitDist = append(exitDist, got)

		near := m.Size * m.Size
		for _, k := range m.Keys {
			if d[m.idx(k)] < near {
				near = d[m.idx(k)]
			}
		}
		if near < cfg.KeyBandMin-1 || near > cfg.KeyBandMax+1 {
			t.Fatalf("seed %d: nearest key %d steps out, band is %d-%d%s",
				seed, near, cfg.KeyBandMin, cfg.KeyBandMax, render(m))
		}
		keyDist = append(keyDist, near)
	}

	sort.Ints(exitDist)
	sort.Ints(keyDist)
	if p50 := exitDist[len(exitDist)/2]; p50 < 8 || p50 > 15 {
		t.Errorf("median exit distance %d, want 8-15 (the tick/cycle config assumes ~11)", p50)
	}
	t.Logf("exit distance   p10=%d p50=%d p90=%d", exitDist[len(exitDist)/10], exitDist[len(exitDist)/2], exitDist[len(exitDist)*9/10])
	t.Logf("nearest key     p10=%d p50=%d p90=%d", keyDist[len(keyDist)/10], keyDist[len(keyDist)/2], keyDist[len(keyDist)*9/10])
}
