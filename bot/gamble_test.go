package main

import (
	"strings"
	"testing"
)

// The gamble tests drive resolveGamble/flushJoins directly instead of waiting on
// the real timers, so they're deterministic. econRouter seeds r.rnd.

func lastSend(chat *fakeChat) string {
	if len(chat.sends) == 0 {
		return ""
	}
	return chat.sends[len(chat.sends)-1]
}

// gmsg is a regular (non-mod) chat message with UserID + RoomID set, for gamble.
func gmsg(user, text string) ChatMessage { return emsg(user, text, false) }

func TestGambleOpenEscrowsAndAnnounces(t *testing.T) {
	r, _, st, chat := econRouter(t)
	st.Credit("id-alice", 500, "accrual", "")

	r.Handle(gmsg("alice", "!g 100"))
	if b, _ := st.Balance("id-alice"); b != 400 {
		t.Fatalf("opener balance=%d want 400 (100 escrowed)", b)
	}
	if r.round == nil || r.round.buyIn != 100 || len(r.round.entrants) != 1 {
		t.Fatalf("round=%+v", r.round)
	}
	if !strings.Contains(lastSend(chat), "started a gamble") || !strings.Contains(lastSend(chat), "100 marks") {
		t.Fatalf("announce=%q", lastSend(chat))
	}
}

func TestGamblePlayThroughPaysWinner(t *testing.T) {
	r, _, st, chat := econRouter(t)
	st.Credit("id-alice", 500, "accrual", "")
	st.Credit("id-bob", 500, "accrual", "")

	r.Handle(gmsg("alice", "!g 100")) // opens, alice escrowed
	r.Handle(gmsg("bob", "!g"))       // joins, bob escrowed
	if b, _ := st.Balance("id-bob"); b != 400 {
		t.Fatalf("bob balance=%d want 400 (escrowed)", b)
	}
	round := r.round
	r.resolveGamble(round) // deterministic draw via seeded rnd

	if r.round != nil {
		t.Fatal("round should be cleared after resolve")
	}
	// Pot of 200 goes to one of them; totals are conserved (1000 total).
	a, _ := st.Balance("id-alice")
	b, _ := st.Balance("id-bob")
	if a+b != 1000 {
		t.Fatalf("total=%d want 1000 (conserved)", a+b)
	}
	if !(a == 600 && b == 400) && !(a == 400 && b == 600) {
		t.Fatalf("balances a=%d b=%d — one should hold the 200 pot", a, b)
	}
	if !strings.Contains(lastSend(chat), "wins the pot of 200 marks") {
		t.Fatalf("result=%q", lastSend(chat))
	}
}

func TestGambleCancelRefundsWhenAlone(t *testing.T) {
	r, _, st, chat := econRouter(t)
	st.Credit("id-alice", 500, "accrual", "")

	r.Handle(gmsg("alice", "!g 100"))
	r.resolveGamble(r.round) // only the opener → cancel

	if b, _ := st.Balance("id-alice"); b != 500 {
		t.Fatalf("alice balance=%d want 500 (refunded)", b)
	}
	if !strings.Contains(lastSend(chat), "cancelled") || !strings.Contains(lastSend(chat), "refunded") {
		t.Fatalf("cancel msg=%q", lastSend(chat))
	}
}

func TestGambleJoinCantAfford(t *testing.T) {
	r, _, st, chat := econRouter(t)
	st.Credit("id-alice", 500, "accrual", "")
	st.Credit("id-bob", 30, "accrual", "")

	r.Handle(gmsg("alice", "!g 100"))
	r.Handle(gmsg("bob", "!g")) // can't afford 100
	if b, _ := st.Balance("id-bob"); b != 30 {
		t.Fatalf("bob balance=%d want 30 (not entered)", b)
	}
	if len(r.round.entrants) != 1 {
		t.Fatalf("entrants=%d want 1 (bob not entered)", len(r.round.entrants))
	}
	if !strings.Contains(lastReply(chat), "buy-in is 100 marks") {
		t.Fatalf("reply=%q", lastReply(chat))
	}
}

func TestGambleDuplicateJoin(t *testing.T) {
	r, _, st, chat := econRouter(t)
	st.Credit("id-alice", 500, "accrual", "")
	st.Credit("id-bob", 500, "accrual", "")

	r.Handle(gmsg("alice", "!g 100"))
	r.Handle(gmsg("bob", "!g"))
	r.Handle(gmsg("bob", "!g")) // dup
	if len(r.round.entrants) != 2 {
		t.Fatalf("entrants=%d want 2 (no dup)", len(r.round.entrants))
	}
	if b, _ := st.Balance("id-bob"); b != 400 {
		t.Fatalf("bob balance=%d want 400 (charged once)", b)
	}
	if !strings.Contains(lastReply(chat), "already in") {
		t.Fatalf("reply=%q", lastReply(chat))
	}
}

func TestGambleAllOpener(t *testing.T) {
	r, _, st, _ := econRouter(t)
	st.Credit("id-alice", 250, "accrual", "")

	r.Handle(gmsg("alice", "!g all"))
	if r.round == nil || r.round.buyIn != 250 {
		t.Fatalf("buyIn=%v want 250 (all)", r.round)
	}
	if b, _ := st.Balance("id-alice"); b != 0 {
		t.Fatalf("alice balance=%d want 0 (all escrowed)", b)
	}
}

func TestGambleCoalescedJoinLine(t *testing.T) {
	r, _, st, chat := econRouter(t)
	for _, u := range []string{"alice", "bob", "carol"} {
		st.Credit("id-"+u, 500, "accrual", "")
	}
	r.Handle(gmsg("alice", "!g 100"))
	sendsBefore := len(chat.sends)
	r.Handle(gmsg("bob", "!g"))
	r.Handle(gmsg("carol", "!g"))
	// No per-join sends yet (buffered); flush groups them into one line.
	if len(chat.sends) != sendsBefore {
		t.Fatalf("joins should be buffered, not sent immediately; sends grew by %d", len(chat.sends)-sendsBefore)
	}
	r.flushJoins(r.round)
	got := lastSend(chat)
	if !strings.Contains(got, "@bob") || !strings.Contains(got, "@carol") || !strings.Contains(got, "3 players") {
		t.Fatalf("coalesced line=%q want both names + 3 players", got)
	}
}

func TestGamblePushesPanelState(t *testing.T) {
	r, _, st, _ := econRouter(t)
	ov := &fakeOverlay{}
	r.overlay = ov
	st.Credit("id-alice", 500, "accrual", "")
	st.Credit("id-bob", 500, "accrual", "")

	// open -> panel with pot=100, 1 player, a deadline for the countdown.
	r.Handle(gmsg("alice", "!g 100"))
	p, ok := ov.last("gamble")
	if !ok {
		t.Fatal("no gamble push on open")
	}
	d := p.data.(gamblePanelData)
	if d.Phase != "open" || d.Pot != 100 || d.Players != 1 || d.EndsAt == 0 {
		t.Fatalf("open panel=%+v want phase=open pot=100 players=1 endsAt>0", d)
	}

	// join -> pot=200, 2 players.
	r.Handle(gmsg("bob", "!g"))
	p, _ = ov.last("gamble")
	d = p.data.(gamblePanelData)
	if d.Phase != "open" || d.Pot != 200 || d.Players != 2 {
		t.Fatalf("join panel=%+v want pot=200 players=2", d)
	}

	// resolve -> result with a winner.
	r.resolveGamble(r.round)
	p, _ = ov.last("gamble")
	d = p.data.(gamblePanelData)
	if d.Phase != "result" || d.Winner == "" {
		t.Fatalf("result panel=%+v want phase=result winner set", d)
	}
}

func TestGambleCancelPushesCancelledPanel(t *testing.T) {
	r, _, st, _ := econRouter(t)
	ov := &fakeOverlay{}
	r.overlay = ov
	st.Credit("id-alice", 500, "accrual", "")

	r.Handle(gmsg("alice", "!g 100"))
	r.resolveGamble(r.round) // alone -> cancelled

	p, _ := ov.last("gamble")
	d := p.data.(gamblePanelData)
	if d.Phase != "result" || !d.Cancelled {
		t.Fatalf("cancel panel=%+v want phase=result cancelled=true", d)
	}
}

func TestGambleBelowMinBet(t *testing.T) {
	r, _, st, chat := econRouter(t) // GambleMinBet=10
	st.Credit("id-alice", 500, "accrual", "")

	r.Handle(gmsg("alice", "!g 5"))
	if r.round != nil {
		t.Fatal("round should not open below min buy-in")
	}
	if b, _ := st.Balance("id-alice"); b != 500 {
		t.Fatalf("balance=%d want 500 (nothing escrowed)", b)
	}
	if !strings.Contains(lastReply(chat), "minimum buy-in is 10 marks") {
		t.Fatalf("reply=%q", lastReply(chat))
	}
}
