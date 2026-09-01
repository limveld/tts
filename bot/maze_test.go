package main

import (
	"encoding/json"
	"math/rand"
	"strings"
	"testing"
	"time"

	"tts/internal/maze"
)

// startTestMaze opens a round and stops its real ticker, so a test drives cycles
// by calling tickMaze directly rather than waiting out an 8-second clock.
func startTestMaze(t *testing.T) (*Router, Store, *fakeChat, *fakeOverlay, *mazeRound) {
	t.Helper()
	r, _, st, chat := econRouter(t)
	ov := &fakeOverlay{}
	r.overlay = ov

	r.Handle(emsg("alice", "!maze", false))
	mr := r.maze
	if mr == nil {
		t.Fatal("!maze did not start a round")
	}
	t.Cleanup(mr.halt)
	mr.halt()
	return r, st, chat, ov, mr
}

// cycle advances the round one tick.
func cycle(r *Router, mr *mazeRound) { r.tickMaze(mr, time.Now()) }

// seat joins users and runs the join window out, so the round is racing.
func seat(t *testing.T, r *Router, mr *mazeRound, users ...string) {
	t.Helper()
	for _, u := range users {
		r.Handle(emsg(u, "!go w", false))
	}
	for mr.round.Phase == maze.PhaseJoining {
		cycle(r, mr)
	}
}

func lastMazeBoard(t *testing.T, ov *fakeOverlay) mazeBoard {
	t.Helper()
	p, ok := ov.last("maze")
	if !ok {
		t.Fatal("no maze push")
	}
	b, ok := p.data.(mazeBoard)
	if !ok {
		t.Fatalf("maze push carried %T, want mazeBoard", p.data)
	}
	return b
}

func TestMazeStartClaimsStageAndAnnounces(t *testing.T) {
	r, _, chat, ov, mr := startTestMaze(t)

	if got := r.liveBoard(); got != boardMaze {
		t.Errorf("stage held by %q, want maze", got)
	}
	if mr.round.Phase != maze.PhaseJoining {
		t.Errorf("phase=%v, want joining", mr.round.Phase)
	}
	if len(chat.sends) != 1 || !strings.Contains(chat.sends[0], "!go") {
		t.Errorf("open announcement=%v, want one line telling people to !go", chat.sends)
	}
	if b := lastMazeBoard(t, ov); b.Size != mr.round.Map.Size || len(b.Cells) != b.Size*b.Size {
		t.Errorf("pushed board size=%d cells=%d", b.Size, len(b.Cells))
	}
}

func TestMazeRefusedWhileAnotherGameHoldsStage(t *testing.T) {
	r, _, _, chat := econRouter(t)
	r.overlay = &fakeOverlay{}
	r.Handle(emsg("alice", "!wordle", false))
	if r.wordle == nil {
		t.Fatal("wordle did not start")
	}
	chat.replies = nil

	r.Handle(emsg("bob", "!maze", false))
	if r.maze != nil {
		t.Fatal("maze started while wordle held the stage")
	}
	if len(chat.replies) != 1 || !strings.Contains(chat.replies[0].text, "Wordle") {
		t.Errorf("replies=%v, want a refusal naming the live game", chat.replies)
	}
}

// TestMazeSeatsLockResolvesKeyCount is the deficit rule as chat sees it: three
// players get two keys, and the bot says so, because the whole point of the
// shortfall is that people know to race for one.
func TestMazeSeatsLockResolvesKeyCount(t *testing.T) {
	r, _, chat, _, mr := startTestMaze(t)
	seat(t, r, mr, "bob", "carol", "dave")

	if got := len(mr.round.Players); got != 3 {
		t.Fatalf("%d players seated, want 3", got)
	}
	if got := len(mr.round.KeysOnMap()); got != 2 {
		t.Fatalf("%d keys for 3 players, want 2", got)
	}
	roster := chat.sends[len(chat.sends)-1]
	for _, want := range []string{"bob", "carol", "dave", "2 keys"} {
		if !strings.Contains(roster, want) {
			t.Errorf("roster line %q missing %q", roster, want)
		}
	}
	if !strings.Contains(roster, "short") {
		t.Errorf("roster line %q does not warn about the key shortfall", roster)
	}
}

func TestMazeFullHouseTurnsPeopleAway(t *testing.T) {
	r, _, chat, _, mr := startTestMaze(t)
	for _, u := range []string{"a", "b", "c", "d", "e"} {
		r.Handle(emsg(u, "!go w", false))
	}
	if got := len(mr.round.Players); got != 5 {
		t.Fatalf("%d seated, want 5", got)
	}

	chat.replies = nil
	r.Handle(emsg("f", "!go w", false))
	if got := len(mr.round.Players); got != 5 {
		t.Errorf("%d seated after a sixth !go, want 5", got)
	}
	if len(chat.replies) != 1 || !strings.Contains(chat.replies[0].text, "next round") {
		t.Errorf("replies=%v, want the sixth player told they're up next round", chat.replies)
	}
}

// TestMazeLockedSeatsTurnLatecomersAway covers the other refusal: seats can close
// with the round half empty, and the reason someone can't play is then that they
// were late, not that the round was popular. Saying "all seats are taken" there
// would be a lie.
func TestMazeLockedSeatsTurnLatecomersAway(t *testing.T) {
	r, _, chat, _, mr := startTestMaze(t)
	seat(t, r, mr, "bob", "carol")
	if len(mr.round.Players) >= mr.round.Cfg.MaxSeats {
		t.Fatal("round is full; this test needs spare seats")
	}

	chat.replies = nil
	r.Handle(emsg("latecomer", "!go w", false))
	if len(chat.replies) != 1 || !strings.Contains(chat.replies[0].text, "locked") {
		t.Errorf("replies=%v, want a late joiner told seats are locked", chat.replies)
	}
	if got := len(mr.round.Players); got != 2 {
		t.Errorf("%d players after a late !go, want 2", got)
	}
}

// TestMazeMovesAreSilent guards the decision that a move gets no chat reply. Five
// players on an 8s cycle would otherwise put ~35 confirmations into a chat with
// fewer than ten people in it.
func TestMazeMovesAreSilent(t *testing.T) {
	r, _, chat, _, mr := startTestMaze(t)
	seat(t, r, mr, "bob", "carol")
	sends, replies := len(chat.sends), len(chat.replies)

	r.Handle(emsg("bob", "!go wwd", false))
	r.Handle(emsg("carol", "!go s", false))

	if len(chat.sends) != sends || len(chat.replies) != replies {
		t.Errorf("a move spoke: sends+%d replies+%d",
			len(chat.sends)-sends, len(chat.replies)-replies)
	}
	if p, _ := mr.round.PlayerBy("id-bob"); p.Queued() != 3 {
		t.Errorf("bob queued %d moves, want 3", p.Queued())
	}
}

func TestMazeRejectsNonsenseAndMissingRound(t *testing.T) {
	r, _, _, chat := econRouter(t)
	r.overlay = &fakeOverlay{}

	r.Handle(emsg("bob", "!go w", false))
	if len(chat.replies) != 1 || !strings.Contains(chat.replies[0].text, "!maze") {
		t.Fatalf("replies=%v, want !go with no round to point at !maze", chat.replies)
	}

	r.Handle(emsg("alice", "!maze", false))
	t.Cleanup(r.maze.halt)
	r.maze.halt()
	chat.replies = nil

	r.Handle(emsg("bob", "!go sideways", false))
	if len(chat.replies) != 1 || !strings.Contains(chat.replies[0].text, "w/a/s/d") {
		t.Errorf("replies=%v, want a usage hint", chat.replies)
	}
	if got := len(r.maze.round.Players); got != 0 {
		t.Errorf("%d players seated by an unparseable move, want 0", got)
	}
}

// TestMazePayloadWithholdsUnsprungTraps is the one real secrecy guarantee in the
// render payload. Traps are the only genuine surprise left once objectives are
// visible, and a payload carrying them would put every one a devtools panel away.
func TestMazePayloadWithholdsUnsprungTraps(t *testing.T) {
	r, _, _, ov, mr := startTestMaze(t)
	seat(t, r, mr, "bob")

	b := lastMazeBoard(t, ov)
	blob, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	doc := string(blob)

	sent, withheld := 0, 0
	for i, tr := range mr.round.Map.Traps {
		if mr.round.TrapSprung(i) {
			sent++
			continue
		}
		withheld++
		if strings.Contains(doc, `"`+tr.At.String()+`"`) {
			t.Errorf("unsprung %s trap at %v appears in the render payload:\n%s", tr.Kind, tr.At, doc)
		}
	}
	if len(b.Traps) != sent {
		t.Errorf("payload carries %d traps, want %d sprung ones", len(b.Traps), sent)
	}
	if withheld == 0 {
		t.Fatal("every trap had already sprung; this proves nothing")
	}
}

// TestMazePayloadWithholdsUnrevealedWalls: the fog hides the maze's shape, so an
// unwalked cell's layout must not travel to the renderer at all — hiding it in
// CSS would leave it readable in the page.
func TestMazePayloadWithholdsUnrevealedWalls(t *testing.T) {
	r, _, _, ov, mr := startTestMaze(t)
	seat(t, r, mr, "bob")
	for i := 0; i < 3; i++ {
		r.Handle(emsg("bob", "!go w", false))
		cycle(r, mr)
	}

	b := lastMazeBoard(t, ov)
	revealed, hidden := 0, 0
	for i, c := range b.Cells {
		switch c.State {
		case "revealed":
			revealed++
		case "frontier", "unknown":
			hidden++
			if c.Walls != 0 {
				t.Errorf("cell %d is %s but carries walls %04b", i, c.State, c.Walls)
			}
		default:
			t.Fatalf("cell %d has unknown state %q", i, c.State)
		}
	}
	if revealed == 0 || hidden == 0 {
		t.Fatalf("fog not exercised: %d revealed, %d hidden", revealed, hidden)
	}
	// Objectives are the deliberate exception: the fog hides the route, not the
	// destination.
	if b.Exit == "" || len(b.Keys) == 0 {
		t.Errorf("payload should always carry the exit and remaining keys; exit=%q keys=%v", b.Exit, b.Keys)
	}
}

// TestMazePayloadWithholdsMoveDirection: showing intent would let a player on a
// fast connection intercept one on a slow connection every cycle.
func TestMazePayloadWithholdsMoveDirection(t *testing.T) {
	r, _, _, ov, mr := startTestMaze(t)
	seat(t, r, mr, "bob")
	r.Handle(emsg("bob", "!go w", false))
	cycle(r, mr)

	b := lastMazeBoard(t, ov)
	var locked bool
	for _, p := range b.Players {
		if p.Locked {
			locked = true
		}
	}
	blob, _ := json.Marshal(b)
	for _, dir := range []string{`"up"`, `"down"`, `"left"`, `"right"`} {
		if strings.Contains(string(blob), dir) {
			t.Errorf("render payload leaks a queued direction (%s):\n%s", dir, blob)
		}
	}
	_ = locked // locked-ness may or may not survive the tick; the leak is the point
}

func TestMazeSkipGameEndsRoundAndFreesStage(t *testing.T) {
	r, _, chat, ov, mr := startTestMaze(t)
	seat(t, r, mr, "bob")

	r.Handle(emsg("chan", "!skipgame", true))
	if r.maze != nil {
		t.Error("round survived !skipgame")
	}
	if got := r.liveBoard(); got != boardNone {
		t.Errorf("stage still held by %q after !skipgame", got)
	}
	if !strings.Contains(lastSend(chat), "skipped") {
		t.Errorf("last send=%q, want a skip announcement", lastSend(chat))
	}
	p, _ := ov.last("maze")
	if m, ok := p.data.(map[string]any); !ok || m["hidden"] != true {
		t.Errorf("last push=%v, want the board hidden", p.data)
	}
}

// TestMazeSurvivesRestart runs the real restore path: a second Router over the
// same store picks the round up mid-flight, with the board, the fog and the
// players intact.
func TestMazeSurvivesRestart(t *testing.T) {
	r, st, _, _, mr := startTestMaze(t)
	seat(t, r, mr, "bob", "carol", "dave")
	for i := 0; i < 3; i++ {
		r.Handle(emsg("bob", "!go w", false))
		r.Handle(emsg("carol", "!go s", false))
		cycle(r, mr)
	}
	if mr.round.Phase != maze.PhaseRacing {
		t.Fatalf("round ended (%v) before the restart point; the test needs a live round", mr.round.Reason)
	}
	want := mr.round

	r2 := newTestRouter(&fakeTTS{})
	r2.store = st
	r2.chat = &fakeChat{}
	r2.overlay = &fakeOverlay{}
	r2.rnd = rand.New(rand.NewSource(1))
	r2.loadMaze()

	if r2.maze == nil {
		t.Fatal("loadMaze did not restore the round")
	}
	t.Cleanup(r2.maze.halt)
	r2.maze.halt()
	got := r2.maze.round

	if got.Cycle != want.Cycle || got.Phase != want.Phase {
		t.Errorf("restored cycle/phase = %d/%v, want %d/%v", got.Cycle, got.Phase, want.Cycle, want.Phase)
	}
	if got.Map.Start != want.Map.Start || got.Map.Exit != want.Map.Exit {
		t.Errorf("restored board start/exit = %v/%v, want %v/%v",
			got.Map.Start, got.Map.Exit, want.Map.Start, want.Map.Exit)
	}
	if len(got.Players) != len(want.Players) {
		t.Fatalf("%d players restored, want %d", len(got.Players), len(want.Players))
	}
	for i := range want.Players {
		a, b := got.Players[i], want.Players[i]
		if a.UserID != b.UserID || a.At != b.At || a.HasKey != b.HasKey {
			t.Errorf("player %d restored as %+v, want %+v", i, a, b)
		}
	}
	for y := 0; y < want.Map.Size; y++ {
		for x := 0; x < want.Map.Size; x++ {
			c := maze.Cell{X: x, Y: y}
			if got.Revealed(c) != want.Revealed(c) || got.Frontier(c) != want.Frontier(c) {
				t.Fatalf("fog at %v differs after restore", c)
			}
		}
	}
	if got := r2.liveBoard(); got != boardMaze {
		t.Errorf("restored round did not claim the stage; stage=%q", got)
	}
}

func TestMazeRestartDropsAFinishedRound(t *testing.T) {
	r, st, _, _, mr := startTestMaze(t)
	seat(t, r, mr, "bob")
	for mr.round.Phase != maze.PhaseDone {
		cycle(r, mr)
	}

	r2 := newTestRouter(&fakeTTS{})
	r2.store = st
	r2.chat = &fakeChat{}
	r2.overlay = &fakeOverlay{}
	r2.loadMaze()
	if r2.maze != nil {
		t.Error("a finished round was resurrected on restart")
	}
	if got := r2.liveBoard(); got != boardNone {
		t.Errorf("stage claimed by a finished round: %q", got)
	}
}

func TestMazeRestartDropsAnUnreadableRound(t *testing.T) {
	r, st, _, _, mr := startTestMaze(t)
	seat(t, r, mr, "bob")

	rec, ok, err := st.LoadRound(mazeGame)
	if err != nil || !ok {
		t.Fatalf("no stored round: ok=%v err=%v", ok, err)
	}
	corrupt := strings.Replace(string(rec.State), `"phase":"racing"`, `"phase":"vibes"`, 1)
	if corrupt == string(rec.State) {
		t.Fatalf("could not corrupt the record; it read: %s", rec.State)
	}
	if err := st.SaveRound(mazeGame, mr.roomID, 0, []byte(corrupt)); err != nil {
		t.Fatalf("save: %v", err)
	}

	r2 := newTestRouter(&fakeTTS{})
	r2.store = st
	r2.chat = &fakeChat{}
	r2.overlay = &fakeOverlay{}
	r2.loadMaze()
	if r2.maze != nil {
		t.Error("an unreadable round was resumed rather than dropped")
	}
	if _, ok, _ := st.LoadRound(mazeGame); ok {
		t.Error("the unreadable record was left in the store")
	}
}

func TestMazeAnnouncesEnding(t *testing.T) {
	r, _, chat, _, mr := startTestMaze(t)
	seat(t, r, mr, "bob")
	for mr.round.Phase != maze.PhaseDone {
		cycle(r, mr)
	}
	last := lastSend(chat)
	if !strings.Contains(last, "!maze") {
		t.Errorf("closing line %q does not invite another round", last)
	}
}

// TestMazeClearFreesTheStage covers what happens after the linger. A stage that
// is never released is a bot that can never start another game until a mod
// intervenes, so this drives clearMaze directly rather than waiting out a
// fifteen-second timer.
func TestMazeClearFreesTheStage(t *testing.T) {
	r, st, _, ov, mr := startTestMaze(t)
	seat(t, r, mr, "bob")
	for mr.round.Phase != maze.PhaseDone {
		cycle(r, mr)
	}
	if got := r.liveBoard(); got != boardMaze {
		t.Fatalf("stage=%q before the clear, want maze", got)
	}

	r.clearMaze(mr)

	if r.maze != nil {
		t.Error("round still current after the clear")
	}
	if got := r.liveBoard(); got != boardNone {
		t.Errorf("stage=%q after the clear, want it freed", got)
	}
	if _, ok, _ := st.LoadRound(mazeGame); ok {
		t.Error("the settled round was left in the store")
	}
	p, _ := ov.last("maze")
	if m, ok := p.data.(map[string]any); !ok || m["hidden"] != true {
		t.Errorf("last push=%v, want the board hidden", p.data)
	}
	// The stage being free is the point: another game must now be able to start.
	if ok, live := r.claimBoard(boardWordle); !ok {
		t.Errorf("stage still held by %q; another game cannot start", live)
	}
}

// TestMazeStaleClearLeavesTheNewRoundAlone: a round started during the previous
// one's linger must not be torn down by that older round's timer.
func TestMazeStaleClearLeavesTheNewRoundAlone(t *testing.T) {
	r, _, _, _, old := startTestMaze(t)
	seat(t, r, old, "bob")
	for old.round.Phase != maze.PhaseDone {
		cycle(r, old)
	}
	r.clearMaze(old)

	r.Handle(emsg("alice", "!maze", false))
	fresh := r.maze
	if fresh == nil {
		t.Fatal("could not start a second round")
	}
	t.Cleanup(fresh.halt)
	fresh.halt()

	r.clearMaze(old) // the old round's timer, firing late

	if r.maze != fresh {
		t.Error("a stale clear tore down the new round")
	}
	if got := r.liveBoard(); got != boardMaze {
		t.Errorf("stage=%q, want the new round still holding it", got)
	}
}

// TestMazeGenerateFailureFreesTheStage: the stage is claimed before the board is
// built, so a config that cannot produce a board must hand it back. Forgetting
// that would wedge every game in the bot behind a round that never started.
func TestMazeGenerateFailureFreesTheStage(t *testing.T) {
	r, _, _, chat := econRouter(t)
	r.overlay = &fakeOverlay{}
	r.mazeCfg = defaultMazeConfig()
	r.mazeCfg.Gen.Size = 1 // smaller than any board the generator will build

	r.Handle(emsg("alice", "!maze", false))

	if r.maze != nil {
		t.Error("a round started on a board that could not be generated")
	}
	if got := r.liveBoard(); got != boardNone {
		t.Errorf("stage=%q after a failed start; it must be handed back", got)
	}
	if len(chat.replies) != 1 {
		t.Errorf("replies=%v, want one telling the caller it failed", chat.replies)
	}
}

func TestMazeCommandsAreReserved(t *testing.T) {
	r, _, _, _ := econRouter(t)
	for _, cmd := range []string{"!maze", "!go", "!mazewins"} {
		if !r.isBuiltin(cmd) {
			t.Errorf("%s is not reserved; !addcom could shadow it", cmd)
		}
	}
}

func TestOrdinal(t *testing.T) {
	cases := map[int]string{1: "1st", 2: "2nd", 3: "3rd", 4: "4th", 11: "11th", 12: "12th", 13: "13th", 21: "21st", 22: "22nd"}
	for n, want := range cases {
		if got := ordinal(n); got != want {
			t.Errorf("ordinal(%d)=%q want %q", n, got, want)
		}
	}
}

// --- callouts ---------------------------------------------------------------

// seatedRound returns a round with players seated and racing, for driving
// announce() with hand-built events. Constructing the events directly is what
// makes these tests precise: getting a real board to spring a specific trap on a
// specific cycle would be an elaborate way to test one sentence.
func seatedRound(t *testing.T) (*Router, *fakeChat, *mazeRound) {
	t.Helper()
	r, _, chat, _, mr := startTestMaze(t)
	seat(t, r, mr, "bob", "carol", "dave")
	chat.sends = nil
	return r, chat, mr
}

func ev(kind maze.EventKind, seat, n int, at maze.Cell) maze.Event {
	return maze.Event{Kind: kind, Seat: seat, N: n, At: at}
}

// TestMazeChatCoalescesACycle is the volume rule. Five players on an 8s cycle can
// easily produce four events at once, and four messages every eight seconds for
// three minutes would bury every human conversation in the channel.
func TestMazeChatCoalescesACycle(t *testing.T) {
	_, _, mr := seatedRound(t)

	lines, _ := mr.announce([]maze.Event{
		ev(maze.EventKeyTaken, 0, 1, maze.Cell{X: 1, Y: 1}),
		ev(maze.EventTrapped, 1, 2, maze.Cell{X: 2, Y: 2}),
		ev(maze.EventSpiked, 2, 0, maze.Cell{X: 3, Y: 3}),
	})

	if len(lines) != 1 {
		t.Fatalf("%d chat lines for one cycle, want 1: %q", len(lines), lines)
	}
	for _, want := range []string{"bob", "carol", "dave", "·"} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("coalesced line %q is missing %q", lines[0], want)
		}
	}
}

// TestMazeBonksStayOutOfChat: bonking is far and away the most frequent event, it
// says only that someone guessed a wall, and the board already shows the move was
// lost. It belongs in the panel, not in a chat with nine people in it.
func TestMazeBonksStayOutOfChat(t *testing.T) {
	_, _, mr := seatedRound(t)

	before := len(mr.feed) // seats locking has already logged an entry
	lines, toasts := mr.announce([]maze.Event{
		ev(maze.EventBonked, 0, 0, maze.Cell{X: 1, Y: 1}),
		ev(maze.EventBonked, 1, 0, maze.Cell{X: 2, Y: 2}),
	})

	if len(lines) != 0 {
		t.Errorf("bonks produced chat lines: %q", lines)
	}
	if len(toasts) != 0 {
		t.Errorf("bonks produced toasts: %+v", toasts)
	}
	if got := len(mr.feed) - before; got != 2 {
		t.Errorf("feed grew by %d, want the two bonks logged: %q", got, mr.feed)
	}
}

// TestMazeSpikeAndKeyDropReadAsOneEvent: the engine emits two events when a
// key-carrier is spiked, but to a reader that is one thing happening.
func TestMazeSpikeAndKeyDropReadAsOneEvent(t *testing.T) {
	_, _, mr := seatedRound(t)
	at := maze.Cell{X: 2, Y: 3}

	lines, toasts := mr.announce([]maze.Event{
		ev(maze.EventKeyDropped, 0, 0, at),
		ev(maze.EventSpiked, 0, 0, at),
	})

	if len(lines) != 1 {
		t.Fatalf("%d lines, want 1: %q", len(lines), lines)
	}
	if strings.Count(lines[0], "bob") != 1 {
		t.Errorf("line names the player twice: %q", lines[0])
	}
	for _, want := range []string{"spikes", at.String(), "key"} {
		if !strings.Contains(lines[0], want) {
			t.Errorf("line %q is missing %q", lines[0], want)
		}
	}
	if len(toasts) != 1 || !strings.Contains(toasts[0].Line2, at.String()) {
		t.Errorf("toasts=%+v, want one naming where the key fell", toasts)
	}
}

// TestMazeBounceIsAnnouncedOnce: someone at the door without a key is a moment
// the first time and noise the fourth.
func TestMazeBounceIsAnnouncedOnce(t *testing.T) {
	_, _, mr := seatedRound(t)
	bounce := []maze.Event{ev(maze.EventBounced, 0, 0, maze.Cell{X: 4, Y: 4})}

	first, _ := mr.announce(bounce)
	if len(first) != 1 || !strings.Contains(first[0], "no key") {
		t.Fatalf("first bounce=%q, want it announced", first)
	}
	for i := 0; i < 3; i++ {
		if again, _ := mr.announce(bounce); len(again) != 0 {
			t.Fatalf("bounce %d repeated in chat: %q", i+2, again)
		}
	}
	// A different player still gets their moment.
	if other, _ := mr.announce([]maze.Event{ev(maze.EventBounced, 1, 0, maze.Cell{X: 4, Y: 4})}); len(other) != 1 {
		t.Errorf("a second player's bounce was suppressed: %q", other)
	}
}

// TestMazeLastKeyIsCalledOut: the moment the last key leaves the board is when
// somebody's round is quietly over, and they should hear about it.
func TestMazeLastKeyIsCalledOut(t *testing.T) {
	_, _, mr := seatedRound(t)

	some, _ := mr.announce([]maze.Event{ev(maze.EventKeyTaken, 0, 2, maze.Cell{X: 1, Y: 1})})
	if len(some) != 1 || !strings.Contains(some[0], "2 keys left") {
		t.Errorf("line=%q, want the remaining count", some)
	}
	last, _ := mr.announce([]maze.Event{ev(maze.EventKeyTaken, 1, 0, maze.Cell{X: 1, Y: 2})})
	if len(last) != 1 || !strings.Contains(last[0], "LAST") {
		t.Errorf("line=%q, want the last key called out", last)
	}
}

// TestMazeToastsAreRareByConstruction is a hard constraint, not a preference: the
// overlay shows toasts one at a time for 5.5 seconds each, so more than about one
// per cycle backs the queue up and the "news" arrives minutes after the board has
// moved on. Only spikes and finishes are allowed through.
func TestMazeToastsAreRareByConstruction(t *testing.T) {
	_, _, mr := seatedRound(t)

	_, quiet := mr.announce([]maze.Event{
		ev(maze.EventKeyTaken, 0, 1, maze.Cell{X: 1, Y: 1}),
		ev(maze.EventTrapped, 1, 2, maze.Cell{X: 2, Y: 2}),
		ev(maze.EventFreed, 1, 0, maze.Cell{X: 2, Y: 2}),
		ev(maze.EventBonked, 2, 0, maze.Cell{X: 3, Y: 3}),
		ev(maze.EventBounced, 2, 0, maze.Cell{X: 3, Y: 3}),
	})
	if len(quiet) != 0 {
		t.Errorf("routine events raised %d toasts: %+v", len(quiet), quiet)
	}

	_, loud := mr.announce([]maze.Event{
		ev(maze.EventSpiked, 0, 0, maze.Cell{X: 1, Y: 1}),
		ev(maze.EventFinished, 1, 1, maze.Cell{X: 5, Y: 5}),
	})
	if len(loud) != 2 {
		t.Fatalf("%d toasts for a spike and a win, want 2: %+v", len(loud), loud)
	}
}

func TestMazeFeedIsBounded(t *testing.T) {
	_, _, mr := seatedRound(t)
	for i := 0; i < mazeFeedLines*3; i++ {
		mr.announce([]maze.Event{ev(maze.EventBonked, i%3, 0, maze.Cell{X: 1, Y: 1})})
	}
	if len(mr.feed) != mazeFeedLines {
		t.Errorf("feed holds %d lines, want it capped at %d", len(mr.feed), mazeFeedLines)
	}
}

// playMazeRound drives a round to the end with competent routing, so the volume
// measured below is what a real game produces rather than what random flailing
// produces.
func playMazeRound(t *testing.T, r *Router, mr *mazeRound) int {
	t.Helper()
	cycles := 0
	for mr.round.Phase != maze.PhaseDone && cycles < 200 {
		playOneMazeCycle(r, mr)
		cycles++
	}
	return cycles
}

// playOneMazeCycle routes every racer one step toward what they are after and
// advances a cycle.
func playOneMazeCycle(r *Router, mr *mazeRound) {
	{
		bd := mr.round.Map
		for _, p := range mr.round.Players {
			if !p.Racing() || p.StuckFor > 0 {
				continue
			}
			target := bd.Exit
			if !p.HasKey {
				if ks := mr.round.KeysOnMap(); len(ks) > 0 {
					target = ks[p.Seat%len(ks)]
				}
			}
			dist := bd.Distances(target)
			best, pick, found := dist[p.At.Y*bd.Size+p.At.X], maze.North, false
			for _, d := range []maze.Dir{maze.North, maze.East, maze.South, maze.West} {
				if !bd.Open(p.At, d) {
					continue
				}
				n, _ := bd.Neighbor(p.At, d)
				if v := dist[n.Y*bd.Size+n.X]; v >= 0 && v < best {
					best, pick, found = v, d, true
				}
			}
			if found {
				mr.round.Submit(p.UserID, []maze.Dir{pick}, time.Now())
			}
		}
		r.tickMaze(mr, time.Now())
	}
}

// TestMazeChatVolumeStaysWithinBudget is the design constraint from the PRD
// measured against a real round rather than asserted in prose. The target is
// roughly one line per two or three cycles; a bot that talks every cycle for
// three minutes drowns a small chat.
func TestMazeChatVolumeStaysWithinBudget(t *testing.T) {
	worst := 0.0
	for seed := 0; seed < 12; seed++ {
		r, _, chat, _, mr := startTestMaze(t)
		seat(t, r, mr, "bob", "carol", "dave", "erin", "finn")
		cycles := playMazeRound(t, r, mr)
		if cycles < 5 {
			t.Fatalf("round %d ended in %d cycles; nothing to measure", seed, cycles)
		}
		perCycle := float64(len(chat.sends)) / float64(cycles)
		if perCycle > worst {
			worst = perCycle
		}
		if perCycle > 1.0 {
			t.Errorf("round %d: %d chat lines over %d cycles (%.2f per cycle) — the bot is talking every cycle",
				seed, len(chat.sends), cycles, perCycle)
		}
	}
	t.Logf("worst observed chat volume: %.2f lines per cycle", worst)
}

// --- payout -----------------------------------------------------------------

func TestMazePayoutCurve(t *testing.T) {
	cases := []struct {
		reward int64
		place  int
		want   int64
	}{
		{100, 1, 100}, {100, 2, 50}, {100, 3, 25}, {100, 4, 12}, {100, 5, 6},
		// The floor is what makes finishing at all beat not finishing, however
		// small the reward or however far down the field.
		{1, 4, 1}, {3, 9, 1}, {100, 40, 1},
		// No economy, no payout.
		{0, 1, 0}, {-5, 1, 0},
		{100, 0, 0},
	}
	for _, c := range cases {
		if got := mazePayout(c.reward, c.place); got != c.want {
			t.Errorf("mazePayout(%d, %d)=%d want %d", c.reward, c.place, got, c.want)
		}
	}
	// Every place pays strictly less than the one above it, until the floor.
	prev := mazePayout(100, 1)
	for place := 2; place <= 5; place++ {
		got := mazePayout(100, place)
		if got >= prev {
			t.Errorf("place %d pays %d, not less than place %d's %d", place, got, place-1, prev)
		}
		if got < 1 {
			t.Errorf("place %d pays %d — finishing must beat not finishing", place, got)
		}
		prev = got
	}
}

// payingMaze starts a round with the maze economy switched on.
func payingMaze(t *testing.T) (*Router, Store, *fakeChat, *mazeRound) {
	t.Helper()
	r, st, chat, _, mr := startTestMaze(t)
	r.econ.MazeReward = 100
	return r, st, chat, mr
}

// TestMazePaysEveryFinisherAndTalliesTheWinner covers the whole settle path
// against a round actually played out, rather than against hand-built events.
func TestMazePaysEveryFinisherAndTalliesTheWinner(t *testing.T) {
	r, st, chat, mr := payingMaze(t)
	seat(t, r, mr, "bob", "carol", "dave", "erin", "finn")
	playMazeRound(t, r, mr)

	places := mr.round.Placements()
	if len(places) < 2 {
		t.Fatalf("%d finishers; this test needs a winner and at least one placer", len(places))
	}
	t.Logf("%d of %d players got out", len(places), len(mr.round.Players))
	for _, p := range places {
		want := mazePayout(100, p.Place)
		got, err := st.Balance(p.UserID)
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("%s finished %s with balance %d, want %d", p.Login, ordinal(p.Place), got, want)
		}
	}
	// Anyone who did not get out is paid nothing.
	for _, p := range mr.round.Players {
		if p.Racing() {
			if bal, _ := st.Balance(p.UserID); bal != 0 {
				t.Errorf("%s never escaped but has %d marks", p.Login, bal)
			}
		}
	}

	lb, err := st.MazeLeaderboard(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(lb) != 1 {
		t.Fatalf("leaderboard=%+v want exactly the winner — placements are paid, not tallied", lb)
	}
	if lb[0].Display != places[0].Display || lb[0].Wins != 1 {
		t.Errorf("leaderboard[0]=%+v want %s with 1 win", lb[0], places[0].Display)
	}
	if !strings.Contains(strings.Join(chat.sends, "\n"), "win #1") {
		t.Errorf("no win announcement in chat: %q", chat.sends)
	}
}

// TestMazeSkipCannotUnpayAWinner is the reason the payout happens at the finish
// rather than when the round settles. Getting through the door is earned the
// moment it happens, and a mod ending the round during the placement scramble
// must not be able to take it back.
func TestMazeSkipCannotUnpayAWinner(t *testing.T) {
	r, st, _, mr := payingMaze(t)
	seat(t, r, mr, "bob", "carol", "dave")

	// Play only until somebody wins, leaving the round live in its placement window.
	for i := 0; i < 60 && mr.round.Phase == maze.PhaseRacing; i++ {
		playOneMazeCycle(r, mr)
		if len(mr.round.Placements()) > 0 {
			break
		}
	}
	places := mr.round.Placements()
	if len(places) == 0 {
		t.Fatal("nobody won; nothing to protect")
	}
	winner := places[0]
	if bal, _ := st.Balance(winner.UserID); bal != 100 {
		t.Fatalf("winner balance=%d at the moment of winning, want 100", bal)
	}

	r.Handle(emsg("chan", "!skipgame", true))

	if bal, _ := st.Balance(winner.UserID); bal != 100 {
		t.Errorf("winner balance=%d after !skipgame, want the 100 they already earned", bal)
	}
	if lb, _ := st.MazeLeaderboard(10); len(lb) != 1 {
		t.Errorf("leaderboard=%+v — a skip erased a win that had already happened", lb)
	}
}

// TestMazeTalliesTheWinEvenWithTheEconomyOff mirrors wordle: the leaderboard is
// about who plays well, not about marks.
func TestMazeTalliesTheWinEvenWithTheEconomyOff(t *testing.T) {
	r, st, _, _, mr := startTestMaze(t)
	r.economy = false
	r.econ.MazeReward = 100
	seat(t, r, mr, "bob", "carol", "dave")
	playMazeRound(t, r, mr)

	places := mr.round.Placements()
	if len(places) == 0 {
		t.Fatal("nobody finished")
	}
	if bal, _ := st.Balance(places[0].UserID); bal != 0 {
		t.Errorf("winner was paid %d with the economy off", bal)
	}
	if lb, _ := st.MazeLeaderboard(10); len(lb) != 1 {
		t.Errorf("leaderboard=%+v want the win tallied regardless of the economy", lb)
	}
}

// TestMazePlacementWindowProducesPlacements guards the thing that was silently
// broken: with the window too short, every round ended with exactly one finisher,
// so the scramble for second and third never happened and the payout curve below
// first place was unreachable. Nothing failed — the game just quietly stopped
// having a feature.
func TestMazePlacementWindowProducesPlacements(t *testing.T) {
	multi := 0
	const rounds = 8
	for i := 0; i < rounds; i++ {
		r, _, _, _, mr := startTestMaze(t)
		seat(t, r, mr, "bob", "carol", "dave", "erin", "finn")
		playMazeRound(t, r, mr)
		if len(mr.round.Placements()) > 1 {
			multi++
		}
	}
	if multi == 0 {
		t.Errorf("no round in %d produced a second place — placement_cycles is too short for anyone to reach the exit behind the winner", rounds)
	}
}

func TestMazeWinsLeaderboard(t *testing.T) {
	r, _, st, chat := econRouter(t)
	if _, err := st.MazeAddWin("id-bob", "bob", "Bob"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.MazeAddWin("id-bob", "bob", "Bob"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.MazeAddWin("id-amy", "amy", "Amy"); err != nil {
		t.Fatal(err)
	}

	r.Handle(emsg("carol", "!mazewins", false))
	if len(chat.replies) != 1 {
		t.Fatalf("replies=%v want one leaderboard line", chat.replies)
	}
	got := chat.replies[0].text
	if !strings.Contains(got, "1. Bob 2") || !strings.Contains(got, "2. Amy 1") {
		t.Errorf("leaderboard=%q want Bob(2) then Amy(1)", got)
	}
}

func TestMazeWinsEmptyLeaderboard(t *testing.T) {
	r, _, _, chat := econRouter(t)
	r.Handle(emsg("bob", "!mazewins", false))
	if len(chat.replies) != 1 || !strings.Contains(chat.replies[0].text, "!maze") {
		t.Errorf("replies=%v want an empty-leaderboard nudge pointing at !maze", chat.replies)
	}
}
