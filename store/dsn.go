package store

import "strings"

// Backend names the implementation a DSN selects.
type Backend int

const (
	// SQLiteBackend is a bare filesystem path, or a sqlite:// URL.
	SQLiteBackend Backend = iota
	// PostgresBackend is a postgres:// or postgresql:// URL.
	PostgresBackend
)

func (b Backend) String() string {
	if b == PostgresBackend {
		return "postgres"
	}
	return "sqlite"
}

// Classify reports which backend dsn selects and the target to hand that
// backend — for SQLite, the file path with any sqlite:// prefix stripped; for
// Postgres, the URL unchanged.
//
// This lives in the type-only parent package on purpose. Both the bot's
// openStore and cmd/store-migrate need to know what a DSN means, and a shared
// helper that also *opened* the store would have to import both backends and
// close the import cycle. Deciding is separable from constructing, so only the
// decision is shared.
func Classify(dsn string) (Backend, string) {
	switch {
	case strings.HasPrefix(dsn, "postgres://"), strings.HasPrefix(dsn, "postgresql://"):
		return PostgresBackend, dsn
	case strings.HasPrefix(dsn, "sqlite://"):
		return SQLiteBackend, strings.TrimPrefix(dsn, "sqlite://")
	default:
		return SQLiteBackend, dsn
	}
}
