package store

import "testing"

func TestCreditSpendBalance(t *testing.T) {
	s := openTemp(t)

	if b, err := s.Balance("u1"); err != nil || b != 0 {
		t.Fatalf("empty balance: %d err=%v", b, err)
	}

	if credited, err := s.Credit("u1", 100, "accrual", ""); err != nil || !credited {
		t.Fatalf("Credit: credited=%v err=%v", credited, err)
	}
	if b, _ := s.Balance("u1"); b != 100 {
		t.Fatalf("balance after credit=%d want 100", b)
	}

	// Affordable spend.
	if ok, err := s.Spend("u1", 40, "tts"); err != nil || !ok {
		t.Fatalf("Spend 40: ok=%v err=%v", ok, err)
	}
	if b, _ := s.Balance("u1"); b != 60 {
		t.Fatalf("balance after spend=%d want 60", b)
	}

	// Overspend rejected, balance unchanged.
	if ok, err := s.Spend("u1", 100, "tts"); err != nil || ok {
		t.Fatalf("overspend: ok=%v err=%v want false/nil", ok, err)
	}
	if b, _ := s.Balance("u1"); b != 60 {
		t.Fatalf("balance after overspend=%d want 60 (unchanged)", b)
	}
}

func TestCreditIdempotentRef(t *testing.T) {
	s := openTemp(t)

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
}

func TestUsersAndLeaderboard(t *testing.T) {
	s := openTemp(t)

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
}

func TestTransfer(t *testing.T) {
	s := openTemp(t)
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

	// Can't transfer more than the balance.
	if ok, err := s.Transfer("giver", "taker", 1000, "give"); err != nil || ok {
		t.Fatalf("over-transfer: ok=%v err=%v want false/nil", ok, err)
	}
	if b, _ := s.Balance("giver"); b != 70 {
		t.Fatalf("giver after failed transfer=%d want 70", b)
	}
}
