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

// MazeWin is one leaderboard row.
type MazeWin struct {
	Login   string
	Display string
	Wins    int
}

// MazeRound is one finished Torch Maze round, kept permanently so a game can be
// reviewed or replayed.
//
// Input is opaque here, exactly as Round.State is: the store carries the document
// and never looks inside it. It holds the board, both configs and the ordered
// submissions — everything needed to re-run the round. The columns beside it are
// lifted out of that document so a round is legible in psql without decoding
// anything, which is the same bargain game_rounds makes with room_id and ends_at.
type MazeRound struct {
	ID     string
	RoomID string
	Seed   int64

	StartedAt int64 // unix seconds
	EndedAt   int64
	TickMS    int64
	Cycles    int
	Reason    string // an end-reason wire value, or "skipped"

	Players   int
	Finishers int

	WinnerID      string // empty when nobody got out
	WinnerLogin   string
	WinnerDisplay string

	Input []byte
}

// MazeEvent is one thing that happened during a round, in emission order.
//
// The player's name is denormalized rather than joined, following chat_message:
// users is written only by the economy and is empty when that is off, and an
// archive should carry the name as it was at the time regardless — people rename.
type MazeEvent struct {
	RoundID string
	Seq     int // 0-based, across the whole round
	Cycle   int

	Kind    string
	Seat    int // -1 for round-level events
	UserID  string
	Login   string
	Display string

	At     string // chat coordinate ("C4"); empty for kinds with no position
	N      int    // meaning depends on Kind; see internal/maze/round.go
	Reason string // set only by round-ended
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
