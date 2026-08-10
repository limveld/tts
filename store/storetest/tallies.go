package storetest

import "testing"

func testWordleTally(t *testing.T, s Store) {
	if wins, err := s.WordleAddWin("u1", "bob", "Bob"); err != nil || wins != 1 {
		t.Fatalf("first win: wins=%d err=%v", wins, err)
	}
	if wins, err := s.WordleAddWin("u1", "bob", "Bob"); err != nil || wins != 2 {
		t.Fatalf("second win: wins=%d err=%v want 2", wins, err)
	}
	// A rename refreshes the stored name without resetting the tally.
	if wins, err := s.WordleAddWin("u1", "bobby", "Bobby"); err != nil || wins != 3 {
		t.Fatalf("after rename: wins=%d err=%v want 3", wins, err)
	}
	// An empty display falls back to the login, so the leaderboard is never blank.
	if _, err := s.WordleAddWin("u2", "amy", ""); err != nil {
		t.Fatal(err)
	}

	lb, err := s.WordleLeaderboard(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(lb) != 2 {
		t.Fatalf("leaderboard=%+v want 2 rows", lb)
	}
	if lb[0].Display != "Bobby" || lb[0].Wins != 3 {
		t.Errorf("leaderboard[0]=%+v want Bobby with 3", lb[0])
	}
	if lb[1].Display != "amy" || lb[1].Wins != 1 {
		t.Errorf("leaderboard[1]=%+v want amy (display fell back to login) with 1", lb[1])
	}
	if lb, _ := s.WordleLeaderboard(1); len(lb) != 1 {
		t.Errorf("WordleLeaderboard(1) returned %d rows", len(lb))
	}
}

func testConnectionsTally(t *testing.T, s Store) {
	if wins, err := s.ConnectionsAddWin("u1", "bob", "Bob"); err != nil || wins != 1 {
		t.Fatalf("first win: wins=%d err=%v", wins, err)
	}
	if wins, err := s.ConnectionsAddWin("u1", "bobby", "Bobby"); err != nil || wins != 2 {
		t.Fatalf("second win: wins=%d err=%v want 2", wins, err)
	}
	if _, err := s.ConnectionsAddWin("u2", "amy", ""); err != nil {
		t.Fatal(err)
	}

	lb, err := s.ConnectionsLeaderboard(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(lb) != 2 || lb[0].Display != "Bobby" || lb[0].Wins != 2 {
		t.Fatalf("leaderboard=%+v want Bobby(2) first", lb)
	}
	if lb[1].Display != "amy" || lb[1].Wins != 1 {
		t.Errorf("leaderboard[1]=%+v want amy with 1", lb[1])
	}

	// The two tallies are separate tables and must not bleed into each other.
	if wl, _ := s.WordleLeaderboard(10); len(wl) != 0 {
		t.Errorf("wordle leaderboard=%+v want empty (connections wins leaked)", wl)
	}
}
