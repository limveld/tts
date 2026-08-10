package storetest

import (
	"sync"
	"testing"
	"time"
)

// These are the cases the accounts row lock exists for. Each one fails in a
// specific, recognizable way if its guard is removed:
//
//	overdraw       -> more spends succeed than there were marks, balance negative
//	deadlock       -> "deadlock detected" from Postgres, or a hang
//	credit race    -> the same ref credits twice
//
// They are Postgres-only. SQLite admits one writer at a time, so running them
// there would pass without exercising anything and quietly imply the locking is
// present when it isn't.

// 100 marks, 20 goroutines each spending 10: exactly 10 must win. Without the
// row lock, several goroutines read the same balance, all pass the check, and
// the balance ends up negative.
func testConcurrentSpendCannotOverdraw(t *testing.T, s Store) {
	if _, err := s.Credit("u1", 100, "accrual", ""); err != nil {
		t.Fatal(err)
	}

	const goroutines = 20
	var wg sync.WaitGroup
	results := make(chan bool, goroutines)
	errs := make(chan error, goroutines)
	start := make(chan struct{})
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start // release them together, to actually contend
			ok, err := s.Spend("u1", 10, "tts")
			if err != nil {
				errs <- err
				return
			}
			results <- ok
		}()
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)

	for err := range errs {
		t.Fatalf("concurrent Spend: %v", err)
	}
	won := 0
	for ok := range results {
		if ok {
			won++
		}
	}
	if won != 10 {
		t.Errorf("successful spends=%d want exactly 10 (100 marks at 10 each)", won)
	}
	bal, err := s.Balance("u1")
	if err != nil {
		t.Fatal(err)
	}
	if bal != 0 {
		t.Errorf("final balance=%d want 0", bal)
	}
	if bal < 0 {
		t.Errorf("balance went NEGATIVE (%d) — the check-then-debit lost an update", bal)
	}
}

// A→B and B→A at the same time. Without sorted lock acquisition each direction
// holds the row the other wants and Postgres kills one with "deadlock detected".
// Marks are conserved either way, so the assertion that matters is the absence
// of an error.
func testBidirectionalTransfersDoNotDeadlock(t *testing.T, s Store) {
	if _, err := s.Credit("alice", 1000, "accrual", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Credit("bob", 1000, "accrual", ""); err != nil {
		t.Fatal(err)
	}

	const pairs = 25
	var wg sync.WaitGroup
	errs := make(chan error, pairs*2)
	start := make(chan struct{})
	for i := 0; i < pairs; i++ {
		for _, dir := range [][2]string{{"alice", "bob"}, {"bob", "alice"}} {
			wg.Add(1)
			go func(from, to string) {
				defer wg.Done()
				<-start
				if _, err := s.Transfer(from, to, 1, "give"); err != nil {
					errs <- err
				}
			}(dir[0], dir[1])
		}
	}
	close(start)

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("bidirectional transfers hung — lock ordering")
	}
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent Transfer: %v (sorted lock acquisition?)", err)
	}

	a, err := s.Balance("alice")
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.Balance("bob")
	if err != nil {
		t.Fatal(err)
	}
	if a+b != 2000 {
		t.Errorf("total=%d want 2000 conserved (alice=%d bob=%d)", a+b, a, b)
	}
}

// Twitch re-serves a redemption until the fulfill propagates, and the conversion
// loop can be mid-poll on two tickers. The same ref must credit exactly once.
func testConcurrentCreditWithSameRefCreditsOnce(t *testing.T, s Store) {
	const goroutines = 16
	var wg sync.WaitGroup
	credited := make(chan bool, goroutines)
	errs := make(chan error, goroutines)
	start := make(chan struct{})
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			ok, err := s.Credit("u1", 500, "convert", "redemption-abc")
			if err != nil {
				errs <- err
				return
			}
			credited <- ok
		}()
	}
	close(start)
	wg.Wait()
	close(credited)
	close(errs)

	for err := range errs {
		t.Fatalf("concurrent Credit: %v", err)
	}
	n := 0
	for ok := range credited {
		if ok {
			n++
		}
	}
	if n != 1 {
		t.Errorf("credited %d times want exactly 1", n)
	}
	if bal, _ := s.Balance("u1"); bal != 500 {
		t.Errorf("balance=%d want 500 (credited once)", bal)
	}
}

// Grant and Spend contending on one user. Grant's claw-back clamps at the
// balance it read, so without the lock a concurrent Spend between the read and
// the write drives the balance below zero.
func testConcurrentGrantAndSpendNeverGoesNegative(t *testing.T, s Store) {
	if _, err := s.Credit("u1", 500, "accrual", ""); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 40)
	start := make(chan struct{})
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, err := s.Spend("u1", 25, "tts"); err != nil {
				errs <- err
			}
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, err := s.Grant("u1", -25, "grant"); err != nil {
				errs <- err
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Fatalf("concurrent Grant/Spend: %v", err)
	}
	bal, err := s.Balance("u1")
	if err != nil {
		t.Fatal(err)
	}
	if bal < 0 {
		t.Errorf("balance=%d — went negative under contention", bal)
	}
}
