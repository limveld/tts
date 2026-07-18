package store

import (
	"path/filepath"
	"testing"
)

func TestWordleWinsTallyAndLeaderboard(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// alice solves twice, bob once.
	if n, err := st.WordleAddWin("id-alice", "alice", "Alice"); err != nil || n != 1 {
		t.Fatalf("first win = %d, %v; want 1", n, err)
	}
	if n, _ := st.WordleAddWin("id-alice", "alice", "Alice"); n != 2 {
		t.Fatalf("second win = %d, want 2", n)
	}
	if n, _ := st.WordleAddWin("id-bob", "bob", "Bob"); n != 1 {
		t.Fatalf("bob win = %d, want 1", n)
	}

	lb, err := st.WordleLeaderboard(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(lb) != 2 {
		t.Fatalf("leaderboard len=%d want 2", len(lb))
	}
	if lb[0].Display != "Alice" || lb[0].Wins != 2 {
		t.Errorf("top=%+v want Alice/2", lb[0])
	}
	if lb[1].Display != "Bob" || lb[1].Wins != 1 {
		t.Errorf("second=%+v want Bob/1", lb[1])
	}
}
