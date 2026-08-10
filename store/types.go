// Package store holds the bot's persisted domain types. It is deliberately
// type-only: the backends live in child packages (store/sqlite, store/postgres)
// and import this one for these types, so this package must never import them
// back. That is also why there is no store.Open(dsn) factory here — a factory
// would have to import every backend and close the cycle. The dispatch lives at
// the consumer instead, in bot/store.go.
package store

// Command is one chat-managed custom command. Name is without the leading "!"
// and lowercased. Cooldown is a global per-command cooldown in seconds; MinRole
// is everyone|sub|vip|mod.
type Command struct {
	Name     string
	Response string
	Cooldown int
	MinRole  string
	Count    int
}

// LedgerEntry is one leaderboard row (a user_id's summed balance with its name).
type LedgerEntry struct {
	UserID  string
	Login   string
	Display string
	Balance int64
}

// WordleWin is one leaderboard row.
type WordleWin struct {
	Login   string
	Display string
	Wins    int
}

// ConnectionsWin is one leaderboard row.
type ConnectionsWin struct {
	Login   string
	Display string
	Wins    int
}

// Round is one in-flight game round: the durable state of gamble/wordle/
// connections. State is the game's own JSON document and the store never looks
// inside it. RoomID and EndsAt (unix millis, 0 = none) are lifted out as columns
// so a round is legible without decoding — the game code still reads them from
// its own document, so nothing downstream depends on the split.
type Round struct {
	Game      string
	RoomID    string
	EndsAt    int64
	State     []byte
	UpdatedAt int64
}
