// Package storetest is the conformance suite every store backend must pass. One
// set of test bodies runs against SQLite and Postgres in the same
// `go test ./...`, which is the only thing that makes "two backends" a claim
// rather than a hope.
//
// The Store interface here duplicates the bot's capability interfaces on
// purpose. The bot's are the consumer's view of what it needs; this one is the
// contract a backend owes. They drift only when a feature is added to one and
// not the other — which is exactly when a compile error is what you want.
package storetest

import (
	"testing"

	"tts/store"
)

// Store is the full contract a backend owes.
type Store interface {
	// Commands
	Get(name string) (store.Command, bool, error)
	Add(c store.Command) (created bool, err error)
	SetResponse(name, response string) (found bool, err error)
	Delete(name string) (found bool, err error)
	List() ([]string, error)
	IncCount(name string) error

	// Ledger
	UpsertUser(userID, login, display string) error
	ResolveLogin(login string) (userID string, ok bool, err error)
	Balance(userID string) (int64, error)
	Credit(userID string, amount int64, reason, ref string) (credited bool, err error)
	Grant(userID string, delta int64, reason string) (newBal int64, err error)
	Spend(userID string, amount int64, reason string) (ok bool, err error)
	Transfer(fromID, toID string, amount int64, reason string) (ok bool, err error)
	Leaderboard(n int) ([]store.LedgerEntry, error)

	// Settings
	GetSetting(key string) (value string, ok bool, err error)
	SetSetting(key, value string) error

	// Rounds
	SaveRound(game, roomID string, endsAt int64, state []byte) error
	LoadRound(game string) (store.Round, bool, error)
	ClearRound(game string) error

	// Game tallies
	WordleAddWin(userID, login, display string) (wins int, err error)
	WordleLeaderboard(n int) ([]store.WordleWin, error)
	ConnectionsAddWin(userID, login, display string) (wins int, err error)
	ConnectionsLeaderboard(n int) ([]store.ConnectionsWin, error)
}

// New builds one isolated store. Implementations must hand back a store that
// shares nothing with any other call (its own file, its own schema); the suite
// writes freely and never cleans up after itself.
type New func(t *testing.T) Store

// Run executes the behavioral conformance suite. Every case gets a fresh store.
func Run(t *testing.T, newStore New) {
	cases := []struct {
		name string
		body func(*testing.T, Store)
	}{
		{"CommandCRUD", testCommandCRUD},
		{"CommandListSorted", testCommandListSorted},
		{"CreditSpendBalance", testCreditSpendBalance},
		{"CreditIdempotentRef", testCreditIdempotentRef},
		{"SpendInsufficient", testSpendInsufficient},
		{"UsersAndLeaderboard", testUsersAndLeaderboard},
		{"LeaderboardExcludesZeroAndNegative", testLeaderboardExcludesZeroAndNegative},
		{"LeaderboardMatchesBalances", testLeaderboardMatchesBalances},
		{"LeaderboardOrderAndTieBreak", testLeaderboardOrderAndTieBreak},
		{"GrantMintAndClamp", testGrantMintAndClamp},
		{"Transfer", testTransfer},
		{"TransferInsufficient", testTransferInsufficient},
		{"TransferSelfIsNoop", testTransferSelfIsNoop},
		{"Settings", testSettings},
		{"WordleTally", testWordleTally},
		{"ConnectionsTally", testConnectionsTally},
		{"RoundSaveLoadClear", testRoundSaveLoadClear},
		{"RoundOverwrite", testRoundOverwrite},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { c.body(t, newStore(t)) })
	}
}

// RunConcurrent executes the concurrency suite: the cases that only mean
// something on a backend with more than one writer.
//
// It is not run against SQLite. SQLite serializes writers by construction, so
// these would pass there without exercising anything — at best they'd test
// busy_timeout, at worst they'd give false confidence that the locking these
// cases exist to check is present.
func RunConcurrent(t *testing.T, newStore New) {
	cases := []struct {
		name string
		body func(*testing.T, Store)
	}{
		{"ConcurrentSpendCannotOverdraw", testConcurrentSpendCannotOverdraw},
		{"BidirectionalTransfersDoNotDeadlock", testBidirectionalTransfersDoNotDeadlock},
		{"ConcurrentCreditWithSameRefCreditsOnce", testConcurrentCreditWithSameRefCreditsOnce},
		{"ConcurrentGrantAndSpendNeverGoesNegative", testConcurrentGrantAndSpendNeverGoesNegative},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) { c.body(t, newStore(t)) })
	}
}
