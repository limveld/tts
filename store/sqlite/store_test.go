package sqlite

import (
	"path/filepath"
	"sync"
	"testing"
)

func openTemp(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

// Cheap insurance that _txlock=immediate didn't regress the write paths: with a
// deferred transaction these goroutines would hit SQLITE_BUSY on lock upgrade,
// which busy_timeout does not rescue. Because SQLite admits one writer at a
// time, the outcome is exact rather than merely safe — 100 marks at 10 each
// means ten winners, ten refusals, and a final balance of zero.
func TestConcurrentSpendNeverOverdraws(t *testing.T) {
	s := openTemp(t)
	if _, err := s.Credit("u1", 100, "accrual", ""); err != nil {
		t.Fatal(err)
	}

	const goroutines = 20
	var wg sync.WaitGroup
	errs := make(chan error, goroutines)
	oks := make(chan bool, goroutines)
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := s.Spend("u1", 10, "tts")
			if err != nil {
				errs <- err
				return
			}
			oks <- ok
		}()
	}
	wg.Wait()
	close(errs)
	close(oks)

	for err := range errs {
		t.Fatalf("concurrent Spend: %v", err)
	}
	spent := 0
	for ok := range oks {
		if ok {
			spent++
		}
	}
	if spent != 10 {
		t.Errorf("successful spends=%d want 10 (100 marks / 10 each)", spent)
	}
	if b, err := s.Balance("u1"); err != nil || b != 0 {
		t.Errorf("final balance=%d err=%v want 0 and never negative", b, err)
	}
}
