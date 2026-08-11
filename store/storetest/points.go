package storetest

import "testing"

func testCreditSpendBalance(t *testing.T, s Store) {
	if b, err := s.Balance("u1"); err != nil || b != 0 {
		t.Fatalf("empty balance: %d err=%v", b, err)
	}

	if credited, err := s.Credit("u1", 100, "accrual", ""); err != nil || !credited {
		t.Fatalf("Credit: credited=%v err=%v", credited, err)
	}
	if b, _ := s.Balance("u1"); b != 100 {
		t.Fatalf("balance after credit=%d want 100", b)
	}

	if ok, err := s.Spend("u1", 40, "tts"); err != nil || !ok {
		t.Fatalf("Spend 40: ok=%v err=%v", ok, err)
	}
	if b, _ := s.Balance("u1"); b != 60 {
		t.Fatalf("balance after spend=%d want 60", b)
	}

	// A non-positive spend is a no-op that succeeds, so callers don't have to
	// special-case a zero cost.
	if ok, err := s.Spend("u1", 0, "tts"); err != nil || !ok {
		t.Errorf("Spend 0: ok=%v err=%v want true/nil", ok, err)
	}
	if b, _ := s.Balance("u1"); b != 60 {
		t.Errorf("balance after zero spend=%d want 60", b)
	}
}

func testSpendInsufficient(t *testing.T, s Store) {
	s.Credit("u1", 60, "accrual", "")

	// Overspend is refused with no error, and changes nothing.
	if ok, err := s.Spend("u1", 100, "tts"); err != nil || ok {
		t.Fatalf("overspend: ok=%v err=%v want false/nil", ok, err)
	}
	if b, _ := s.Balance("u1"); b != 60 {
		t.Fatalf("balance after overspend=%d want 60 (unchanged)", b)
	}
	// Spending the exact balance is allowed.
	if ok, err := s.Spend("u1", 60, "tts"); err != nil || !ok {
		t.Fatalf("spend everything: ok=%v err=%v", ok, err)
	}
	if b, _ := s.Balance("u1"); b != 0 {
		t.Fatalf("balance=%d want 0", b)
	}
	// A user who has never been seen can't spend.
	if ok, _ := s.Spend("ghost", 1, "tts"); ok {
		t.Error("unknown user was allowed to spend")
	}
}

func testCreditIdempotentRef(t *testing.T, s Store) {
	if credited, err := s.Credit("u1", 500, "convert", "redemption-abc"); err != nil || !credited {
		t.Fatalf("first credit: credited=%v err=%v", credited, err)
	}
	// Same ref again: ignored.
	if credited, err := s.Credit("u1", 500, "convert", "redemption-abc"); err != nil || credited {
		t.Fatalf("duplicate ref: credited=%v err=%v want false/nil", credited, err)
	}
	if b, _ := s.Balance("u1"); b != 500 {
		t.Fatalf("balance=%d want 500 (credited once)", b)
	}
	// A different ref credits again.
	if credited, _ := s.Credit("u1", 500, "convert", "redemption-def"); !credited {
		t.Fatal("second distinct ref not credited")
	}
	if b, _ := s.Balance("u1"); b != 1000 {
		t.Fatalf("balance=%d want 1000", b)
	}
	// Empty refs are NULL, not "", so any number of them coexist — otherwise
	// the second accrual of the stream would be silently dropped.
	for i := 0; i < 3; i++ {
		if credited, err := s.Credit("u1", 1, "accrual", ""); err != nil || !credited {
			t.Fatalf("unreffed credit %d: credited=%v err=%v", i, credited, err)
		}
	}
	if b, _ := s.Balance("u1"); b != 1003 {
		t.Fatalf("balance=%d want 1003 (three unreffed credits all landed)", b)
	}
}

func testUsersAndLeaderboard(t *testing.T, s Store) {
	if err := s.UpsertUser("u1", "bob", "Bob"); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertUser("u2", "amy", "Amy"); err != nil {
		t.Fatal(err)
	}
	// Rename bob (same user_id): latest name wins.
	if err := s.UpsertUser("u1", "bobby", "Bobby"); err != nil {
		t.Fatal(err)
	}

	if id, ok, err := s.ResolveLogin("bobby"); err != nil || !ok || id != "u1" {
		t.Fatalf("ResolveLogin bobby: id=%q ok=%v err=%v", id, ok, err)
	}
	if _, ok, _ := s.ResolveLogin("ghost"); ok {
		t.Error("ResolveLogin ghost: want ok=false")
	}

	s.Credit("u1", 300, "accrual", "")
	s.Credit("u2", 900, "accrual", "")

	lb, err := s.Leaderboard(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(lb) != 2 || lb[0].UserID != "u2" || lb[0].Balance != 900 || lb[0].Display != "Amy" {
		t.Fatalf("leaderboard=%+v want amy(900) first", lb)
	}
	if lb[1].UserID != "u1" || lb[1].Balance != 300 || lb[1].Display != "Bobby" {
		t.Fatalf("leaderboard[1]=%+v want bobby(300)", lb[1])
	}

	// A user with marks but no identity row is omitted — there'd be no name to
	// show. This is also what pins the JOIN rather than a LEFT JOIN.
	s.Credit("anon", 5000, "accrual", "")
	lb, _ = s.Leaderboard(10)
	if len(lb) != 2 {
		t.Fatalf("leaderboard=%+v want the nameless user omitted", lb)
	}
}

// Only users in credit appear. The exclusion lives in the query's HAVING, which
// had to be rewritten to be legal Postgres, so this is the case that catches a
// regression there in either dialect.
func testLeaderboardExcludesZeroAndNegative(t *testing.T, s Store) {
	for _, u := range []struct{ id, login, display string }{
		{"pos", "pos", "Positive"},
		{"zero", "zero", "Zeroed"},
		{"neg", "neg", "Negative"},
	} {
		if err := s.UpsertUser(u.id, u.login, u.display); err != nil {
			t.Fatal(err)
		}
	}
	s.Credit("pos", 1, "accrual", "")
	// Nets to exactly zero: has ledger rows, but no marks.
	s.Credit("zero", 50, "accrual", "")
	s.Spend("zero", 50, "tts")
	// Nets negative. Spend and Grant both refuse to go below zero, so this is
	// only reachable by writing a negative delta straight to the ledger — but
	// the query must not surface it if it ever happens.
	s.Credit("neg", 10, "accrual", "")
	s.Credit("neg", -25, "correction", "")

	lb, err := s.Leaderboard(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(lb) != 1 || lb[0].UserID != "pos" || lb[0].Balance != 1 {
		t.Fatalf("leaderboard=%+v want only pos(1)", lb)
	}
}

// The leaderboard and Balance are two different queries over the same number.
// They were one aggregate over the ledger and are now two reads of
// accounts.balance, so a join that quietly changed the figure — picking up a
// duplicate users row, say — would show up here and nowhere else.
func testLeaderboardMatchesBalances(t *testing.T, s Store) {
	for i, u := range []struct {
		id, login, display string
		credit, spend      int64
	}{
		{"a", "amy", "Amy", 900, 100},
		{"b", "bob", "Bob", 500, 0},
		{"c", "cal", "Cal", 250, 250}, // nets zero, must not appear
		{"d", "dee", "Dee", 750, 300},
	} {
		if err := s.UpsertUser(u.id, u.login, u.display); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Credit(u.id, u.credit, "accrual", ""); err != nil {
			t.Fatal(err)
		}
		if u.spend > 0 {
			if _, err := s.Spend(u.id, u.spend, "tts"); err != nil {
				t.Fatal(err)
			}
		}
		if i == 0 {
			if _, err := s.Transfer("a", "b", 50, "give"); err != nil {
				t.Fatal(err)
			}
		}
	}

	lb, err := s.Leaderboard(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(lb) == 0 {
		t.Fatal("leaderboard is empty — the seed wrote nothing, so this proved nothing")
	}
	for _, e := range lb {
		bal, err := s.Balance(e.UserID)
		if err != nil {
			t.Fatal(err)
		}
		if bal != e.Balance {
			t.Errorf("%s: leaderboard says %d, Balance says %d", e.UserID, e.Balance, bal)
		}
		if e.Balance <= 0 {
			t.Errorf("%s: leaderboard included a non-positive balance %d", e.UserID, e.Balance)
		}
	}
	for _, e := range lb {
		if e.UserID == "c" {
			t.Error("leaderboard included c, whose balance nets to zero")
		}
	}
}

// Ordering is balance descending, ties broken by display ascending — and the
// tie-break is where the two backends diverge without COLLATE "C". "amy" sorts
// before "Bea" under en_US.UTF-8 (case-insensitive) but after it in byte order.
func testLeaderboardOrderAndTieBreak(t *testing.T, s Store) {
	for _, u := range []struct {
		id, login, display string
		amount             int64
	}{
		{"lead", "lead", "Zoe", 90},
		{"tieB", "tieb", "amy", 50},
		{"tieA", "tiea", "Bea", 50},
	} {
		if err := s.UpsertUser(u.id, u.login, u.display); err != nil {
			t.Fatal(err)
		}
		s.Credit(u.id, u.amount, "accrual", "")
	}

	lb, err := s.Leaderboard(10)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range lb {
		got = append(got, e.Display)
	}
	want := []string{"Zoe", "Bea", "amy"}
	if len(got) != len(want) {
		t.Fatalf("leaderboard=%v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("leaderboard=%v want %v (byte order: 'B' < 'a')", got, want)
		}
	}

	// The limit is honored.
	if lb, _ := s.Leaderboard(1); len(lb) != 1 || lb[0].Display != "Zoe" {
		t.Errorf("Leaderboard(1)=%+v want just Zoe", lb)
	}
}

func testGrantMintAndClamp(t *testing.T, s Store) {
	// Mint.
	if bal, err := s.Grant("u1", 500, "grant"); err != nil || bal != 500 {
		t.Fatalf("Grant +500: bal=%d err=%v", bal, err)
	}
	// Partial removal.
	if bal, err := s.Grant("u1", -200, "grant"); err != nil || bal != 300 {
		t.Fatalf("Grant -200: bal=%d err=%v want 300", bal, err)
	}
	// Over-removal clamps to 0 (not negative).
	if bal, err := s.Grant("u1", -100000, "grant"); err != nil || bal != 0 {
		t.Fatalf("Grant -100000: bal=%d err=%v want 0", bal, err)
	}
	if b, _ := s.Balance("u1"); b != 0 {
		t.Fatalf("balance=%d want 0 (never negative)", b)
	}
	// A zero grant writes no ledger row but still reports the balance.
	if bal, err := s.Grant("u1", 0, "grant"); err != nil || bal != 0 {
		t.Errorf("Grant 0: bal=%d err=%v", bal, err)
	}
}

func testTransfer(t *testing.T, s Store) {
	s.Credit("giver", 100, "accrual", "")

	if ok, err := s.Transfer("giver", "taker", 30, "give"); err != nil || !ok {
		t.Fatalf("Transfer: ok=%v err=%v", ok, err)
	}
	if b, _ := s.Balance("giver"); b != 70 {
		t.Fatalf("giver=%d want 70", b)
	}
	if b, _ := s.Balance("taker"); b != 30 {
		t.Fatalf("taker=%d want 30", b)
	}
}

func testTransferInsufficient(t *testing.T, s Store) {
	s.Credit("giver", 70, "accrual", "")

	if ok, err := s.Transfer("giver", "taker", 1000, "give"); err != nil || ok {
		t.Fatalf("over-transfer: ok=%v err=%v want false/nil", ok, err)
	}
	if b, _ := s.Balance("giver"); b != 70 {
		t.Fatalf("giver after failed transfer=%d want 70", b)
	}
	if b, _ := s.Balance("taker"); b != 0 {
		t.Fatalf("taker after failed transfer=%d want 0 (no half-transfer)", b)
	}
}

// A self-transfer conserves the balance. Worth pinning because the Postgres path
// locks both sides and must not try to lock the same row twice.
func testTransferSelfIsNoop(t *testing.T, s Store) {
	s.Credit("u1", 100, "accrual", "")

	if ok, err := s.Transfer("u1", "u1", 30, "give"); err != nil || !ok {
		t.Fatalf("self transfer: ok=%v err=%v", ok, err)
	}
	if b, _ := s.Balance("u1"); b != 100 {
		t.Fatalf("balance after self transfer=%d want 100 (unchanged)", b)
	}
}
