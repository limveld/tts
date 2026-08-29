package main

import (
	"tts/store"
	"tts/store/postgres"
	"tts/store/sqlite"
)

// The bot is the sole owner of the database. The server process has none and
// gains none: it is a stateless relay that renders whatever the bot pushes it.
//
// The store's capabilities are declared as separate interfaces in the files that
// consume them — CommandStore in commands.go, Ledger in economy.go, WordleWins
// in wordle.go, ConnectionsWins in connections.go, ChatLog in chatlog.go,
// SettingStore here — so a consumer's dependencies are visible where the
// consumer is, and its fakes have to prove exactly that much. Store is the
// composite: the *field* type, so Router carries one store rather than seven.

// SettingStore is the key/value slice of the store used for runtime toggles —
// the charge mode and the depth widget's totals (an interface so tests can
// substitute a fake). *sqlite.Store and *postgres.Store satisfy it.
type SettingStore interface {
	GetSetting(key string) (value string, ok bool, err error)
	SetSetting(key, value string) error
}

// Store is everything the bot needs from a persistence backend: every capability
// interface plus Close. Backends satisfy it in full or not at all — see the
// compile-time assertions below.
//
// A nil Store means persistence is disabled, and every consumer checks for it
// (`if r.store == nil { return }`). That contract is why a *typed* nil must
// never be assigned to the field: `var s *sqlite.Store; r.store = s` leaves
// r.store != nil, and every one of those guards falls through into a nil-pointer
// panic on the first call. Construct through openStore, or leave the field zero.
type Store interface {
	CommandStore
	Ledger
	SettingStore
	RoundStore
	WordleWins
	ConnectionsWins
	ChatLog

	Close() error
}

// The backends satisfy the contract, or the build fails. Once there is a
// conformance suite this is no longer the only proof, but it stays the fastest
// one.
var (
	_ Store = (*sqlite.Store)(nil)
	_ Store = (*postgres.Store)(nil)
)

// openStore opens the backend named by dsn. What a DSN means is decided once, in
// store.Classify, so this and cmd/store-migrate can't drift: a "postgres://" URL
// selects Postgres, a "sqlite://" URL or a bare filesystem path selects SQLite.
// A bare path is the historical spelling and stays the default, so nothing about
// the current invocation changes.
func openStore(dsn string) (Store, error) {
	backend, target := store.Classify(dsn)
	if backend == store.PostgresBackend {
		return postgres.Open(target)
	}
	return sqlite.Open(target)
}
