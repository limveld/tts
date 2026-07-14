package main

import (
	"strings"
	"testing"
)

func TestGambleWin(t *testing.T) {
	r, _, st, chat := econRouter(t)
	r.econ.GambleWinChance = 1 // always win
	st.Credit("id-bob", 100, "accrual", "")

	r.Handle(emsg("bob", "!gamble 40", true)) // mod: no cooldown in the way
	if b, _ := st.Balance("id-bob"); b != 140 {
		t.Fatalf("balance=%d want 140 (won 40)", b)
	}
	if !strings.Contains(lastReply(chat), "won 40 marks") {
		t.Fatalf("reply=%q want a win message", lastReply(chat))
	}
}

func TestGambleLose(t *testing.T) {
	r, _, st, chat := econRouter(t)
	r.econ.GambleWinChance = 0 // always lose
	st.Credit("id-bob", 100, "accrual", "")

	r.Handle(emsg("bob", "!gamble 40", true))
	if b, _ := st.Balance("id-bob"); b != 60 {
		t.Fatalf("balance=%d want 60 (lost 40)", b)
	}
	if !strings.Contains(lastReply(chat), "lost 40 marks") {
		t.Fatalf("reply=%q want a loss message", lastReply(chat))
	}
}

func TestGambleAll(t *testing.T) {
	r, _, st, _ := econRouter(t)
	r.econ.GambleWinChance = 1
	st.Credit("id-bob", 250, "accrual", "")

	r.Handle(emsg("bob", "!gamble all", true))
	if b, _ := st.Balance("id-bob"); b != 500 {
		t.Fatalf("balance=%d want 500 (doubled all)", b)
	}
}

func TestGambleBelowMinBet(t *testing.T) {
	r, _, st, chat := econRouter(t) // GambleMinBet=10
	st.Credit("id-bob", 100, "accrual", "")

	r.Handle(emsg("bob", "!gamble 5", true))
	if b, _ := st.Balance("id-bob"); b != 100 {
		t.Fatalf("balance=%d want 100 (bet rejected)", b)
	}
	if !strings.Contains(lastReply(chat), "minimum bet is 10 marks") {
		t.Fatalf("reply=%q want min-bet message", lastReply(chat))
	}
}

func TestGambleMoreThanBalance(t *testing.T) {
	r, _, st, chat := econRouter(t)
	st.Credit("id-bob", 30, "accrual", "")

	r.Handle(emsg("bob", "!gamble 100", true))
	if b, _ := st.Balance("id-bob"); b != 30 {
		t.Fatalf("balance=%d want 30 (unchanged)", b)
	}
	if !strings.Contains(lastReply(chat), "you only have 30 marks") {
		t.Fatalf("reply=%q want insufficient message", lastReply(chat))
	}
}

func TestGiveTransfers(t *testing.T) {
	r, _, st, chat := econRouter(t)
	st.UpsertUser("id-bob", "bob", "Bob")
	st.UpsertUser("id-amy", "amy", "Amy")
	st.Credit("id-bob", 100, "accrual", "")

	r.Handle(emsg("bob", "!give @amy 30", true))
	if b, _ := st.Balance("id-bob"); b != 70 {
		t.Fatalf("giver=%d want 70", b)
	}
	if b, _ := st.Balance("id-amy"); b != 30 {
		t.Fatalf("recipient=%d want 30", b)
	}
	if !strings.Contains(lastReply(chat), "gave 30 marks to @amy") {
		t.Fatalf("reply=%q", lastReply(chat))
	}
}

func TestGiveSelfBlocked(t *testing.T) {
	r, _, st, chat := econRouter(t)
	st.UpsertUser("id-bob", "bob", "Bob")
	st.Credit("id-bob", 100, "accrual", "")

	r.Handle(emsg("bob", "!give @bob 30", true))
	if b, _ := st.Balance("id-bob"); b != 100 {
		t.Fatalf("balance=%d want 100 (self-give blocked)", b)
	}
	if !strings.Contains(lastReply(chat), "can't give to yourself") {
		t.Fatalf("reply=%q", lastReply(chat))
	}
}

func TestGiveUnseenRecipient(t *testing.T) {
	r, _, st, chat := econRouter(t)
	st.Credit("id-bob", 100, "accrual", "")

	r.Handle(emsg("bob", "!give @ghost 30", true))
	if b, _ := st.Balance("id-bob"); b != 100 {
		t.Fatalf("balance=%d want 100 (recipient unknown)", b)
	}
	if lastReply(chat) != "haven't seen @ghost yet." {
		t.Fatalf("reply=%q", lastReply(chat))
	}
}

func TestGiveMoreThanBalance(t *testing.T) {
	r, _, st, chat := econRouter(t)
	st.UpsertUser("id-bob", "bob", "Bob")
	st.UpsertUser("id-amy", "amy", "Amy")
	st.Credit("id-bob", 20, "accrual", "")

	r.Handle(emsg("bob", "!give @amy 100", true))
	if b, _ := st.Balance("id-bob"); b != 20 {
		t.Fatalf("giver=%d want 20 (unchanged)", b)
	}
	if b, _ := st.Balance("id-amy"); b != 0 {
		t.Fatalf("recipient=%d want 0", b)
	}
	if !strings.Contains(lastReply(chat), "you only have 20 marks") {
		t.Fatalf("reply=%q", lastReply(chat))
	}
}

func TestGamesDisabledWhenEconomyOff(t *testing.T) {
	r, _, st, _ := econRouter(t)
	r.economy = false
	st.UpsertUser("id-amy", "amy", "Amy")
	st.Credit("id-bob", 100, "accrual", "")

	// With the economy off, !gamble/!give aren't built-ins (they'd fall through
	// to custom-command lookup, which finds nothing) — no balance change.
	r.Handle(emsg("bob", "!gamble 40", true))
	r.Handle(emsg("bob", "!give @amy 40", true))
	if b, _ := st.Balance("id-bob"); b != 100 {
		t.Fatalf("balance=%d want 100 (games inert when economy off)", b)
	}
}
