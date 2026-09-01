package storetest

import (
	"testing"

	"tts/store"
)

func aMazeRound(id string, endedAt int64) store.MazeRound {
	return store.MazeRound{
		ID: id, RoomID: "room1", Seed: 4242,
		StartedAt: endedAt - 200, EndedAt: endedAt, TickMS: 10000,
		Cycles: 18, Reason: "placements-closed",
		Players: 5, Finishers: 4,
		WinnerID: "u1", WinnerLogin: "bob", WinnerDisplay: "Bob",
		Input: []byte(`{"board":{"size":6},"moves":[{"cycle":1,"seat":0,"dir":"up"}]}`),
	}
}

func mazeEvents(id string) []store.MazeEvent {
	return []store.MazeEvent{
		{RoundID: id, Seq: 0, Cycle: 0, Kind: "seats-locked", Seat: -1, N: 4},
		{RoundID: id, Seq: 1, Cycle: 3, Kind: "key-taken", Seat: 0,
			UserID: "u1", Login: "bob", Display: "Bob", At: "C4", N: 3},
		{RoundID: id, Seq: 2, Cycle: 9, Kind: "round-ended", Seat: -1, Reason: "placements-closed"},
	}
}

func testMazeLogRoundTrip(t *testing.T, s Store) {
	want := aMazeRound("r1", 1_700_000_000)
	if err := s.MazeLogRound(want, mazeEvents("r1")); err != nil {
		t.Fatal(err)
	}

	got, ok, err := s.MazeRoundByID("r1")
	if err != nil || !ok {
		t.Fatalf("MazeRoundByID: ok=%v err=%v", ok, err)
	}
	if got.RoomID != want.RoomID || got.Seed != want.Seed || got.Cycles != want.Cycles ||
		got.Reason != want.Reason || got.Players != want.Players || got.Finishers != want.Finishers ||
		got.WinnerDisplay != want.WinnerDisplay || got.TickMS != want.TickMS ||
		got.StartedAt != want.StartedAt || got.EndedAt != want.EndedAt {
		t.Errorf("round round-tripped as %+v, want %+v", got, want)
	}
	// Compared as JSON: Postgres stores this as JSONB and normalizes key order, so
	// a byte compare is green on SQLite and red on Postgres for no real reason.
	if !sameJSON(t, got.Input, want.Input) {
		t.Errorf("input = %s want %s", got.Input, want.Input)
	}

	evs, err := s.MazeRoundEvents("r1")
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 3 {
		t.Fatalf("%d events, want 3", len(evs))
	}
	if evs[1].Kind != "key-taken" || evs[1].At != "C4" || evs[1].N != 3 || evs[1].Display != "Bob" {
		t.Errorf("event[1] = %+v", evs[1])
	}
	// Round-level events carry no position, and must not have acquired one.
	if evs[0].At != "" || evs[2].At != "" {
		t.Errorf("a round-level event has a board position: %+v / %+v", evs[0], evs[2])
	}
	if evs[0].Seat != -1 {
		t.Errorf("seats-locked seat = %d, want -1", evs[0].Seat)
	}
	if evs[2].Reason != "placements-closed" {
		t.Errorf("round-ended reason = %q", evs[2].Reason)
	}
}

// testMazeLogIsIdempotent: several paths end a round — a finishing tick, a
// moderator's !skipgame, a bot restarting onto a round that had already ended —
// and writing twice must not double the archive.
func testMazeLogIsIdempotent(t *testing.T, s Store) {
	r := aMazeRound("r1", 1_700_000_000)
	for i := 0; i < 2; i++ {
		if err := s.MazeLogRound(r, mazeEvents("r1")); err != nil {
			t.Fatalf("write %d: %v", i+1, err)
		}
	}
	if rounds, _ := s.MazeRoundLog(10); len(rounds) != 1 {
		t.Errorf("%d rounds after writing twice, want 1", len(rounds))
	}
	if evs, _ := s.MazeRoundEvents("r1"); len(evs) != 3 {
		t.Errorf("%d events after writing twice, want 3", len(evs))
	}
}

// testMazeLogOrderIsEmissionOrder inserts out of natural order on purpose. Without
// an explicit ORDER BY this passes on SQLite, which hands back rowid order, and
// fails on Postgres — the exact shape of bug the conformance suite exists for.
func testMazeLogOrderIsEmissionOrder(t *testing.T, s Store) {
	evs := mazeEvents("r1")
	shuffled := []store.MazeEvent{evs[2], evs[0], evs[1]}
	if err := s.MazeLogRound(aMazeRound("r1", 1_700_000_000), shuffled); err != nil {
		t.Fatal(err)
	}
	got, err := s.MazeRoundEvents("r1")
	if err != nil {
		t.Fatal(err)
	}
	for i, e := range got {
		if e.Seq != i {
			t.Fatalf("events came back as %d,%d,%d — not in seq order",
				got[0].Seq, got[1].Seq, got[2].Seq)
		}
		_ = e
	}
}

// testMazeRoundLogNewestFirst also pins the tie-break, which is a text comparison
// on the id — and Postgres's default collation orders text differently from
// SQLite's byte comparison unless it is told not to.
func testMazeRoundLogNewestFirst(t *testing.T, s Store) {
	for _, r := range []store.MazeRound{
		aMazeRound("aaa", 100),
		aMazeRound("bbb", 300),
		aMazeRound("ccc", 300), // same instant as bbb: the id breaks the tie
	} {
		if err := s.MazeLogRound(r, nil); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.MazeRoundLog(10)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"ccc", "bbb", "aaa"}
	if len(got) != len(want) {
		t.Fatalf("%d rounds, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].ID != want[i] {
			t.Fatalf("order = %s,%s,%s want %v", got[0].ID, got[1].ID, got[2].ID, want)
		}
	}
	if one, _ := s.MazeRoundLog(1); len(one) != 1 || one[0].ID != "ccc" {
		t.Errorf("MazeRoundLog(1) = %+v", one)
	}
}

// testMazeLogEmptyEvents: an abandoned round nobody joined still belongs in the
// archive, and writes fine with nothing to say.
func testMazeLogEmptyEvents(t *testing.T, s Store) {
	r := aMazeRound("r1", 1_700_000_000)
	r.Reason, r.Players, r.Finishers, r.WinnerID, r.WinnerLogin, r.WinnerDisplay = "abandoned", 0, 0, "", "", ""
	if err := s.MazeLogRound(r, nil); err != nil {
		t.Fatal(err)
	}
	if evs, err := s.MazeRoundEvents("r1"); err != nil || len(evs) != 0 {
		t.Errorf("events = %v err = %v, want none", evs, err)
	}
}

func testMazeRoundMissing(t *testing.T, s Store) {
	if _, ok, err := s.MazeRoundByID("nope"); ok || err != nil {
		t.Errorf("MazeRoundByID(nope) = ok:%v err:%v, want false and no error", ok, err)
	}
	if evs, err := s.MazeRoundEvents("nope"); err != nil || len(evs) != 0 {
		t.Errorf("MazeRoundEvents(nope) = %v err:%v", evs, err)
	}
}

// testMazeLogDoesNotTouchTallies is testTalliesAreIndependent's argument moved to
// a new file: mazelog.go was written by mirroring maze.go, and the way that goes
// wrong is a table name left behind in a copied query. Every test above would
// still pass if the log wrote into maze_wins.
func testMazeLogDoesNotTouchTallies(t *testing.T, s Store) {
	if err := s.MazeLogRound(aMazeRound("r1", 1), mazeEvents("r1")); err != nil {
		t.Fatal(err)
	}
	if lb, _ := s.MazeLeaderboard(10); len(lb) != 0 {
		t.Errorf("maze leaderboard = %+v after logging a round, want empty", lb)
	}
	if _, err := s.MazeAddWin("u1", "bob", "Bob"); err != nil {
		t.Fatal(err)
	}
	if rounds, _ := s.MazeRoundLog(10); len(rounds) != 1 {
		t.Errorf("%d logged rounds after a win was tallied, want 1", len(rounds))
	}
}
