package sqlite_test

import (
	"database/sql"
	"path/filepath"
	"testing"

	"tts/store/sqlite"
	"tts/store/storetest"
)

// SQLite runs the conformance suite unconditionally — no daemon, no DSN, no
// skip. It is the backend that always gets tested, which is most of why it stays
// supported.
//
// RunConcurrent is deliberately absent: SQLite admits one writer at a time, so
// those cases would pass here without exercising the locking they exist to
// check.
func TestConformance(t *testing.T) {
	storetest.Run(t, func(t *testing.T) storetest.Store {
		s, err := sqlite.Open(filepath.Join(t.TempDir(), "conformance.db"))
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		t.Cleanup(func() { s.Close() })
		return s
	})
}

// The invariant suite does run here: accounts.balance is behavioral, so it has to
// hold on both backends. On SQLite the ledger_opening term is always zero, which
// makes the invariant the simpler balance == SUM(ledger).
func TestConformanceInvariants(t *testing.T) {
	storetest.RunInvariants(t, func(t *testing.T) (storetest.Store, *sql.DB) {
		s, err := sqlite.Open(filepath.Join(t.TempDir(), "invariants.db"))
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		t.Cleanup(func() { s.Close() })
		return s, s.DB()
	})
}
