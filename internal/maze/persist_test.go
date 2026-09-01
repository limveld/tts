package maze

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"
	"time"
)

// liveRound plays a generated board a few cycles in, so snapshots are taken of a
// round with real accumulated state — fog spread, keys claimed, traps sprung —
// rather than of a pristine one where most fields are still zero.
func liveRound(t *testing.T, seed int64, cycles int) *Round {
	t.Helper()
	m := mustGenerate(t, seed, testConfig())
	r := start(t, m, testRoundConfig(), "a", "b", "c", "d", "e")
	rnd := rand.New(rand.NewSource(seed))
	for i := 0; i < cycles && r.Phase == PhaseRacing; i++ {
		for _, p := range r.Players {
			if p.Racing() {
				if d, ok := greedyStep(m, p.At, greedyTarget(r, p)); ok {
					r.Submit(p.UserID, d, epoch.Add(time.Duration(rnd.Intn(999))*time.Millisecond))
				}
			}
		}
		r.Tick(epoch)
	}
	return r
}

func roundTrip(t *testing.T, r *Round) *Round {
	t.Helper()
	blob, err := json.Marshal(r.State())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back RoundState
	if err := json.Unmarshal(blob, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	restored, err := Restore(back)
	if err != nil {
		t.Fatalf("Restore: %v", err)
	}
	return restored
}

// assertSameRound compares two rounds through their public surface rather than
// through State().
//
// That distinction matters more than it looks. Comparing State() to State() is
// self-referential: anything State() forgets to record is equally absent from
// both sides, so the comparison passes while the data is being lost. Fog is the
// case that exposed it — frontier cells affect only rendering, never play, so
// neither a State() comparison nor any gameplay divergence test can see them go
// missing. What the caller is actually promised is this surface.
func assertSameRound(t *testing.T, got, want *Round, ctx string) {
	t.Helper()
	if got.Phase != want.Phase || got.Reason != want.Reason || got.Cycle != want.Cycle {
		t.Fatalf("%s: phase/reason/cycle = %v/%v/%d, want %v/%v/%d",
			ctx, got.Phase, got.Reason, got.Cycle, want.Phase, want.Reason, want.Cycle)
	}
	if !got.Deadline().Equal(want.Deadline()) {
		t.Fatalf("%s: deadline %v, want %v", ctx, got.Deadline(), want.Deadline())
	}
	if len(got.Players) != len(want.Players) {
		t.Fatalf("%s: %d players, want %d", ctx, len(got.Players), len(want.Players))
	}
	for i := range want.Players {
		a, b := got.Players[i], want.Players[i]
		if a.UserID != b.UserID || a.At != b.At || a.HasKey != b.HasKey ||
			a.StuckFor != b.StuckFor || a.Place != b.Place || a.Queued() != b.Queued() {
			t.Fatalf("%s: player %d = %+v, want %+v", ctx, i, a, b)
		}
	}
	if !reflect.DeepEqual(got.KeysOnMap(), want.KeysOnMap()) {
		t.Fatalf("%s: keys %v, want %v", ctx, got.KeysOnMap(), want.KeysOnMap())
	}
	for i := range want.Map.Traps {
		if got.TrapSprung(i) != want.TrapSprung(i) {
			t.Fatalf("%s: trap %d sprung=%v, want %v", ctx, i, got.TrapSprung(i), want.TrapSprung(i))
		}
	}
	for y := 0; y < want.Map.Size; y++ {
		for x := 0; x < want.Map.Size; x++ {
			c := Cell{x, y}
			if got.Revealed(c) != want.Revealed(c) {
				t.Fatalf("%s: %v revealed=%v, want %v", ctx, c, got.Revealed(c), want.Revealed(c))
			}
			if got.Frontier(c) != want.Frontier(c) {
				t.Fatalf("%s: %v frontier=%v, want %v", ctx, c, got.Frontier(c), want.Frontier(c))
			}
			if got.Map.Walls(c) != want.Map.Walls(c) {
				t.Fatalf("%s: %v walls=%04b, want %04b", ctx, c, got.Map.Walls(c), want.Map.Walls(c))
			}
		}
	}
}

// TestRoundTripThroughJSON checks nothing is lost on the way to storage and
// back, across many boards so it sees varied fog, key and trap states.
func TestRoundTripThroughJSON(t *testing.T) {
	for seed := int64(0); seed < 60; seed++ {
		r := liveRound(t, seed, 6)
		back := roundTrip(t, r)
		assertSameRound(t, back, r, fmt.Sprintf("seed %d", seed))
		if got, want := back.State(), r.State(); !reflect.DeepEqual(got, want) {
			t.Fatalf("seed %d: stored form changed across a round trip: got %+v want %+v", seed, got, want)
		}
	}
}

// greedyMoves picks each racer's next step from one round, so every copy under
// test is driven by identical input and any difference is the engine's.
func greedyMoves(r *Round) map[string]Dir {
	out := map[string]Dir{}
	for _, p := range r.Players {
		if p.Racing() && p.StuckFor == 0 {
			if d, ok := greedyStep(r.Map, p.At, greedyTarget(r, p)); ok {
				out[p.UserID] = d
			}
		}
	}
	return out
}

func applyMoves(r *Round, moves map[string]Dir, at time.Time) {
	for _, p := range r.Players {
		if d, ok := moves[p.UserID]; ok {
			r.Submit(p.UserID, d, at)
		}
	}
}

// TestRestoredRoundPlaysIdentically is the guarantee that actually matters. A
// round that came back looking right but then diverged on the next tick would be
// worse than one that failed to load: the divergence is invisible, and the
// players who lived through the first half are scored on the second.
//
// It checks two things at once, because neither alone is enough. Restoring at
// *every* cycle and comparing a single tick catches state dropped at some
// particular moment in a round. Restoring once after the first player finishes
// and then playing through to the end catches state whose effect only lands
// several cycles later — the placement-window deadline being the case that
// motivated this, since it is set on the winning cycle and not read again until
// the window closes.
//
// Play is greedy rather than random on purpose. Random walkers reach the exit in
// roughly one round in a hundred, so a random-driven version of this test almost
// never sees a finish, a placement window or a contested key, and would pass
// happily while persistence quietly dropped all three.
func TestRestoredRoundPlaysIdentically(t *testing.T) {
	for seed := int64(0); seed < 40; seed++ {
		m := mustGenerate(t, seed, testConfig())
		live := start(t, m, testRoundConfig(), "a", "b", "c", "d", "e")

		var resumed *Round
		resumedFrom := 0
		for live.Phase == PhaseRacing {
			probe := roundTrip(t, live)
			moves := greedyMoves(live)
			at := epoch.Add(time.Duration(live.Cycle) * time.Second)

			applyMoves(live, moves, at)
			applyMoves(probe, moves, at)
			if resumed != nil {
				applyMoves(resumed, moves, at)
			}

			cycle := live.Cycle
			want := live.Tick(epoch)

			if got := probe.Tick(epoch); !reflect.DeepEqual(got, want) {
				t.Fatalf("seed %d cycle %d: a round restored this cycle emitted different events: got %+v want %+v",
					seed, cycle, got, want)
			}
			assertSameRound(t, probe, live, fmt.Sprintf("seed %d cycle %d: round restored this cycle", seed, cycle))

			if resumed != nil {
				if got := resumed.Tick(epoch); !reflect.DeepEqual(got, want) {
					t.Fatalf("seed %d cycle %d: round resumed at cycle %d emitted different events: got %+v want %+v",
						seed, cycle, resumedFrom, got, want)
				}
				assertSameRound(t, resumed, live, fmt.Sprintf("seed %d cycle %d: round resumed at cycle %d", seed, cycle, resumedFrom))
				continue
			}
			for _, e := range want {
				if e.Kind == EventFinished {
					resumed, resumedFrom = roundTrip(t, live), cycle
					break
				}
			}
		}

		if resumed != nil && (live.Phase != resumed.Phase || live.Reason != resumed.Reason) {
			t.Fatalf("seed %d: ended %v/%v live but %v/%v resumed",
				seed, live.Phase, live.Reason, resumed.Phase, resumed.Reason)
		}
	}
}

// TestRestorePreservesSubmissionOrder targets the one field whose loss is
// invisible in ordinary play: a submission timestamp only decides anything when
// two players reach the same key on the same cycle. The later submitter is
// seated first here, so if the timestamps did not survive storage the seat-order
// tie-break would hand the key to the wrong player.
func TestRestorePreservesSubmissionOrder(t *testing.T) {
	m := openBoard(4)
	m.Start = Cell{1, 1}
	m.Keys = []Cell{{1, 0}}
	cfg := testRoundConfig()
	cfg.DeficitMinPlayers = 2 // one key, two players
	r := start(t, m, cfg, "seated-first", "seated-second")

	r.Submit("seated-first", North, epoch.Add(2*time.Second))
	r.Submit("seated-second", North, epoch.Add(1*time.Second))

	resumed := roundTrip(t, r)
	resumed.Tick(epoch)

	first, _ := resumed.PlayerBy("seated-first")
	second, _ := resumed.PlayerBy("seated-second")
	if first.HasKey || !second.HasKey {
		t.Errorf("first.HasKey=%v second.HasKey=%v — the earlier submission must still win after a restore",
			first.HasKey, second.HasKey)
	}
}

// TestRestoreDuringJoinWindow covers a restart in the few seconds before seats
// lock — the window is short but it is also when the streamer is most likely to
// be restarting the bot, having just started a round.
func TestRestoreDuringJoinWindow(t *testing.T) {
	m := mustGenerate(t, 3, testConfig())
	cfg := testRoundConfig()
	cfg.JoinCycles = 3
	r := NewRound(m, cfg, epoch)
	r.Join("a", "a", "a")
	r.Join("b", "b", "b")
	r.Tick(epoch)

	resumed := roundTrip(t, r)
	if resumed.Phase != PhaseJoining {
		t.Fatalf("phase=%v, want still joining", resumed.Phase)
	}
	if got := len(resumed.Players); got != 2 {
		t.Fatalf("%d players, want 2", got)
	}
	if len(resumed.KeysOnMap()) != 0 {
		t.Error("keys visible before lock in the resumed round")
	}
	// A late arrival must still be able to take the remaining seat.
	if _, ok := resumed.Join("c", "c", "c"); !ok {
		t.Error("join refused on a resumed round still in its join window")
	}
	for resumed.Phase == PhaseJoining {
		resumed.Tick(epoch)
	}
	if got := len(resumed.KeysOnMap()); got != 2 {
		t.Errorf("%d keys after lock with 3 players, want 2", got)
	}
}

// TestRestoreFinishedRound checks a settled round survives storage, so the bot
// can pay out placements after a restart that lands between the win and the
// payout.
func TestRestoreFinishedRound(t *testing.T) {
	m := openBoard(4)
	m.Start = Cell{3, 2}
	m.Exit = Cell{3, 3}
	m.Keys = []Cell{{2, 2}, {3, 1}}
	r := start(t, m, testRoundConfig(), "a", "b")
	submit(t, r, "a", "a", epoch)
	submit(t, r, "b", "w", epoch)
	r.Tick(epoch)
	submit(t, r, "a", "d", epoch)
	submit(t, r, "b", "s", epoch)
	r.Tick(epoch)
	submit(t, r, "a", "s", epoch)
	submit(t, r, "b", "s", epoch)
	r.Tick(epoch)
	if r.Phase != PhaseDone {
		t.Fatalf("phase=%v, want done", r.Phase)
	}

	resumed := roundTrip(t, r)
	if resumed.Phase != PhaseDone || resumed.Reason != r.Reason {
		t.Fatalf("phase=%v reason=%v, want %v/%v", resumed.Phase, resumed.Reason, r.Phase, r.Reason)
	}
	got, want := resumed.Placements(), r.Placements()
	if len(got) != len(want) {
		t.Fatalf("%d placements, want %d", len(got), len(want))
	}
	for i := range got {
		if got[i].UserID != want[i].UserID || got[i].Place != want[i].Place {
			t.Errorf("place %d: %s/%d want %s/%d", i, got[i].UserID, got[i].Place, want[i].UserID, want[i].Place)
		}
	}
}

// TestStoredRoundIsLegible checks the record can be read in psql without
// decoding anything — the same reason the store keeps room_id and ends_at as
// real columns rather than burying them in the blob.
func TestStoredRoundIsLegible(t *testing.T) {
	r := liveRound(t, 11, 5)
	blob, err := json.Marshal(r.State())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	doc := string(blob)
	for _, want := range []string{
		`"start":"` + r.Map.Start.String() + `"`,
		`"exit":"` + r.Map.Exit.String() + `"`,
		`"phase":"racing"`,
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("stored round does not contain %s\n%s", want, doc)
		}
	}
	// Enum values must never be stored as their iota numbers: inserting a new
	// constant later would silently reinterpret every already-stored round.
	if strings.Contains(doc, `"phase":0`) || strings.Contains(doc, `"phase":1`) {
		t.Error("phase stored as a number; it must be a stable string")
	}
}

// --- rejection --------------------------------------------------------------

func validState(t *testing.T) RoundState {
	t.Helper()
	return liveRound(t, 5, 4).State()
}

// TestRestoreRejectsCorruptRecords covers each way a stored round can be wrong.
// Every case here is a deliberate choice to lose one round rather than resume a
// game whose state cannot be trusted and then pay out on it.
func TestRestoreRejectsCorruptRecords(t *testing.T) {
	cases := []struct {
		name   string
		break_ func(*RoundState)
	}{
		{"unknown phase", func(s *RoundState) { s.Phase = "paused" }},
		{"unknown end reason", func(s *RoundState) { s.Reason = "vibes" }},
		{"negative cycle", func(s *RoundState) { s.Cycle = -1 }},
		{"board too small", func(s *RoundState) { s.Board.Size = 2 }},
		{"wall count mismatch", func(s *RoundState) { s.Board.Walls = s.Board.Walls[:10] }},
		{"walls disagree", func(s *RoundState) { s.Board.Walls[0] = 0 }},
		{"open through the edge", func(s *RoundState) {
			for i := range s.Board.Walls {
				s.Board.Walls[i] = 0
			}
		}},
		{"start off board", func(s *RoundState) { s.Board.Start = "Z9" }},
		{"exit unparseable", func(s *RoundState) { s.Board.Exit = "!!" }},
		{"unknown trap kind", func(s *RoundState) { s.Board.Traps[0].Kind = "landmine" }},
		{"trap off board", func(s *RoundState) { s.Board.Traps[0].At = "Z9" }},
		{"sprung index out of range", func(s *RoundState) { s.Sprung = []int{99} }},
		{"key off board", func(s *RoundState) { s.Keys = []string{"Z9"} }},
		{"revealed cell off board", func(s *RoundState) { s.Revealed = append(s.Revealed, "Z9") }},
		{"player position off board", func(s *RoundState) { s.Players[0].At = "Z9" }},
		{"seats out of order", func(s *RoundState) { s.Players[0].Seat = 4 }},
		{"player without a user id", func(s *RoundState) { s.Players[0].UserID = "" }},
		{"duplicate player", func(s *RoundState) { s.Players[1].UserID = s.Players[0].UserID }},
		{"unknown queued direction", func(s *RoundState) { s.Players[0].Queue = []string{"widdershins"} }},
		{"more finishers than players", func(s *RoundState) { s.Finished = len(s.Players) + 1 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := validState(t)
			tc.break_(&s)
			if _, err := Restore(s); err == nil {
				t.Fatal("want an error, got nil — a corrupt round would have been resumed")
			}
		})
	}
}

func TestRestoreAcceptsAValidRecord(t *testing.T) {
	if _, err := Restore(validState(t)); err != nil {
		t.Fatalf("a valid record was rejected: %v", err)
	}
}

func TestParseCellRoundTrip(t *testing.T) {
	for y := 0; y < 7; y++ {
		for x := 0; x < 7; x++ {
			c := Cell{x, y}
			got, err := ParseCell(c.String())
			if err != nil {
				t.Fatalf("ParseCell(%q): %v", c.String(), err)
			}
			if got != c {
				t.Errorf("ParseCell(%q)=%v want %v", c.String(), got, c)
			}
		}
	}
	for _, bad := range []string{"", "A", "4", "AA", "A0", "A-1", "!1", "Ax"} {
		if got, err := ParseCell(bad); err == nil {
			t.Errorf("ParseCell(%q)=%v, want an error", bad, got)
		}
	}
}
