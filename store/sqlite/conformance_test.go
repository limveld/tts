package sqlite_test

import (
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
