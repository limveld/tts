package maze

import (
	"math/rand"
	"sort"
	"testing"
	"time"
)

// openBoard is a board with every interior wall removed, so a move never fails
// for reasons the test didn't set up. Engine rules are about keys, traps, fog
// and timing; letting a generated maze decide which moves are legal would make
// these tests depend on the generator's seed, and a generator change would then
// break the engine suite for no reason.
func openBoard(size int) *Map {
	m := &Map{Size: size, walls: make([]uint8, size*size)}
	for i := range m.walls {
		m.walls[i] = allWalls
	}
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			for _, d := range [2]Dir{East, South} {
				if _, ok := m.Neighbor(Cell{x, y}, d); ok {
					m.openWall(Cell{x, y}, d)
				}
			}
		}
	}
	m.Start = Cell{0, 0}
	m.Exit = Cell{size - 1, size - 1}
	// A board with no key is unwinnable, and the engine rightly ends such a round
	// on its first tick. That would quietly cut short any test not about keys, so
	// every board gets one out of the way in the far corner; tests that care about
	// key placement overwrite this.
	m.Keys = []Cell{{0, size - 1}}
	return m
}

var epoch = time.Unix(1_700_000_000, 0)

func testRoundConfig() RoundConfig {
	c := DefaultRoundConfig()
	c.JoinCycles = 1
	return c
}

// start seats the given users and ticks through the join window, so a test can
// begin at cycle 0 of the race.
func start(t *testing.T, m *Map, cfg RoundConfig, users ...string) *Round {
	t.Helper()
	r := NewRound(m, cfg, epoch)
	for _, u := range users {
		if _, ok := r.Join(u, u, u); !ok {
			t.Fatalf("Join(%s) refused", u)
		}
	}
	for r.Phase == PhaseJoining {
		r.Tick(epoch)
	}
	return r
}

// testDirs is the WASD shorthand the tests write moves in. Chat itself uses
// !up/!down/!left/!right; the engine only ever sees a Dir.
var testDirs = map[string]Dir{"w": North, "a": West, "s": South, "d": East}

func submit(t *testing.T, r *Round, user, dir string, at time.Time) {
	t.Helper()
	d, ok := testDirs[dir]
	if !ok {
		t.Fatalf("test wrote an unknown direction %q", dir)
	}
	if !r.Submit(user, d, at) {
		t.Fatalf("Submit(%s, %q) refused", user, dir)
	}
}

func player(t *testing.T, r *Round, user string) *Player {
	t.Helper()
	p, ok := r.PlayerBy(user)
	if !ok {
		t.Fatalf("%s is not seated", user)
	}
	return p
}

func has(evs []Event, k EventKind, seat int) bool {
	for _, e := range evs {
		if e.Kind == k && e.Seat == seat {
			return true
		}
	}
	return false
}

func find(evs []Event, k EventKind) (Event, bool) {
	for _, e := range evs {
		if e.Kind == k {
			return e, true
		}
	}
	return Event{}, false
}

// heldKeys counts keys in players' hands.
func heldKeys(r *Round) int {
	n := 0
	for _, p := range r.Players {
		if p.HasKey {
			n++
		}
	}
	return n
}

// --- keys -------------------------------------------------------------------

// TestSpikedKeyHolderDropsKey is the resolution of the seam the design argument
// stalled on. A key must not be able to leave play except through the door, or a
// carrier's bad luck can make the round unwinnable for someone else.
func TestSpikedKeyHolderDropsKey(t *testing.T) {
	m := openBoard(4)
	m.Keys = []Cell{{1, 0}}
	m.Traps = []Trap{{At: Cell{2, 0}, Kind: Spike}}
	r := start(t, m, testRoundConfig(), "bob")

	submit(t, r, "bob", "d", epoch)
	r.Tick(epoch)
	if p := player(t, r, "bob"); !p.HasKey || p.At != (Cell{1, 0}) {
		t.Fatalf("after pickup: at=%v hasKey=%v, want B1 with a key", p.At, p.HasKey)
	}

	submit(t, r, "bob", "d", epoch)
	evs := r.Tick(epoch)

	p := player(t, r, "bob")
	if p.HasKey {
		t.Error("spiked player kept the key")
	}
	if p.At != m.Start {
		t.Errorf("at=%v want start %v", p.At, m.Start)
	}
	if keys := r.KeysOnMap(); len(keys) != 1 || keys[0] != (Cell{2, 0}) {
		t.Errorf("keys on map=%v, want the dropped key on C1", keys)
	}
	if !has(evs, EventSpiked, p.Seat) || !has(evs, EventKeyDropped, p.Seat) {
		t.Errorf("events=%v, want spiked + key-dropped", evs)
	}
	if p.Queued() != 0 {
		t.Errorf("queue=%d, want cleared by the spike", p.Queued())
	}
}

// TestKeyCountAtLock covers the whole range of turnouts this channel actually
// sees, including the two-player case the deficit is deliberately not applied to.
func TestKeyCountAtLock(t *testing.T) {
	cases := []struct{ players, want int }{
		{1, 1}, {2, 2}, {3, 2}, {4, 3}, {5, 4},
	}
	for _, tc := range cases {
		m := openBoard(6)
		m.Keys = []Cell{{1, 0}, {2, 0}, {3, 0}, {4, 0}}
		users := make([]string, tc.players)
		for i := range users {
			users[i] = string(rune('a' + i))
		}
		r := start(t, m, testRoundConfig(), users...)
		if got := len(r.KeysOnMap()); got != tc.want {
			t.Errorf("%d players: %d keys, want %d", tc.players, got, tc.want)
		}
	}
}

// TestNoKeysBeforeLock checks the surplus never renders. Keys appearing and then
// vanishing on a board players are already routing toward would be worse than
// keys appearing a beat late.
func TestNoKeysBeforeLock(t *testing.T) {
	m := openBoard(4)
	m.Keys = []Cell{{1, 0}, {2, 0}, {3, 0}, {0, 1}}
	r := NewRound(m, testRoundConfig(), epoch)
	r.Join("bob", "bob", "bob")
	if got := len(r.KeysOnMap()); got != 0 {
		t.Fatalf("%d keys visible during the join window, want 0", got)
	}
	r.Tick(epoch)
	if got := len(r.KeysOnMap()); got != 1 {
		t.Fatalf("%d keys after lock, want 1", got)
	}
}

// TestKeyContentionTieBreak pins the one genuinely contested case in a cycle.
func TestKeyContentionTieBreak(t *testing.T) {
	m := openBoard(4)
	m.Start = Cell{1, 1}
	m.Keys = []Cell{{1, 0}}
	cfg := testRoundConfig()
	cfg.DeficitMinPlayers = 2 // one key for two players, so it is truly contested
	r := start(t, m, cfg, "early", "late")

	if got := len(r.KeysOnMap()); got != 1 {
		t.Fatalf("%d keys, want 1 so the key is contested", got)
	}
	submit(t, r, "late", "w", epoch.Add(2*time.Second))
	submit(t, r, "early", "w", epoch.Add(1*time.Second))
	r.Tick(epoch)

	if e, l := player(t, r, "early"), player(t, r, "late"); !e.HasKey || l.HasKey {
		t.Errorf("early.HasKey=%v late.HasKey=%v, want the earlier submission to win", e.HasKey, l.HasKey)
	}
	if got := len(r.KeysOnMap()); got != 0 {
		t.Errorf("%d keys left on the board, want 0", got)
	}
}

// TestKeyConservation is the strongest guarantee here: across randomly played
// rounds on generated boards, a key is only ever on the board, in a hand, or
// spent at the door — never duplicated and never destroyed. Every trap, drop and
// pickup path has to preserve it, so this catches accounting bugs the
// hand-written cases would miss.
func TestKeyConservation(t *testing.T) {
	rnd := rand.New(rand.NewSource(99))
	for seed := int64(0); seed < 200; seed++ {
		m := mustGenerate(t, seed, testConfig())
		cfg := testRoundConfig()
		r := start(t, m, cfg, "a", "b", "c", "d", "e")

		initial := len(r.KeysOnMap())
		if initial != 4 {
			t.Fatalf("seed %d: %d keys at lock, want 4", seed, initial)
		}

		for r.Phase == PhaseRacing {
			for _, p := range r.Players {
				if !p.Racing() {
					continue
				}
				r.Submit(p.UserID, dirs[rnd.Intn(4)], epoch.Add(time.Duration(rnd.Intn(1000))*time.Millisecond))
			}
			r.Tick(epoch)

			spent := 0
			for _, p := range r.Players {
				if !p.Racing() {
					spent++
				}
			}
			if got := len(r.KeysOnMap()) + heldKeys(r) + spent; got != initial {
				t.Fatalf("seed %d cycle %d: %d on board + %d held + %d spent = %d, want %d",
					seed, r.Cycle, len(r.KeysOnMap()), heldKeys(r), spent, got, initial)
			}
		}
	}
}

// --- traps ------------------------------------------------------------------

// TestBearTrapHoldsExactlyNCycles checks both halves of the rule: the key is not
// lost to immobilisation (that is what "held till the door" was actually aimed
// at), and the counter costs exactly what it says.
func TestBearTrapHoldsExactlyNCycles(t *testing.T) {
	m := openBoard(4)
	m.Keys = []Cell{{1, 0}}
	m.Traps = []Trap{{At: Cell{2, 0}, Kind: BearTrap}}
	cfg := testRoundConfig()
	cfg.BearTrapCycles = 2
	r := start(t, m, cfg, "bob")

	submit(t, r, "bob", "d", epoch) // onto the key
	r.Tick(epoch)
	submit(t, r, "bob", "d", epoch) // and on into the trap
	evs := r.Tick(epoch)

	p := player(t, r, "bob")
	if !p.HasKey {
		t.Error("bear trap stripped the key; immobilising must not cost a key")
	}
	if p.StuckFor != 2 {
		t.Fatalf("StuckFor=%d want 2", p.StuckFor)
	}
	if !has(evs, EventTrapped, p.Seat) {
		t.Errorf("events=%v, want trapped", evs)
	}
	stuckAt := p.At

	// Two cycles of not moving, even with a move queued the whole time.
	for i := 1; i <= 2; i++ {
		submit(t, r, "bob", "s", epoch)
		evs = r.Tick(epoch)
		if p.At != stuckAt {
			t.Fatalf("moved on stuck cycle %d: at=%v want %v", i, p.At, stuckAt)
		}
	}
	if p.StuckFor != 0 || !has(evs, EventFreed, p.Seat) {
		t.Fatalf("StuckFor=%d freed=%v, want 0 and a freed event", p.StuckFor, has(evs, EventFreed, p.Seat))
	}

	// Freed does not move you; the cycle after does.
	r.Tick(epoch)
	if p.At == stuckAt {
		t.Errorf("still at %v the cycle after being freed, want a move", p.At)
	}
}

// TestTrapSpringsOnce is the "sacrificial pathfinder" rule: the first player
// through clears the hazard for everyone behind them.
func TestTrapSpringsOnce(t *testing.T) {
	m := openBoard(4)
	m.Start = Cell{0, 0}
	m.Traps = []Trap{{At: Cell{1, 0}, Kind: Spike}}
	r := start(t, m, testRoundConfig(), "first", "second")

	submit(t, r, "first", "d", epoch)
	r.Tick(epoch)
	if p := player(t, r, "first"); p.At != m.Start {
		t.Fatalf("first player at %v, want to have been flung back to %v", p.At, m.Start)
	}
	if !r.TrapSprung(0) {
		t.Fatal("trap did not spring")
	}

	submit(t, r, "second", "d", epoch)
	evs := r.Tick(epoch)
	p := player(t, r, "second")
	if p.At != (Cell{1, 0}) {
		t.Errorf("second player at %v, want to walk the cleared cell C... A2", p.At)
	}
	if has(evs, EventSpiked, p.Seat) {
		t.Error("a sprung trap fired again")
	}
}

// TestTrapFiresForEverySimultaneousEntrant checks the deliberate choice not to
// let the key tie-break spare a second player: they stepped on it at the same
// instant, so it springs for both and only then despawns.
func TestTrapFiresForEverySimultaneousEntrant(t *testing.T) {
	m := openBoard(4)
	m.Start = Cell{0, 0}
	m.Traps = []Trap{{At: Cell{1, 0}, Kind: Spike}}
	r := start(t, m, testRoundConfig(), "a", "b")

	submit(t, r, "a", "d", epoch.Add(time.Second))
	submit(t, r, "b", "d", epoch.Add(2*time.Second))
	evs := r.Tick(epoch)

	for _, u := range []string{"a", "b"} {
		p := player(t, r, u)
		if !has(evs, EventSpiked, p.Seat) {
			t.Errorf("%s was not spiked; a simultaneous entrant must not be spared", u)
		}
		if p.At != m.Start {
			t.Errorf("%s at %v, want start", u, p.At)
		}
	}
}

// --- movement ---------------------------------------------------------------

// TestOneMessageOneMove covers the correction mechanic: a second command replaces
// the first, so the last thing said before the tick is what happens — and a
// player never banks more than a single cell per cycle however much they type.
func TestOneMessageOneMove(t *testing.T) {
	m := openBoard(6)
	m.Start = Cell{2, 2}
	r := start(t, m, testRoundConfig(), "bob")
	p := player(t, r, "bob")

	submit(t, r, "bob", "s", epoch)
	if p.Queued() != 1 {
		t.Fatalf("Queued=%d want 1", p.Queued())
	}
	submit(t, r, "bob", "w", epoch)
	if p.Queued() != 1 {
		t.Errorf("Queued=%d after a correction, want the move replaced not stacked", p.Queued())
	}
	if d, ok := p.NextDir(); !ok || d != North {
		t.Errorf("NextDir=%v/%v want the corrected direction", d, ok)
	}

	before := p.At
	r.Tick(epoch)
	if p.At.Y != before.Y-1 {
		t.Errorf("moved to %v from %v, want the corrected direction (up)", p.At, before)
	}
	if p.Queued() != 0 {
		t.Errorf("Queued=%d after the tick, want the move spent", p.Queued())
	}

	// Nothing carries over: standing still is the default.
	at := p.At
	r.Tick(epoch)
	if p.At != at {
		t.Errorf("moved to %v with nothing submitted, want to stay on %v", p.At, at)
	}
}

// TestBonkCostsTheCycle: walking into a wall spends the move and goes nowhere.
func TestBonkCostsTheCycle(t *testing.T) {
	m := openBoard(4)
	m.Start = Cell{0, 0}
	r := start(t, m, testRoundConfig(), "bob")

	submit(t, r, "bob", "a", epoch) // west from column A is the board edge
	evs := r.Tick(epoch)

	p := player(t, r, "bob")
	if p.At != m.Start {
		t.Errorf("at=%v want to have stayed on %v", p.At, m.Start)
	}
	if p.Queued() != 0 {
		t.Errorf("Queued=%d after a bonk, want the move spent", p.Queued())
	}
	if !has(evs, EventBonked, p.Seat) {
		t.Errorf("events=%v want bonked", evs)
	}
}

// TestBounceAtLockedExit covers the keyless player reaching the door. They do not
// move and — since the exit's position was always visible and they never entered
// the cell — nothing about its walls is revealed.
func TestBounceAtLockedExit(t *testing.T) {
	m := openBoard(4)
	m.Start = Cell{3, 2}
	m.Exit = Cell{3, 3}
	r := start(t, m, testRoundConfig(), "bob")

	submit(t, r, "bob", "s", epoch)
	evs := r.Tick(epoch)

	p := player(t, r, "bob")
	if p.At != m.Start {
		t.Errorf("at=%v want to have bounced off the door", p.At)
	}
	if !p.Racing() {
		t.Error("finished without a key")
	}
	if !has(evs, EventBounced, p.Seat) {
		t.Errorf("events=%v want bounced", evs)
	}
	if r.Revealed(m.Exit) {
		t.Error("exit revealed by a bounce; the player never entered the cell")
	}
}

// --- fog --------------------------------------------------------------------

func TestFogRevealAndFrontier(t *testing.T) {
	m := openBoard(4)
	m.Start = Cell{0, 0}
	r := start(t, m, testRoundConfig(), "bob")

	if !r.Revealed(m.Start) {
		t.Error("start not revealed at round open")
	}
	if !r.Frontier(Cell{1, 0}) || !r.Frontier(Cell{0, 1}) {
		t.Error("cells reachable from start are not marked frontier")
	}
	if r.Revealed(Cell{1, 0}) {
		t.Error("a frontier cell must not count as revealed — its walls are unknown")
	}
	if r.Frontier(Cell{2, 0}) {
		t.Error("a cell two steps out is neither revealed nor frontier")
	}

	submit(t, r, "bob", "d", epoch)
	r.Tick(epoch)

	if !r.Revealed(Cell{1, 0}) {
		t.Error("entered cell not revealed")
	}
	if r.Frontier(Cell{1, 0}) {
		t.Error("a revealed cell must stop being frontier")
	}
	if !r.Frontier(Cell{2, 0}) {
		t.Error("the next cell out did not become frontier")
	}
}

// TestSpikeRevealsTheTrapCell: the player is flung home, but they did walk into
// the cell and the shared board should show what they found.
func TestSpikeRevealsTheTrapCell(t *testing.T) {
	m := openBoard(4)
	m.Start = Cell{0, 0}
	m.Traps = []Trap{{At: Cell{1, 0}, Kind: Spike}}
	r := start(t, m, testRoundConfig(), "bob")

	submit(t, r, "bob", "d", epoch)
	r.Tick(epoch)

	if !r.Revealed(Cell{1, 0}) {
		t.Error("trap cell not revealed after someone sprang it")
	}
	if p := player(t, r, "bob"); p.At != m.Start {
		t.Errorf("at=%v want start", p.At)
	}
}

// --- round lifecycle --------------------------------------------------------

func TestAbandonedRound(t *testing.T) {
	r := NewRound(openBoard(4), testRoundConfig(), epoch)
	evs := r.Tick(epoch)
	if r.Phase != PhaseDone || r.Reason != EndAbandoned {
		t.Fatalf("phase=%v reason=%v, want done/abandoned", r.Phase, r.Reason)
	}
	if e, ok := find(evs, EventRoundEnded); !ok || e.Reason != EndAbandoned {
		t.Errorf("events=%v want a round-ended/abandoned event", evs)
	}
	if r.Tick(epoch) != nil {
		t.Error("a finished round kept ticking")
	}
}

func TestJoinClosesAtLockAndAtCapacity(t *testing.T) {
	m := openBoard(4)
	cfg := testRoundConfig()
	cfg.MaxSeats = 2
	r := NewRound(m, cfg, epoch)

	if _, ok := r.Join("a", "a", "a"); !ok {
		t.Fatal("first join refused")
	}
	if _, ok := r.Join("a", "a", "a"); !ok {
		t.Error("re-join should be idempotent, not a refusal")
	}
	if _, ok := r.Join("b", "b", "b"); !ok {
		t.Fatal("second join refused")
	}
	if _, ok := r.Join("c", "c", "c"); ok {
		t.Error("joined past MaxSeats")
	}
	r.Tick(epoch)
	if _, ok := r.Join("d", "d", "d"); ok {
		t.Error("joined after seats locked")
	}
	if r.Submit("d", North, epoch) {
		t.Error("an unseated user's move was accepted")
	}
}

// TestWinnerThenPlacementWindow checks the finish is punchy but not abrupt: the
// win lands immediately and the rest get a scramble for the remaining places.
func TestWinnerThenPlacementWindow(t *testing.T) {
	m := openBoard(4)
	m.Start = Cell{3, 2}
	m.Exit = Cell{3, 3}
	m.Keys = []Cell{{2, 2}, {1, 2}}
	cfg := testRoundConfig()
	cfg.PlacementCycles = 3
	r := start(t, m, cfg, "winner", "idler")

	submit(t, r, "winner", "a", epoch) // grab a key
	r.Tick(epoch)
	submit(t, r, "winner", "d", epoch) // back toward the door
	r.Tick(epoch)
	submit(t, r, "winner", "s", epoch) // through it
	evs := r.Tick(epoch)

	w := player(t, r, "winner")
	if w.Place != 1 {
		t.Fatalf("Place=%d want 1; events=%v", w.Place, evs)
	}
	if r.Phase != PhaseRacing {
		t.Fatalf("phase=%v, want the round still live for placements", r.Phase)
	}

	for i := 0; i < cfg.PlacementCycles; i++ {
		r.Tick(epoch)
	}
	if r.Phase != PhaseDone || r.Reason != EndPlacementClosed {
		t.Errorf("phase=%v reason=%v, want done/placements closed", r.Phase, r.Reason)
	}
	if places := r.Placements(); len(places) != 1 || places[0].UserID != "winner" {
		t.Errorf("placements=%v, want just the winner", places)
	}
}

// TestEndsWhenNobodyCanFinish is the short-circuit that stops the placement
// window running down a clock nobody can beat.
func TestEndsWhenNobodyCanFinish(t *testing.T) {
	m := openBoard(4)
	m.Start = Cell{3, 2}
	m.Exit = Cell{3, 3}
	m.Keys = []Cell{{2, 2}}
	cfg := testRoundConfig()
	cfg.DeficitMinPlayers = 2 // one key, two players
	r := start(t, m, cfg, "winner", "loser")

	submit(t, r, "winner", "a", epoch)
	r.Tick(epoch)
	submit(t, r, "winner", "d", epoch)
	r.Tick(epoch)
	submit(t, r, "winner", "s", epoch)
	r.Tick(epoch)

	if r.Phase != PhaseDone || r.Reason != EndNobodyCanFinish {
		t.Fatalf("phase=%v reason=%v, want done because the last key left play", r.Phase, r.Reason)
	}
	if p := player(t, r, "loser"); !p.Racing() {
		t.Error("the keyless player should end the round unfinished, not placed")
	}
}

func TestEndsWhenEveryoneFinishes(t *testing.T) {
	m := openBoard(4)
	m.Start = Cell{3, 2}
	m.Exit = Cell{3, 3}
	m.Keys = []Cell{{2, 2}, {3, 1}}
	r := start(t, m, testRoundConfig(), "a", "b")

	submit(t, r, "a", "a", epoch) // west to the key on C3
	submit(t, r, "b", "w", epoch) // north to the key on D2
	r.Tick(epoch)
	submit(t, r, "a", "d", epoch)
	submit(t, r, "b", "s", epoch)
	r.Tick(epoch)
	submit(t, r, "a", "s", epoch)
	submit(t, r, "b", "s", epoch)
	r.Tick(epoch)

	if r.Phase != PhaseDone || r.Reason != EndAllFinished {
		t.Fatalf("phase=%v reason=%v, want done/everyone finished", r.Phase, r.Reason)
	}
	if got := len(r.Placements()); got != 2 {
		t.Errorf("%d placements, want 2", got)
	}
}

func TestEndsOnCycleCap(t *testing.T) {
	m := openBoard(6)
	m.Keys = []Cell{{5, 0}}
	cfg := testRoundConfig()
	cfg.MaxCycles = 4
	r := start(t, m, cfg, "bob")

	for i := 0; i < cfg.MaxCycles; i++ {
		if r.Phase != PhaseRacing {
			t.Fatalf("ended early at cycle %d (%v)", i, r.Reason)
		}
		r.Tick(epoch)
	}
	if r.Phase != PhaseDone || r.Reason != EndCycleCap {
		t.Fatalf("phase=%v reason=%v, want done/cycle cap", r.Phase, r.Reason)
	}
}

func TestEndsOnTimeCap(t *testing.T) {
	m := openBoard(6)
	m.Keys = []Cell{{5, 0}}
	cfg := testRoundConfig()
	cfg.MaxCycles = 1000 // so only the wall clock can stop it
	cfg.MaxSeconds = 30
	r := start(t, m, cfg, "bob")

	r.Tick(epoch.Add(10 * time.Second))
	if r.Phase != PhaseRacing {
		t.Fatalf("ended at 10s, before the %ds cap", cfg.MaxSeconds)
	}
	r.Tick(epoch.Add(30 * time.Second))
	if r.Phase != PhaseDone || r.Reason != EndTimeCap {
		t.Fatalf("phase=%v reason=%v, want done/time cap", r.Phase, r.Reason)
	}
}

// TestFinishedPlayerStopsPlaying guards against a winner still walking the board
// during someone else's placement scramble.
func TestFinishedPlayerStopsPlaying(t *testing.T) {
	m := openBoard(4)
	m.Start = Cell{3, 2}
	m.Exit = Cell{3, 3}
	m.Keys = []Cell{{2, 2}, {1, 2}}
	r := start(t, m, testRoundConfig(), "winner", "other")

	submit(t, r, "winner", "a", epoch)
	r.Tick(epoch)
	submit(t, r, "winner", "d", epoch)
	r.Tick(epoch)
	submit(t, r, "winner", "s", epoch)
	r.Tick(epoch)

	w := player(t, r, "winner")
	if w.Place == 0 {
		t.Fatal("winner did not finish")
	}
	if r.Submit("winner", North, epoch) {
		t.Error("a finished player was allowed to queue a move")
	}
	at := w.At
	r.Tick(epoch)
	if w.At != at {
		t.Errorf("finished player moved from %v to %v", at, w.At)
	}
}

// greedyStep is a perfect-information pathfinder: one step from `from` along a
// shortest route to `to`. It ignores fog deliberately — the point is to model
// competent play as an upper bound, so a round that still runs long under
// omniscient players is genuinely too long, not just badly explored.
func greedyStep(m *Map, from, to Cell) (Dir, bool) {
	dist := m.Distances(to)
	best, pick, found := dist[m.idx(from)], North, false
	for _, d := range dirs {
		if !m.Open(from, d) {
			continue
		}
		n, _ := m.Neighbor(from, d)
		if dist[m.idx(n)] >= 0 && dist[m.idx(n)] < best {
			best, pick, found = dist[m.idx(n)], d, true
		}
	}
	return pick, found
}

// greedyTarget is what a competent player heads for: the nearest unclaimed key
// while empty-handed, the door once carrying one. A player who is empty-handed
// with no keys left on the board makes for the exit anyway — there is nothing
// else to do but shadow the leaders and hope somebody hits spikes, which is
// exactly the position the key deficit puts one player in.
func greedyTarget(r *Round, p *Player) Cell {
	if p.HasKey {
		return r.Map.Exit
	}
	keys := r.KeysOnMap()
	if len(keys) == 0 {
		return r.Map.Exit
	}
	dist := r.Map.Distances(p.At)
	best := keys[0]
	for _, k := range keys[1:] {
		if dist[r.Map.idx(k)] < dist[r.Map.idx(best)] {
			best = k
		}
	}
	return best
}

// TestGreedyPlaythrough runs full rounds under competent play. It is the only
// test that exercises the whole chain at once — board geometry, the key detour,
// the deficit, traps, the placement window and the end conditions — and it is
// what backs the claim that a round fits inside the configured cycle cap. The
// random-walk conservation test above covers the messy paths; this one covers
// the path a real round actually takes.
func TestGreedyPlaythrough(t *testing.T) {
	cfg := testRoundConfig()
	var firstFinish []int
	reasons := map[EndReason]int{}
	finishes := 0

	for seed := int64(0); seed < 300; seed++ {
		m := mustGenerate(t, seed, testConfig())
		r := start(t, m, cfg, "a", "b", "c", "d", "e")
		initial := len(r.KeysOnMap())
		won := 0

		for guard := 0; r.Phase == PhaseRacing; guard++ {
			if guard > cfg.MaxCycles+cfg.PlacementCycles+5 {
				t.Fatalf("seed %d: round did not terminate", seed)
			}
			for _, p := range r.Players {
				if p.Racing() && p.StuckFor == 0 {
					if d, ok := greedyStep(m, p.At, greedyTarget(r, p)); ok {
						r.Submit(p.UserID, d, epoch)
					}
				}
			}
			for _, e := range r.Tick(epoch) {
				if e.Kind == EventFinished {
					finishes++
					if e.N == 1 {
						won = r.Cycle
					}
				}
			}

			spent := 0
			for _, p := range r.Players {
				if !p.Racing() {
					spent++
				}
			}
			if got := len(r.KeysOnMap()) + heldKeys(r) + spent; got != initial {
				t.Fatalf("seed %d cycle %d: keys %d, want %d", seed, r.Cycle, got, initial)
			}
		}

		reasons[r.Reason]++
		if won > 0 {
			firstFinish = append(firstFinish, won)
		}

		// Exactly one player is locked out by the deficit, so four of five should
		// get through — unless the round ran out of cycles first.
		if r.Reason == EndPlacementClosed || r.Reason == EndNobodyCanFinish {
			if got := len(r.Placements()); got == 0 {
				t.Fatalf("seed %d: round ended %v with nobody placed", seed, r.Reason)
			}
		}
	}

	if finishes == 0 {
		t.Fatal("no round was ever won; the playthrough is not exercising the exit")
	}
	if len(firstFinish) < 250 {
		t.Errorf("only %d of 300 rounds produced a winner under competent play", len(firstFinish))
	}

	sort.Ints(firstFinish)
	p50, p90 := firstFinish[len(firstFinish)/2], firstFinish[len(firstFinish)*9/10]
	if p90 >= cfg.MaxCycles {
		t.Errorf("p90 cycles to first win = %d, at or over the %d-cycle cap — rounds would routinely time out",
			p90, cfg.MaxCycles)
	}
	t.Logf("cycles to first win: p50=%d p90=%d max=%d (cap %d)", p50, p90, firstFinish[len(firstFinish)-1], cfg.MaxCycles)
	t.Logf("end reasons: %v", reasons)
}

// TestDirWordsRoundTrip: chat says "left" and the archive stores "left", so the
// word and its meaning have to be one fact rather than two that agree today.
func TestDirWordsRoundTrip(t *testing.T) {
	for _, d := range dirs {
		got, ok := ParseDir(d.String())
		if !ok {
			t.Errorf("%v stringifies to %q, which does not parse back", d, d.String())
			continue
		}
		if got != d {
			t.Errorf("%v -> %q -> %v", d, d.String(), got)
		}
	}
	for _, bad := range []string{"", "north", "w", "up ", "UP", "sideways"} {
		if got, ok := ParseDir(bad); ok {
			t.Errorf("ParseDir(%q) = %v, want a refusal", bad, got)
		}
	}
}
