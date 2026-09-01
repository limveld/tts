package main

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"testing"
	"time"

	"tts/internal/maze"
	"tts/store"
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
		r.Handle(emsg(u, "!up", false))
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
	if len(chat.sends) != 1 || !strings.Contains(chat.sends[0], "!up") {
		t.Errorf("open announcement=%v, want one line naming the direction commands", chat.sends)
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
		r.Handle(emsg(u, "!up", false))
	}
	if got := len(mr.round.Players); got != 5 {
		t.Fatalf("%d seated, want 5", got)
	}

	chat.replies = nil
	r.Handle(emsg("f", "!up", false))
	if got := len(mr.round.Players); got != 5 {
		t.Errorf("%d seated after a sixth join, want 5", got)
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
	r.Handle(emsg("latecomer", "!up", false))
	if len(chat.replies) != 1 || !strings.Contains(chat.replies[0].text, "locked") {
		t.Errorf("replies=%v, want a late joiner told seats are locked", chat.replies)
	}
	if got := len(mr.round.Players); got != 2 {
		t.Errorf("%d players after a late join, want 2", got)
	}
}

// TestMazeMovesAreSilent guards the decision that a move gets no chat reply. Five
// players on an 8s cycle would otherwise put ~35 confirmations into a chat with
// fewer than ten people in it.
func TestMazeMovesAreSilent(t *testing.T) {
	r, _, chat, _, mr := startTestMaze(t)
	seat(t, r, mr, "bob", "carol")
	sends, replies := len(chat.sends), len(chat.replies)

	r.Handle(emsg("bob", "!up", false))
	r.Handle(emsg("carol", "!down", false))

	if len(chat.sends) != sends || len(chat.replies) != replies {
		t.Errorf("a move spoke: sends+%d replies+%d",
			len(chat.sends)-sends, len(chat.replies)-replies)
	}
	p, _ := mr.round.PlayerBy("id-bob")
	if d, ok := p.NextDir(); !ok || d != maze.North {
		t.Errorf("bob has %v/%v locked in, want up", d, ok)
	}
}

func TestMazeMoveWithNoRoundPointsAtMaze(t *testing.T) {
	r, _, _, chat := econRouter(t)
	r.overlay = &fakeOverlay{}

	r.Handle(emsg("bob", "!up", false))
	if len(chat.replies) != 1 || !strings.Contains(chat.replies[0].text, "!maze") {
		t.Fatalf("replies=%v, want a move with no round to point at !maze", chat.replies)
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
		r.Handle(emsg("bob", "!up", false))
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

// TestMazePayloadCarriesTheChosenDirection reverses what this test used to
// assert. The direction was withheld so a viewer on a fast connection could not
// read a slower player's intent and counter it inside the cycle; playtesting
// found that on a stream this size the interception is theoretical and the
// feedback is not.
func TestMazePayloadCarriesTheChosenDirection(t *testing.T) {
	r, _, _, ov, mr := startTestMaze(t)
	seat(t, r, mr, "bob")
	r.Handle(emsg("bob", "!left", false))

	b := lastMazeBoard(t, ov)
	var found bool
	for _, p := range b.Players {
		if p.Name == "bob" {
			found = true
			if !p.Locked {
				t.Errorf("bob has a move in but Locked=false")
			}
			if p.Move != "left" {
				t.Errorf("Move=%q want %q", p.Move, "left")
			}
		}
	}
	if !found {
		t.Fatalf("bob is not in the payload: %+v", b.Players)
	}

	// And it clears once the move is spent, so the HUD is never stale.
	cycle(r, mr)
	b = lastMazeBoard(t, ov)
	for _, p := range b.Players {
		if p.Name == "bob" && (p.Locked || p.Move != "") {
			t.Errorf("after the cycle bob still shows %q locked=%v", p.Move, p.Locked)
		}
	}
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
		r.Handle(emsg("bob", "!up", false))
		r.Handle(emsg("carol", "!down", false))
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
	for _, cmd := range []string{"!maze", "!up", "!down", "!left", "!right", "!mazewins"} {
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

	lines, _, _ := mr.announce([]maze.Event{
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
	lines, _, toasts := mr.announce([]maze.Event{
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

	lines, _, toasts := mr.announce([]maze.Event{
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

	first, _, _ := mr.announce(bounce)
	if len(first) != 1 || !strings.Contains(first[0], "no key") {
		t.Fatalf("first bounce=%q, want it announced", first)
	}
	for i := 0; i < 3; i++ {
		if again, _, _ := mr.announce(bounce); len(again) != 0 {
			t.Fatalf("bounce %d repeated in chat: %q", i+2, again)
		}
	}
	// A different player still gets their moment.
	if other, _, _ := mr.announce([]maze.Event{ev(maze.EventBounced, 1, 0, maze.Cell{X: 4, Y: 4})}); len(other) != 1 {
		t.Errorf("a second player's bounce was suppressed: %q", other)
	}
}

// TestMazeLastKeyIsCalledOut: the moment the last key leaves the board is when
// somebody's round is quietly over, and they should hear about it.
func TestMazeLastKeyIsCalledOut(t *testing.T) {
	_, _, mr := seatedRound(t)

	some, _, _ := mr.announce([]maze.Event{ev(maze.EventKeyTaken, 0, 2, maze.Cell{X: 1, Y: 1})})
	if len(some) != 1 || !strings.Contains(some[0], "2 keys left") {
		t.Errorf("line=%q, want the remaining count", some)
	}
	last, _, _ := mr.announce([]maze.Event{ev(maze.EventKeyTaken, 1, 0, maze.Cell{X: 1, Y: 2})})
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

	_, _, quiet := mr.announce([]maze.Event{
		ev(maze.EventKeyTaken, 0, 1, maze.Cell{X: 1, Y: 1}),
		ev(maze.EventTrapped, 1, 2, maze.Cell{X: 2, Y: 2}),
		ev(maze.EventFreed, 1, 0, maze.Cell{X: 2, Y: 2}),
		ev(maze.EventBonked, 2, 0, maze.Cell{X: 3, Y: 3}),
		ev(maze.EventBounced, 2, 0, maze.Cell{X: 3, Y: 3}),
	})
	if len(quiet) != 0 {
		t.Errorf("routine events raised %d toasts: %+v", len(quiet), quiet)
	}

	_, _, loud := mr.announce([]maze.Event{
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
				mr.round.Submit(p.UserID, pick, time.Now())
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

// TestMazeOpenBannerNamesRealSeconds: shortDuration rounds to the nearest minute
// — it is built for uptime and follow-age — so every join window this game will
// ever have rendered as "0m".
func TestMazeOpenBannerNamesRealSeconds(t *testing.T) {
	_, _, chat, _, mr := startTestMaze(t)
	want := time.Duration(mr.round.Cfg.JoinCycles) * mr.cfg.Tick

	if len(chat.sends) == 0 {
		t.Fatal("no opening announcement")
	}
	got := chat.sends[0]
	if strings.Contains(got, "0m") {
		t.Errorf("opening line says %q — the join window is %v, not zero", got, want)
	}
	if !strings.Contains(got, fmt.Sprintf("%.0fs", want.Seconds())) {
		t.Errorf("opening line %q should name the %v join window in seconds", got, want)
	}
}

// TestMazeClosingSummaryFollowsTheFinishes. On the last cycle the final players
// get out and the round ends together, and the closing line names the placements
// — so announcing it before the finishes that produced them reads backwards.
func TestMazeClosingSummaryFollowsTheFinishes(t *testing.T) {
	r, _, chat, _, mr := startTestMaze(t)
	seat(t, r, mr, "bob", "carol", "dave", "erin", "finn")
	playMazeRound(t, r, mr)

	lastFinish, roundOver := -1, -1
	for i, line := range chat.sends {
		if strings.Contains(line, "escapes in") || strings.Contains(line, "is OUT of the maze") {
			lastFinish = i
		}
		if strings.Contains(line, "Round over") {
			roundOver = i
		}
	}
	if lastFinish < 0 || roundOver < 0 {
		t.Fatalf("expected both a finish and a closing line; got %q", chat.sends)
	}
	if roundOver < lastFinish {
		t.Errorf("the closing summary (line %d) is announced before the last finish (line %d):\n%s",
			roundOver, lastFinish, strings.Join(chat.sends, "\n"))
	}
}

// --- playtest regressions ---------------------------------------------------

// TestMazeJoiningDoesNotAlsoMove is the bug the first live round found, and the
// root cause of every complaint from it.
//
// The command that takes your seat used to bank a move as well, so a player's
// very first message moved them a cell they had not asked for. Their sprite was
// then permanently one step from where they believed it was — which is why, at
// what they thought was the board edge, "left" walked them into the exit instead
// of bonking a wall.
func TestMazeJoiningDoesNotAlsoMove(t *testing.T) {
	r, _, _, _, mr := startTestMaze(t)

	r.Handle(emsg("bob", "!up", false))
	p, ok := mr.round.PlayerBy("id-bob")
	if !ok {
		t.Fatal("!up during the join window did not seat anyone")
	}
	if p.Queued() != 0 {
		t.Fatalf("joining also banked a move (%d queued)", p.Queued())
	}

	// Run the join window out; the player must still be exactly where they started.
	for mr.round.Phase == maze.PhaseJoining {
		cycle(r, mr)
	}
	if p.At != mr.round.Map.Start {
		t.Fatalf("player moved to %v before the race began, want the start %v", p.At, mr.round.Map.Start)
	}

	// Their first move once racing is their first move, and it is worth one cell.
	r.Handle(emsg("bob", "!up", false))
	before := p.At
	cycle(r, mr)
	if p.At == before {
		t.Fatal("the first racing move did nothing")
	}
	if d := abs(p.At.Y-before.Y) + abs(p.At.X-before.X); d != 1 {
		t.Errorf("moved %d cells in one cycle, want exactly 1 (%v -> %v)", d, before, p.At)
	}
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// TestMazeFullScreenIsOwnerOnly: full-stage covers whatever else is on screen for
// several minutes, so it is the channel owner's call — but asking for it without
// being the owner still gets you a round, in the corner.
func TestMazeFullScreenIsOwnerOnly(t *testing.T) {
	t.Run("owner gets it", func(t *testing.T) {
		r, _, _, _ := econRouter(t)
		r.overlay = &fakeOverlay{}
		m := emsg("chan", "!maze full", true)
		m.IsBroadcaster = true
		r.Handle(m)
		if r.maze == nil {
			t.Fatal("round did not start")
		}
		t.Cleanup(r.maze.halt)
		r.maze.halt()
		if got := r.maze.cfg.Display; got != "full" {
			t.Errorf("display=%q want full", got)
		}
	})

	t.Run("a mod does not", func(t *testing.T) {
		r, _, _, chat := econRouter(t)
		r.overlay = &fakeOverlay{}
		m := emsg("amod", "!maze full", true) // mod, but not the broadcaster
		r.Handle(m)
		if r.maze == nil {
			t.Fatal("a non-owner asking for full should still get a round")
		}
		t.Cleanup(r.maze.halt)
		r.maze.halt()
		if got := r.maze.cfg.Display; got != "panel" {
			t.Errorf("display=%q want panel", got)
		}
		if len(chat.replies) != 1 || !strings.Contains(chat.replies[0].text, "owner") {
			t.Errorf("replies=%v, want one saying full-screen is the owner's call", chat.replies)
		}
	})

	t.Run("bare !maze is a panel", func(t *testing.T) {
		r, _, _, _, mr := startTestMaze(t)
		_ = r
		if got := mr.cfg.Display; got != "panel" {
			t.Errorf("display=%q want panel by default", got)
		}
	})
}

// slowChat blocks on every send, standing in for a wedged Helix.
type slowChat struct {
	mu    sync.Mutex
	sends []string
	delay time.Duration
}

func (s *slowChat) Send(broadcasterID, text string) error {
	time.Sleep(s.delay)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sends = append(s.sends, text)
	return nil
}

func (s *slowChat) Reply(broadcasterID, parentID, text string) error {
	return s.Send(broadcasterID, text)
}

func (s *slowChat) lines() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.sends...)
}

// TestMazeTickerDoesNotCollapseWhenChatIsSlow drives the real ticker, which every
// other test deliberately halts — and which is why this went unnoticed.
//
// Chat sends used to run inline on the ticker goroutine, each an HTTPS call with
// a timeout longer than a cycle. Go's ticker buffers one tick, so an overrun
// delivered the next tick the instant the slow one returned: two cycles resolving
// back to back and a sprite moving twice with no input. The assertion is on the
// gap between overlay pushes, which happen once per cycle on the ticker.
func TestMazeTickerDoesNotCollapseWhenChatIsSlow(t *testing.T) {
	r, _, _, _ := econRouter(t)
	ov := &fakeOverlay{}
	r.overlay = ov
	slow := &slowChat{delay: 90 * time.Millisecond}
	r.chat = slow
	r.startMazeChat()

	const tick = 30 * time.Millisecond
	cfg := defaultMazeConfig()
	cfg.Tick = tick
	r.mazeCfg = cfg

	r.Handle(emsg("alice", "!maze", false))
	mr := r.maze
	if mr == nil {
		t.Fatal("round did not start")
	}
	defer mr.halt()
	r.Handle(emsg("bob", "!up", false))

	// Let the real ticker run for a while.
	deadline := time.Now().Add(600 * time.Millisecond)
	for time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	mr.halt()

	// Only pushes where the cycle actually advanced are cycles: starting a round
	// and locking in a move both push as well, so that the board is live rather
	// than redrawn once a tick.
	ov.mu.Lock()
	type mark struct {
		cycle int
		at    time.Time
	}
	var cycles []mark
	for _, p := range ov.pushes {
		b, ok := p.data.(mazeBoard)
		if !ok {
			continue
		}
		if len(cycles) == 0 || b.Cycle != cycles[len(cycles)-1].cycle {
			cycles = append(cycles, mark{b.Cycle, p.at})
		}
	}
	ov.mu.Unlock()

	if len(cycles) < 4 {
		t.Fatalf("only %d cycles ran; the ticker is not driving the round", len(cycles))
	}
	for i := 1; i < len(cycles); i++ {
		if gap := cycles[i].at.Sub(cycles[i-1].at); gap < tick/2 {
			t.Fatalf("cycles %d and %d resolved %v apart, less than half the %v tick — the ticker collapsed",
				cycles[i-1].cycle, cycles[i].cycle, gap, tick)
		}
	}
}

// TestMazeNarrationKeepsItsOrder: the queue may drop a line when Helix is wedged,
// but it must never reorder one. "@bob took the last key" after "@bob is OUT"
// would read as a different round.
func TestMazeNarrationKeepsItsOrder(t *testing.T) {
	r, _, _, _ := econRouter(t)
	slow := &slowChat{delay: 2 * time.Millisecond}
	r.chat = slow
	r.startMazeChat()

	want := []string{"first", "second", "third", "fourth", "fifth"}
	for _, line := range want {
		r.sendMaze("room1", line)
	}
	for i := 0; i < 100 && len(slow.lines()) < len(want); i++ {
		time.Sleep(10 * time.Millisecond)
	}
	got := slow.lines()
	if len(got) != len(want) {
		t.Fatalf("got %d lines, want %d: %q", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("line %d = %q want %q (order is not preserved): %q", i, got[i], want[i], got)
		}
	}
}

// TestSFXNamesMayNotShadowCommands: sounds dispatch before the command engine, so
// a sound named "up" does not conflict with !up — it wins, and !up stops
// existing. A viewer's move would play an airhorn, with nothing logged.
func TestSFXNamesMayNotShadowCommands(t *testing.T) {
	r, _, _, _ := econRouter(t)

	if err := r.checkSFXNames(); err != nil {
		t.Fatalf("the test router's sounds should be clean: %v", err)
	}

	r.sfx["!up"] = struct{}{}
	r.sfx["!wordle"] = struct{}{}
	err := r.checkSFXNames()
	if err == nil {
		t.Fatal("a sound shadowing a command was accepted")
	}
	for _, want := range []string{"!up", "!wordle"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should name %s", err, want)
		}
	}
}

// --- replay log -------------------------------------------------------------

// mazeLogReader is the archive's read side.
//
// The bot's Store interface deliberately does not carry it: the bot only ever
// writes the archive, and that interface is the consumer's view of what it needs
// rather than everything a backend can do. The tests read back through the
// concrete backend instead of widening production for their own benefit.
type mazeLogReader interface {
	MazeRoundLog(n int) ([]store.MazeRound, error)
	MazeRoundEvents(id string) ([]store.MazeEvent, error)
}

func mazeLog(t *testing.T, st Store) mazeLogReader {
	t.Helper()
	rd, ok := st.(mazeLogReader)
	if !ok {
		t.Fatalf("%T cannot read the maze archive", st)
	}
	return rd
}

// TestMazeArchivesAFinishedRound is the round trip: play one out, then read it
// back from the archive and check it describes the same game.
func TestMazeArchivesAFinishedRound(t *testing.T) {
	r, st, _, _, mr := startTestMaze(t)
	r.econ.MazeReward = 100
	seat(t, r, mr, "bob", "carol", "dave", "erin", "finn")
	r.Handle(emsg("bob", "!left", false))
	playMazeRound(t, r, mr)

	rounds, err := mazeLog(t, st).MazeRoundLog(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rounds) != 1 {
		t.Fatalf("%d archived rounds, want 1", len(rounds))
	}
	got := rounds[0]
	if got.Seed != mr.round.Map.Seed {
		t.Errorf("seed = %d want %d", got.Seed, mr.round.Map.Seed)
	}
	if got.Players != 5 {
		t.Errorf("players = %d want 5", got.Players)
	}
	if want := len(mr.round.Placements()); got.Finishers != want {
		t.Errorf("finishers = %d want %d", got.Finishers, want)
	}
	if got.Cycles != mr.round.Cycle {
		t.Errorf("cycles = %d want %d", got.Cycles, mr.round.Cycle)
	}
	if got.Reason == "" || got.Reason == "skipped" {
		t.Errorf("reason = %q, want the engine's own end reason", got.Reason)
	}
	if places := mr.round.Placements(); len(places) > 0 && got.WinnerLogin != places[0].Login {
		t.Errorf("winner = %q want %q", got.WinnerLogin, places[0].Login)
	}

	// The replay document must actually be replayable: the board has to be in it,
	// not merely a seed to rebuild one from.
	var doc mazeReplay
	if err := json.Unmarshal(got.Input, &doc); err != nil {
		t.Fatalf("input is not a replay document: %v", err)
	}
	if len(doc.Initial.Board.Walls) != mr.round.Map.Size*mr.round.Map.Size {
		t.Errorf("replay carries %d wall masks, want a whole board", len(doc.Initial.Board.Walls))
	}
	if len(doc.Moves) == 0 {
		t.Error("replay carries no moves; the round cannot be re-run from it")
	}
	if _, err := maze.Restore(doc.Initial); err != nil {
		t.Errorf("the archived board does not restore: %v", err)
	}

	evs, err := mazeLog(t, st).MazeRoundEvents(got.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) == 0 {
		t.Fatal("no events archived")
	}
	for i, e := range evs {
		if e.Seq != i {
			t.Fatalf("event %d has seq %d — the log is not in emission order", i, e.Seq)
		}
		if e.Kind == "" {
			t.Errorf("event %d has no kind: %+v", i, e)
		}
		// Round-level events have no board position, and must not have picked up
		// the zero cell's "A1".
		if (e.Kind == "seats-locked" || e.Kind == "round-ended") && e.At != "" {
			t.Errorf("%s event carries a position %q", e.Kind, e.At)
		}
	}
}

// TestMazeArchivesASkippedRound: a moderator cutting a round short still happened,
// and the engine never reaches PhaseDone on that path — so it has no end reason of
// its own and would otherwise vanish entirely.
func TestMazeArchivesASkippedRound(t *testing.T) {
	r, st, _, _, mr := startTestMaze(t)
	seat(t, r, mr, "bob", "carol")
	for i := 0; i < 3; i++ {
		r.Handle(emsg("bob", "!up", false))
		cycle(r, mr)
	}

	r.Handle(emsg("chan", "!skipgame", true))

	rounds, err := mazeLog(t, st).MazeRoundLog(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rounds) != 1 {
		t.Fatalf("%d archived rounds after a skip, want 1", len(rounds))
	}
	if rounds[0].Reason != "skipped" {
		t.Errorf("reason = %q want skipped", rounds[0].Reason)
	}
	if evs, _ := mazeLog(t, st).MazeRoundEvents(rounds[0].ID); len(evs) == 0 {
		t.Error("a skipped round archived no events, losing everything that happened before the skip")
	}
}

// TestMazeArchivesARoundThatEndedAsTheBotDied covers the end path that looks
// least like one: the bot comes back up, finds a round already finished on disk,
// and today would simply delete it.
func TestMazeArchivesARoundThatEndedAsTheBotDied(t *testing.T) {
	r, st, _, _, mr := startTestMaze(t)
	seat(t, r, mr, "bob")
	// Suppress the in-process archive, so the round reaches PhaseDone on disk
	// having never been written — which is exactly the state a bot that died
	// between the two leaves behind. `logged` is not persisted, so the restoring
	// router sees a round it has never archived.
	mr.logged = true
	for mr.round.Phase != maze.PhaseDone {
		cycle(r, mr)
	}
	r.persistMaze(mr)
	if _, ok, _ := st.LoadRound(mazeGame); !ok {
		t.Fatal("no stored round to restore")
	}
	if before, _ := mazeLog(t, st).MazeRoundLog(10); len(before) != 0 {
		t.Fatalf("%d rounds archived already; this test needs an unarchived one", len(before))
	}

	r2 := newTestRouter(&fakeTTS{})
	r2.store = st
	r2.chat = &fakeChat{}
	r2.overlay = &fakeOverlay{}
	r2.loadMaze()

	after, err := mazeLog(t, st).MazeRoundLog(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 1 {
		t.Fatalf("%d archived rounds after restore, want 1 — a round that ended as the bot died was thrown away", len(after))
	}
	if r2.maze != nil {
		t.Error("a finished round was resumed")
	}
}

// TestMazeArchiveIsWrittenOnce: several paths end a round, and more than one can
// reach the same one.
func TestMazeArchiveIsWrittenOnce(t *testing.T) {
	r, st, _, _, mr := startTestMaze(t)
	seat(t, r, mr, "bob")
	for mr.round.Phase != maze.PhaseDone {
		cycle(r, mr)
	}
	r.archiveMaze(mr, "")
	r.archiveMaze(mr, "skipped")

	rounds, _ := mazeLog(t, st).MazeRoundLog(10)
	if len(rounds) != 1 {
		t.Fatalf("%d archived rounds, want 1", len(rounds))
	}
	if rounds[0].Reason == "skipped" {
		t.Error("a second archive call overwrote the real end reason")
	}
}

// TestMazeLogSurvivesARestart: the accumulating record rides in the round document
// rather than in memory, so a bot that restarts mid-round still has the first half
// of the game when it comes to archive it.
func TestMazeLogSurvivesARestart(t *testing.T) {
	r, st, _, _, mr := startTestMaze(t)
	seat(t, r, mr, "bob", "carol")
	for i := 0; i < 4; i++ {
		r.Handle(emsg("bob", "!up", false))
		r.Handle(emsg("carol", "!down", false))
		cycle(r, mr)
	}
	wantEvents, wantMoves := len(mr.log), len(mr.moves)
	if wantEvents == 0 || wantMoves == 0 {
		t.Fatalf("nothing accumulated: %d events, %d moves", wantEvents, wantMoves)
	}

	r2 := newTestRouter(&fakeTTS{})
	r2.store = st
	r2.chat = &fakeChat{}
	r2.overlay = &fakeOverlay{}
	r2.loadMaze()
	if r2.maze == nil {
		t.Fatal("round not restored")
	}
	t.Cleanup(r2.maze.halt)
	r2.maze.halt()

	if got := len(r2.maze.log); got != wantEvents {
		t.Errorf("%d events survived the restart, want %d", got, wantEvents)
	}
	if got := len(r2.maze.moves); got != wantMoves {
		t.Errorf("%d moves survived the restart, want %d", got, wantMoves)
	}
}

// TestMazeRosterNamesEachPlayersColour: the board is five small squares, and this
// line is the only seat confirmation chat gets — joins are coalesced rather than
// answered one by one — so it is where a player finds out which square is theirs.
func TestMazeRosterNamesEachPlayersColour(t *testing.T) {
	r, _, chat, _, mr := startTestMaze(t)
	seat(t, r, mr, "bob", "carol", "dave")

	roster := chat.sends[len(chat.sends)-1]
	if !strings.Contains(roster, "Seats locked") {
		t.Fatalf("last line is not the roster: %q", roster)
	}
	for _, p := range mr.round.Players {
		_, emoji := mazeSeat(p.Seat)
		if !strings.Contains(roster, emoji+" @"+p.Display) {
			t.Errorf("roster %q does not give %s their colour %s", roster, p.Display, emoji)
		}
	}
}

// TestMazeSeatColoursAreOnePalette is the point of moving the palette into the
// bot. The emoji chat is told and the swatch drawn on the board are the same fact,
// and a player told they are red who then sees no red dot has been lied to by a
// mismatch nothing else would surface.
func TestMazeSeatColoursAreOnePalette(t *testing.T) {
	r, _, _, ov, mr := startTestMaze(t)
	seat(t, r, mr, "bob", "carol", "dave", "erin", "finn")

	b := lastMazeBoard(t, ov)
	if len(b.Players) != len(mr.round.Players) {
		t.Fatalf("%d players in the payload, want %d", len(b.Players), len(mr.round.Players))
	}
	seen := map[string]int{}
	for _, p := range b.Players {
		hex, _ := mazeSeat(p.Seat)
		if p.Color != hex {
			t.Errorf("seat %d renders %s but the roster calls it %s", p.Seat, p.Color, hex)
		}
		if p.Color == "" {
			t.Errorf("seat %d has no colour; the renderer would fall back and the roster would be wrong", p.Seat)
		}
		seen[p.Color]++
	}
	// Every seat a distinct colour, or two players are the same square.
	for hex, n := range seen {
		if n > 1 {
			t.Errorf("%d players share colour %s", n, hex)
		}
	}
}

// TestMazeSeatWrapsPastThePalette: max_seats is configurable and the palette is
// five long, so seat 5 has to come back round rather than panic.
func TestMazeSeatWrapsPastThePalette(t *testing.T) {
	for _, n := range []int{0, 4, 5, 11} {
		hex, emoji := mazeSeat(n)
		if hex == "" || emoji == "" {
			t.Errorf("seat %d has no colour: %q/%q", n, hex, emoji)
		}
	}
	if h0, e0 := mazeSeat(0); func() bool { h, e := mazeSeat(len(mazeSeats)); return h != h0 || e != e0 }() {
		t.Error("seat wrapping does not return to the first colour")
	}
}

// TestMazeRoundIDIsStableAndMatchesTheArchive. The renderer keys its per-round
// state on this id, so it has to be the same string for every push of a round and
// a different one for the next; and it is the archive's primary key, so the two
// must agree or a round on screen cannot be matched to its record.
func TestMazeRoundIDIsStableAndMatchesTheArchive(t *testing.T) {
	r, st, _, ov, mr := startTestMaze(t)
	seat(t, r, mr, "bob")

	first := lastMazeBoard(t, ov).RoundID
	if first == "" {
		t.Fatal("the payload carries no round id; the renderer would fall back to guessing")
	}
	for i := 0; i < 3; i++ {
		r.Handle(emsg("bob", "!up", false))
		cycle(r, mr)
		if got := lastMazeBoard(t, ov).RoundID; got != first {
			t.Fatalf("round id changed mid-round: %q then %q", first, got)
		}
	}

	for mr.round.Phase != maze.PhaseDone {
		cycle(r, mr)
	}
	rounds, err := mazeLog(t, st).MazeRoundLog(10)
	if err != nil || len(rounds) != 1 {
		t.Fatalf("archive: %d rounds err=%v", len(rounds), err)
	}
	if rounds[0].ID != first {
		t.Errorf("archived as %q but shown as %q — a watched round cannot be matched to its record",
			rounds[0].ID, first)
	}
}

// TestMazeRoundIDsDifferBetweenRounds is the property the trap-fade state depends
// on: without it the renderer carries one round's spent traps into the next, where
// they would never be drawn — on cells that may genuinely be trapped this time.
func TestMazeRoundIDsDifferBetweenRounds(t *testing.T) {
	r, _, _, ov, mr := startTestMaze(t)
	seat(t, r, mr, "bob")
	first := lastMazeBoard(t, ov).RoundID
	for mr.round.Phase != maze.PhaseDone {
		cycle(r, mr)
	}
	r.clearMaze(mr)

	r.Handle(emsg("alice", "!maze", false))
	if r.maze == nil {
		t.Fatal("second round did not start")
	}
	t.Cleanup(r.maze.halt)
	r.maze.halt()

	if second := lastMazeBoard(t, ov).RoundID; second == first {
		t.Errorf("both rounds are %q — the renderer cannot tell them apart", second)
	}
}

// TestMazeEveryDirectionSpelling walks the whole alias table. Each direction has
// three spellings so a player can alternate: Twitch drops a message identical to
// their previous one inside thirty seconds, and this game asks people to walk
// straight corridors, so repeating yourself is the normal case rather than an
// edge one.
func TestMazeEveryDirectionSpelling(t *testing.T) {
	cases := []struct {
		cmd  string
		want maze.Dir
	}{
		{"!up", maze.North}, {"!go up", maze.North}, {"!gou", maze.North},
		{"!down", maze.South}, {"!go down", maze.South}, {"!god", maze.South},
		{"!left", maze.West}, {"!go left", maze.West}, {"!gol", maze.West},
		{"!right", maze.East}, {"!go right", maze.East}, {"!gor", maze.East},
	}
	for _, c := range cases {
		t.Run(c.cmd, func(t *testing.T) {
			r, _, chat, _, mr := startTestMaze(t)
			seat(t, r, mr, "bob")
			chat.replies = nil

			r.Handle(emsg("bob", c.cmd, false))

			p, _ := mr.round.PlayerBy("id-bob")
			got, ok := p.NextDir()
			if !ok {
				t.Fatalf("%q locked in nothing; replies=%v", c.cmd, chat.replies)
			}
			if got != c.want {
				t.Errorf("%q = %v want %v", c.cmd, got, c.want)
			}
			if len(chat.replies) != 0 {
				t.Errorf("%q spoke: %v", c.cmd, chat.replies)
			}
		})
	}
}

// TestMazeAlternatingSpellingsAllLand is the point of the aliases: three messages
// in a row that Twitch would see as different, all moving the same way.
func TestMazeAlternatingSpellingsAllLand(t *testing.T) {
	r, _, _, _, mr := startTestMaze(t)
	seat(t, r, mr, "bob")
	p, _ := mr.round.PlayerBy("id-bob")

	start := p.At
	for i, cmd := range []string{"!down", "!go down", "!god"} {
		r.Handle(emsg("bob", cmd, false))
		d, ok := p.NextDir()
		if !ok || d != maze.South {
			t.Fatalf("%q (%d) locked in %v/%v, want down", cmd, i, d, ok)
		}
		cycle(r, mr)
	}
	if p.At == start {
		t.Error("three moves and the player never moved")
	}
}

func TestMazeGoRejectsNonsense(t *testing.T) {
	r, _, chat, _, mr := startTestMaze(t)
	seat(t, r, mr, "bob")
	chat.replies = nil

	for _, bad := range []string{"!go", "!go sideways", "!go w", "!go north"} {
		r.Handle(emsg("bob", bad, false))
	}
	if len(chat.replies) != 4 {
		t.Fatalf("%d replies for 4 bad arguments, want 4: %v", len(chat.replies), chat.replies)
	}
	if !strings.Contains(chat.replies[0].text, "!gou") {
		t.Errorf("usage reply %q should name the short forms too", chat.replies[0].text)
	}
	if p, _ := mr.round.PlayerBy("id-bob"); p.Queued() != 0 {
		t.Error("an unparseable direction still locked a move in")
	}
}

func TestMazeAliasesAreReserved(t *testing.T) {
	r, _, _, _ := econRouter(t)
	for _, cmd := range []string{"!go", "!gou", "!god", "!gol", "!gor"} {
		if !r.isBuiltin(cmd) {
			t.Errorf("%s is not reserved; !addcom could shadow it", cmd)
		}
	}
}

// TestMazeAppearsInTheCommandList: a game nobody can find is a game nobody plays.
// The maze shipped without an entry here while Wordle and Connections had one, so
// the only way to discover it was to already know.
func TestMazeAppearsInTheCommandList(t *testing.T) {
	r, _, _, chat := econRouter(t)
	r.Handle(emsg("bob", "!commands", false))
	if len(chat.replies) != 1 {
		t.Fatalf("replies=%v", chat.replies)
	}
	got := chat.replies[0].text
	for _, want := range []string{"!maze", "!up", "!down", "!left", "!right", "!mazewins"} {
		if !strings.Contains(got, want) {
			t.Errorf("!commands does not mention %s: %q", want, got)
		}
	}
	// The alias spellings exist to dodge Twitch's duplicate rule, not to be browsed;
	// listing all twelve would bury the four that matter.
	for _, noise := range []string{"!gou", "!god", "!gol", "!gor"} {
		if strings.Contains(got, noise) {
			t.Errorf("!commands lists the alias %s; the list should stay one entry per way in", noise)
		}
	}
}
