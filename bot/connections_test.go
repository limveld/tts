package main

import (
	"math/rand"
	"strings"
	"testing"
	"time"

	"tts/store"
)

// testConnPuzzle has four groups of four, all words distinct across groups so
// group membership is unambiguous.
var testConnPuzzle = connPuzzle{ID: 1, Groups: []connGroup{
	{Name: "RED", Level: 0, Words: []string{"APPLE", "CHERRY", "RUBY", "ROSE"}},
	{Name: "GREEN", Level: 1, Words: []string{"LIME", "EMERALD", "JADE", "MOSS"}},
	{Name: "BLUE", Level: 2, Words: []string{"SKY", "OCEAN", "SAPPHIRE", "DENIM"}},
	{Name: "PURPLE", Level: 3, Words: []string{"GRAPE", "PLUM", "VIOLET", "ORCHID"}},
}}

// connRouter is an econRouter primed with the Connections economy config, a fake
// overlay, and the single test puzzle so starts are deterministic.
func connRouter(t *testing.T) (*Router, *store.Store, *fakeChat, *fakeOverlay) {
	t.Helper()
	r, _, st, chat := econRouter(t)
	r.econ.ConnectionsGroupReward = 10
	r.econ.ConnectionsSolveBonus = 50
	r.econ.ConnectionsDuration = time.Minute
	r.connPuzzles = []connPuzzle{testConnPuzzle}
	ov := &fakeOverlay{}
	r.overlay = ov
	return r, st, chat, ov
}

// tilesFor returns the tile numbers currently glued to the given words.
func tilesFor(st *connectionsState, words []string) []int {
	nums := make([]int, 0, len(words))
	for _, w := range words {
		for i, sw := range st.Words {
			if sw == w {
				nums = append(nums, i+1)
				break
			}
		}
	}
	return nums
}

// numArgs renders tile numbers as a "!group a b c d" argument string.
func numArgs(nums []int) string { return numsString(nums) }

func TestConnectionsStartBuildsBoard(t *testing.T) {
	r, _, chat, ov := connRouter(t)
	r.Handle(emsg("alice", "!connections", false))

	st := r.conn
	if st == nil || st.Done || len(st.Words) != 16 || len(st.Order) != 16 {
		t.Fatalf("round not started cleanly: %+v", st)
	}
	if r.liveBoard() != boardConnections {
		t.Fatalf("stage not claimed: %v", r.liveBoard())
	}
	if !strings.Contains(lastSend(chat), "Connections!") {
		t.Fatalf("no start announcement; last=%q", lastSend(chat))
	}
	// overlay payload must not leak unsolved grouping: 16 tiles, 0 solved bars.
	p, ok := ov.last("connections")
	if !ok {
		t.Fatal("no connections push on start")
	}
	pl := p.data.(connPayload)
	if len(pl.Tiles) != 16 || len(pl.Solved) != 0 {
		t.Fatalf("start payload tiles=%d solved=%d want 16/0", len(pl.Tiles), len(pl.Solved))
	}
}

func TestConnectionsSolveGroupsAndComplete(t *testing.T) {
	r, st, chat, ov := connRouter(t)
	r.Handle(emsg("alice", "!connections", false))
	cs := r.conn

	// alice solves the first three groups (yellow/green/blue = 10/20/30).
	for g := 0; g < 3; g++ {
		nums := tilesFor(cs, testConnPuzzle.Groups[g].Words)
		r.Handle(emsg("alice", "!group "+numArgs(nums), false))
	}
	if len(cs.SolvedIdx) != 3 || cs.Done {
		t.Fatalf("after 3 groups: solved=%v done=%v", cs.SolvedIdx, cs.Done)
	}
	if bal, _ := st.Balance("id-alice"); bal != 10+20+30 {
		t.Fatalf("alice balance=%d want 60", bal)
	}

	// bob lands the final (purple = 40) group + completion bonus (50).
	nums := tilesFor(cs, testConnPuzzle.Groups[3].Words)
	r.Handle(emsg("bob", "!group "+numArgs(nums), false))
	if !cs.Done || !cs.Won {
		t.Fatalf("after 4th group: done=%v won=%v", cs.Done, cs.Won)
	}
	if bal, _ := st.Balance("id-bob"); bal != 40+50 {
		t.Fatalf("bob balance=%d want 90 (group 40 + bonus 50)", bal)
	}
	wins, _ := st.ConnectionsLeaderboard(10)
	if len(wins) != 1 || wins[0].Login != "bob" || wins[0].Wins != 1 {
		t.Fatalf("leaderboard=%+v want bob with 1 completion", wins)
	}
	if !strings.Contains(lastSend(chat), "completed the puzzle") {
		t.Fatalf("no completion announcement; last=%q", lastSend(chat))
	}
	// final payload: all four groups revealed as bars, no tiles, done+won.
	p, _ := ov.last("connections")
	pl := p.data.(connPayload)
	if len(pl.Solved) != 4 || len(pl.Tiles) != 0 || !pl.Done || !pl.Won {
		t.Fatalf("final payload=%+v want 4 bars, 0 tiles, done+won", pl)
	}
}

func TestConnectionsOneAwayAndBust(t *testing.T) {
	r, _, chat, _ := connRouter(t)
	r.Handle(emsg("alice", "!connections", false))
	cs := r.conn

	red := tilesFor(cs, testConnPuzzle.Groups[0].Words)   // 4 red tiles
	green := tilesFor(cs, testConnPuzzle.Groups[1].Words) // 4 green tiles
	blue := tilesFor(cs, testConnPuzzle.Groups[2].Words)
	purple := tilesFor(cs, testConnPuzzle.Groups[3].Words)

	// three red + one green → "one away", costs a life.
	oneAway := []int{red[0], red[1], red[2], green[0]}
	r.Handle(emsg("bob", "!group "+numArgs(oneAway), false))
	if cs.Mistakes != 1 || !strings.Contains(lastReply(chat), "one away") {
		t.Fatalf("one-away: mistakes=%d reply=%q", cs.Mistakes, lastReply(chat))
	}

	// repeat the exact same wrong guess → ignored, no extra life.
	r.Handle(emsg("bob", "!group "+numArgs(oneAway), false))
	if cs.Mistakes != 1 {
		t.Fatalf("repeat wrong guess should not cost a life; mistakes=%d", cs.Mistakes)
	}

	// Distinct wrong guesses (one tile from each group, so never a group) until
	// the allowance runs out. Each k picks a different tuple, hence a different
	// guess key.
	for k := 1; k < connMaxMistakes; k++ {
		w := []int{red[k%4], green[(k/4)%4], blue[(k/16)%4], purple[(k/64)%4]}
		r.Handle(emsg("bob", "!group "+numArgs(w), false))
	}
	if !cs.Done || cs.Won || cs.Mistakes != connMaxMistakes {
		t.Fatalf("after %d mistakes: done=%v won=%v mistakes=%d", connMaxMistakes, cs.Done, cs.Won, cs.Mistakes)
	}
	if !strings.Contains(lastSend(chat), "Out of mistakes") {
		t.Fatalf("no bust announcement; last=%q", lastSend(chat))
	}
}

func TestConnectionsRejectsBadSubmissions(t *testing.T) {
	r, _, chat, _ := connRouter(t)
	r.Handle(emsg("alice", "!connections", false))

	r.Handle(emsg("bob", "!group 1 2 3", false)) // only three
	if !strings.Contains(lastReply(chat), "four tile numbers") {
		t.Fatalf("short submission reply=%q", lastReply(chat))
	}
	r.Handle(emsg("bob", "!group 1 2 3 99", false)) // out of range
	if !strings.Contains(lastReply(chat), "four tile numbers") {
		t.Fatalf("range submission reply=%q", lastReply(chat))
	}
	if r.conn.Mistakes != 0 {
		t.Fatalf("invalid submissions must not cost a life; mistakes=%d", r.conn.Mistakes)
	}
}

func TestConnectionsArbiterOneGameAtATime(t *testing.T) {
	r, _, chat, _ := connRouter(t)
	r.Handle(emsg("alice", "!connections", false))

	// Wordle is refused while Connections owns the stage.
	r.Handle(emsg("bob", "!wordle", false))
	if r.wordle != nil {
		t.Fatal("wordle should not start while connections is live")
	}
	if !strings.Contains(lastReply(chat), "Connections round is going") {
		t.Fatalf("refusal reply=%q", lastReply(chat))
	}

	// A mod !skipgame frees the stage immediately, so Wordle can start now.
	r.Handle(emsg("mod", "!skipgame", true))
	if r.conn != nil || r.liveBoard() != boardNone {
		t.Fatalf("skipgame didn't clear connections: conn=%v board=%v", r.conn, r.liveBoard())
	}
	r.Handle(emsg("alice", "!wordle", false))
	if r.wordle == nil {
		t.Fatal("wordle should start after skipgame freed the stage")
	}
	// And now Connections is refused while Wordle owns the stage.
	r.Handle(emsg("bob", "!connections", false))
	if r.conn != nil {
		t.Fatal("connections should not start while wordle is live")
	}
	if !strings.Contains(lastReply(chat), "Wordle round is going") {
		t.Fatalf("reverse refusal reply=%q", lastReply(chat))
	}
}

func TestConnectionsShuffleKeepsNumberGlue(t *testing.T) {
	r, _, _, _ := connRouter(t)
	r.Handle(emsg("alice", "!connections", false))
	cs := r.conn
	before := append([]string(nil), cs.Words...)

	r.Handle(emsg("mod", "!shuffle", true))
	// number→word mapping is unchanged; only display order permutes.
	for i := range before {
		if cs.Words[i] != before[i] {
			t.Fatalf("shuffle changed number→word glue at tile %d: %q→%q", i+1, before[i], cs.Words[i])
		}
	}
	if len(cs.Order) != 16 {
		t.Fatalf("shuffle changed tile count: %d", len(cs.Order))
	}
}

func TestConnectionsPersistRestore(t *testing.T) {
	r, st, _, _ := connRouter(t)
	r.Handle(emsg("alice", "!connections", false))
	cs := r.conn
	// solve one group and make one mistake, so there's real state to restore.
	red := tilesFor(cs, testConnPuzzle.Groups[0].Words)
	r.Handle(emsg("alice", "!group "+numArgs(red), false))
	r.Handle(emsg("bob", "!group 1 2 3 4", false)) // a wrong guess (some life lost, unless it happened to be a group)

	// Simulate a restart: a fresh router over the same store restores the round.
	r2, _, _, _ := econRouter(t)
	r2.store = st
	r2.rnd = rand.New(rand.NewSource(1))
	r2.connPuzzles = []connPuzzle{testConnPuzzle}
	r2.restoreConnections()

	rc := r2.conn
	if rc == nil {
		t.Fatal("round not restored")
	}
	if len(rc.SolvedIdx) != len(cs.SolvedIdx) || rc.Mistakes != cs.Mistakes {
		t.Fatalf("restored state solved=%v mistakes=%d want solved=%v mistakes=%d",
			rc.SolvedIdx, rc.Mistakes, cs.SolvedIdx, cs.Mistakes)
	}
	for i := range cs.Words {
		if rc.Words[i] != cs.Words[i] {
			t.Fatalf("restored word glue differs at %d", i)
		}
	}
	if r2.liveBoard() != boardConnections {
		t.Fatalf("restore didn't claim the stage: %v", r2.liveBoard())
	}
}

func TestParseGroupNums(t *testing.T) {
	cases := []struct {
		in   string
		want []int
		ok   bool
	}{
		{"1 5 9 12", []int{1, 5, 9, 12}, true},
		{"1,5,9,12", []int{1, 5, 9, 12}, true},
		{" 3 , 7 , 11 , 14 ", []int{3, 7, 11, 14}, true},
		{"1 2 3", nil, false},     // too few
		{"1 2 3 4 5", nil, false}, // too many
		{"1 2 3 17", nil, false},  // out of range
		{"1 2 3 3", nil, false},   // duplicate
		{"a b c d", nil, false},   // non-numeric
	}
	for _, c := range cases {
		got, ok := parseGroupNums(c.in)
		if ok != c.ok {
			t.Errorf("parseGroupNums(%q) ok=%v want %v", c.in, ok, c.ok)
			continue
		}
		if ok && numsString(got) != numsString(c.want) {
			t.Errorf("parseGroupNums(%q)=%v want %v", c.in, got, c.want)
		}
	}
}

func TestConnectionsSeedLoads(t *testing.T) {
	// The embedded seed must parse into a non-trivial bank, and a bad path falls
	// back to it rather than erroring.
	puzzles, source := loadConnectionsPuzzles("/no/such/file.json")
	if len(puzzles) < 100 || source != "embedded seed" {
		t.Fatalf("fallback load: %d puzzles from %q", len(puzzles), source)
	}
	for _, p := range puzzles[:20] {
		if len(p.Groups) != 4 {
			t.Fatalf("puzzle %d has %d groups", p.ID, len(p.Groups))
		}
		for _, g := range p.Groups {
			if len(g.Words) != 4 || g.Name == "" {
				t.Fatalf("puzzle %d group %q malformed", p.ID, g.Name)
			}
			if g.Level < 0 || g.Level > 3 {
				t.Fatalf("puzzle %d group %q level=%d out of 0..3", p.ID, g.Name, g.Level)
			}
		}
	}
}

// The upstream corpus publishes newer puzzles with every group at level -1.
// Those must come out of the parser with usable levels — an unlabeled level once
// panicked the bot on the color-emoji lookup when a group was solved.
func TestConnectionsUnlabeledLevelsNormalized(t *testing.T) {
	raw := []byte(`[{"id":1,"date":"2026-01-01","answers":[
		{"level":-1,"group":"RED","members":["APPLE","CHERRY","RUBY","ROSE"]},
		{"level":-1,"group":"GREEN","members":["LIME","EMERALD","JADE","MOSS"]},
		{"level":-1,"group":"BLUE","members":["SKY","OCEAN","SAPPHIRE","DENIM"]},
		{"level":-1,"group":"PURPLE","members":["GRAPE","PLUM","VIOLET","ORCHID"]}]}]`)
	puzzles := parseConnections(raw)
	if len(puzzles) != 1 {
		t.Fatalf("parsed %d puzzles, want 1", len(puzzles))
	}
	for i, g := range puzzles[0].Groups {
		if g.Level != i {
			t.Fatalf("group %d (%s) level=%d, want %d", i, g.Name, g.Level, i)
		}
	}

	// And solving a group from such a puzzle announces rather than panics.
	r, _, chat, _ := connRouter(t)
	r.connPuzzles = puzzles
	r.Handle(emsg("alice", "!connections", false))
	cs := r.conn
	r.Handle(emsg("alice", "!group "+numArgs(tilesFor(cs, puzzles[0].Groups[0].Words)), false))
	if len(cs.SolvedIdx) != 1 || !strings.Contains(lastSend(chat), "RED") {
		t.Fatalf("solve on unlabeled puzzle: solved=%v last=%q", cs.SolvedIdx, lastSend(chat))
	}
}
