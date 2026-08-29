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

// ChatMessage is one persisted line of chat.
//
// It is deliberately not the bot's ChatMessage. That one carries IRC parsing
// concerns — the raw emotes tag positioned against Text, role badges the router
// branches on — and this one carries columns. The conversion happens at the
// boundary, in bot/chatlog.go, so neither type has to grow the other's shape.
//
// Text is stored raw, exactly as it arrived. removeEmotes can always recompute
// the stripped form from Text plus Emotes; nothing recovers the original from
// the stripped one.
//
// MsgID is Twitch's own message id (the `id` tag), and is what CLEARMSG targets.
// Login and Display are the names as they were when the line was sent — people
// rename, and an archive that silently rewrites history is not an archive.
//
// DeletedAt is 0 while the message is live; a tombstone sets it and names what
// removed the line in DeletedBy ("clearmsg" for a single deletion, "clearchat"
// for a ban or timeout). Text survives a tombstone on purpose: the moderation
// question is "what did they say that got them banned", and redacting answers
// the one case the log exists for with a blank.
type ChatMessage struct {
	ID      int64
	TS      int64 // unix seconds
	RoomID  string
	MsgID   string
	UserID  string
	Login   string
	Display string
	Text    string
	Emotes  string

	IsMod         bool
	IsSub         bool
	IsVIP         bool
	IsBroadcaster bool

	DeletedAt int64  // 0 = live
	DeletedBy string // "clearmsg" | "clearchat" | ""
}

// ChatStat is one user's folded chat totals — the only chat history that
// outlives retention.
//
// It is the ledger_opening pattern, not the accounts.balance one: nothing on the
// write path maintains it, so it counts only messages whose partition has
// already been folded and dropped. A user's true total is Messages plus a live
// COUNT over chat_message, and either half alone is an undercount. See
// docs/adr/0003-chat-log.md.
type ChatStat struct {
	UserID   string
	Login    string
	Display  string
	Messages int64
	Chars    int64
	FirstTS  int64 // oldest folded message
	LastTS   int64 // newest folded message
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
