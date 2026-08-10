package postgres_test

import (
	"testing"

	"tts/store/postgres"
	"tts/store/storetest"
)

// newStore builds a store in its own throwaway schema. TempSchemaDSN registers
// its DROP first and the store's Close second, and cleanups run last-in-first-
// out — so the pool closes before the schema is dropped, which matters because
// DROP SCHEMA blocks on any connection still using it.
func newStore(base string) storetest.New {
	return func(t *testing.T) storetest.Store {
		dsn := storetest.TempSchemaDSN(t, base)
		s, err := postgres.Open(dsn)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		t.Cleanup(func() { s.Close() })
		return s
	}
}

func TestConformance(t *testing.T) {
	base := storetest.PostgresDSN(t)
	storetest.Run(t, newStore(base))
}

// The concurrency suite runs here and nowhere else: these cases are about two
// writers contending, which only Postgres has.
func TestConformanceConcurrent(t *testing.T) {
	base := storetest.PostgresDSN(t)
	storetest.RunConcurrent(t, newStore(base))
}
