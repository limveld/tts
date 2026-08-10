package main

import (
	"path/filepath"
	"testing"

	"tts/store/sqlite"
)

// newTestStore opens a throwaway store for a bot test. It is always SQLite, and
// deliberately so: these tests are about chat behavior, not about persistence.
// That the two backends behave identically is proven once, in store/storetest —
// running the bot's suite twice would double the time to red and buy nothing.
func newTestStore(t *testing.T) Store {
	t.Helper()
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "bot-test.db"))
	if err != nil {
		t.Fatalf("open test store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}
