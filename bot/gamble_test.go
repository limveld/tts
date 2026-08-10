package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tts/store"
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

// --- durability: a round must survive a bot restart -------------------------

// crashGamble simulates the bot dying mid-round: the deadline is moved to
// `remaining` from now (negative = already passed), the record is re-persisted,
// and the in-memory round is dropped along with its timers — exactly the state a
// restart leaves behind.
func crashGamble(r *Router, remaining time.Duration) {
	r.gambleMu.Lock()
	defer r.gambleMu.Unlock()
	r.round.endsAt = time.Now().Add(remaining)
	r.persistGamble(r.round)
	r.round = nil
}

// rebootRouter builds a fresh router over an existing store: same durable ledger
// and settings, brand-new in-memory state. This is the bot coming back up.
func rebootRouter(t *testing.T, st *store.Store) (*Router, *fakeChat, *fakeOverlay) {
	t.Helper()
	r, _, _, chat := econRouter(t)
	r.store = st
	ov := &fakeOverlay{}
	r.overlay = ov
	return r, chat, ov
}

// persistedRound decodes the stored round, or reports that none is stored.
func persistedRound(t *testing.T, st *store.Store) (gambleRec, bool) {
	t.Helper()
	v, ok, err := st.GetSetting(gambleSettingKey)
	if err != nil {
		t.Fatalf("GetSetting: %v", err)
	}
	if !ok || v == "" {
		return gambleRec{}, false
	}
	var rec gambleRec
	if err := json.Unmarshal([]byte(v), &rec); err != nil {
		t.Fatalf("unmarshal %q: %v", v, err)
	}
	return rec, true
}

func TestGamblePersistsOpenRound(t *testing.T) {
	r, _, st, _ := econRouter(t)
	st.Credit("id-alice", 500, "accrual", "")
	r.Handle(gmsg("alice", "!g 100"))

	rec, ok := persistedRound(t, st)
	if !ok {
		t.Fatal("open round was not persisted")
	}
	if rec.BuyIn != 100 || len(rec.Entrants) != 1 || rec.Entrants[0].UserID != "id-alice" {
		t.Fatalf("rec=%+v want buy-in 100 with alice", rec)
	}
	if rec.Winner != -1 || rec.ID == "" || rec.EndsAt == 0 {
		t.Fatalf("rec=%+v want winner=-1 with an id and deadline", rec)
	}
}

func TestGamblePersistsJoin(t *testing.T) {
	r, _, st, _ := econRouter(t)
	st.Credit("id-alice", 500, "accrual", "")
	st.Credit("id-bob", 500, "accrual", "")
	r.Handle(gmsg("alice", "!g 100"))
	r.Handle(gmsg("bob", "!g"))

	rec, _ := persistedRound(t, st)
	if len(rec.Entrants) != 2 || rec.Entrants[1].UserID != "id-bob" {
		t.Fatalf("rec.Entrants=%+v want alice then bob", rec.Entrants)
	}
}

func TestGambleClearsPersistOnResolve(t *testing.T) {
	r, _, st, _ := econRouter(t)
	st.Credit("id-alice", 500, "accrual", "")
	r.Handle(gmsg("alice", "!g 100"))
	r.resolveGamble(r.round)

	if rec, ok := persistedRound(t, st); ok {
		t.Fatalf("round still persisted after resolve: %+v", rec)
	}
}

// The reported bug: alice opens a gamble, nobody joins, the bot restarts inside
// the round window — and her buy-in is gone forever because the only timer that
// would have refunded her died with the process.
func TestGambleRestartRefundsSoloRound(t *testing.T) {
	r, _, st, _ := econRouter(t)
	st.Credit("id-alice", 500, "accrual", "")
	r.Handle(gmsg("alice", "!g 100"))
	if b, _ := st.Balance("id-alice"); b != 400 {
		t.Fatalf("balance=%d want 400 escrowed", b)
	}
	crashGamble(r, -time.Second) // deadline passed while the bot was down

	r2, chat2, _ := rebootRouter(t, st)
	r2.loadGamble()

	if b, _ := st.Balance("id-alice"); b != 500 {
		t.Fatalf("balance=%d want 500 — the escrowed buy-in was not refunded", b)
	}
	if r2.round != nil {
		t.Fatalf("round still live after settling: %+v", r2.round)
	}
	if rec, ok := persistedRound(t, st); ok {
		t.Fatalf("settled round still persisted: %+v", rec)
	}
	if !strings.Contains(lastSend(chat2), "refunded") {
		t.Fatalf("no refund announcement; last=%q", lastSend(chat2))
	}
}

func TestGambleRestartPaysOutMultiplayerRound(t *testing.T) {
	r, _, st, _ := econRouter(t)
	st.Credit("id-alice", 500, "accrual", "")
	st.Credit("id-bob", 500, "accrual", "")
	r.Handle(gmsg("alice", "!g 100"))
	r.Handle(gmsg("bob", "!g"))
	crashGamble(r, -time.Second)

	r2, chat2, _ := rebootRouter(t, st)
	r2.loadGamble()

	a, _ := st.Balance("id-alice")
	b, _ := st.Balance("id-bob")
	if a+b != 1000 {
		t.Fatalf("alice=%d bob=%d — marks not conserved (want 1000 total)", a, b)
	}
	if a != 600 && b != 600 {
		t.Fatalf("alice=%d bob=%d — nobody was paid the 200 pot", a, b)
	}
	if !strings.Contains(lastSend(chat2), "wins the pot") {
		t.Fatalf("no payout announcement; last=%q", lastSend(chat2))
	}
	if _, ok := persistedRound(t, st); ok {
		t.Fatal("paid-out round still persisted")
	}
}

func TestGambleRestartReschedulesOpenRound(t *testing.T) {
	r, _, st, _ := econRouter(t)
	st.Credit("id-alice", 500, "accrual", "")
	st.Credit("id-bob", 500, "accrual", "")
	r.Handle(gmsg("alice", "!g 100"))
	r.Handle(gmsg("bob", "!g"))
	crashGamble(r, 50*time.Second) // still inside the window

	r2, _, ov2 := rebootRouter(t, st)
	r2.loadGamble()

	rd := r2.round
	if rd == nil || len(rd.entrants) != 2 || !rd.joined["id-bob"] {
		t.Fatalf("round not resumed as open: %+v", rd)
	}
	if a, _ := st.Balance("id-alice"); a != 400 {
		t.Fatalf("alice=%d — a resumed round must not settle yet", a)
	}
	p, ok := ov2.last("gamble")
	if !ok {
		t.Fatal("no overlay push for the resumed round")
	}
	d := p.data.(gamblePanelData)
	if d.Phase != "open" || d.EndsAt == 0 || d.Pot != 200 {
		t.Fatalf("panel=%+v want open with a deadline and the 200 pot", d)
	}
}

// A long outage shouldn't surprise chat with a payout for a round nobody
// remembers — an undrawn round that went stale refunds instead.
func TestGambleStaleRoundRefundsInsteadOfPayingOut(t *testing.T) {
	r, _, st, _ := econRouter(t)
	st.Credit("id-alice", 500, "accrual", "")
	st.Credit("id-bob", 500, "accrual", "")
	r.Handle(gmsg("alice", "!g 100"))
	r.Handle(gmsg("bob", "!g"))
	crashGamble(r, -(gambleStaleAfter + time.Minute))

	r2, chat2, _ := rebootRouter(t, st)
	r2.loadGamble()

	a, _ := st.Balance("id-alice")
	b, _ := st.Balance("id-bob")
	if a != 500 || b != 500 {
		t.Fatalf("alice=%d bob=%d want both refunded to 500", a, b)
	}
	if strings.Contains(lastSend(chat2), "wins the pot") {
		t.Fatalf("stale round paid out instead of refunding; last=%q", lastSend(chat2))
	}
}

// A restore can race the original timer, so settling twice must not pay twice.
func TestGambleSettleIsIdempotent(t *testing.T) {
	r, _, st, _ := econRouter(t)
	st.Credit("id-alice", 500, "accrual", "")
	st.Credit("id-bob", 500, "accrual", "")
	r.Handle(gmsg("alice", "!g 100"))
	r.Handle(gmsg("bob", "!g"))
	round := r.round
	r.resolveGamble(round)

	a1, _ := st.Balance("id-alice")
	b1, _ := st.Balance("id-bob")
	winner := round.winner

	r.settleGamble(round, false) // as if a restored round re-settled

	a2, _ := st.Balance("id-alice")
	b2, _ := st.Balance("id-bob")
	if a1 != a2 || b1 != b2 {
		t.Fatalf("second settle moved marks: alice %d->%d bob %d->%d", a1, a2, b1, b2)
	}
	if round.winner != winner {
		t.Fatalf("winner re-drawn on re-settle: %d -> %d", winner, round.winner)
	}
}

func TestGambleRefundIsIdempotent(t *testing.T) {
	r, _, st, _ := econRouter(t)
	st.Credit("id-alice", 500, "accrual", "")
	r.Handle(gmsg("alice", "!g 100"))
	round := r.round
	r.resolveGamble(round)
	r.settleGamble(round, false)

	if b, _ := st.Balance("id-alice"); b != 500 {
		t.Fatalf("balance=%d want 500 — the refund was applied twice", b)
	}
}

// A failed payout must still show and announce the result (the old code returned
// early, leaving the overlay stuck on "open" forever) and must keep the record so
// the next boot can retry the same ledger ref.
func TestGamblePayoutErrorKeepsRoundPersisted(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "g.db")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	r, chat, ov := rebootRouter(t, st)
	st.Credit("id-alice", 500, "accrual", "")
	st.Credit("id-bob", 500, "accrual", "")
	r.Handle(gmsg("alice", "!g 100"))
	r.Handle(gmsg("bob", "!g"))
	round := r.round

	st.Close() // every store call from here on fails
	r.resolveGamble(round)

	p, _ := ov.last("gamble")
	if d := p.data.(gamblePanelData); d.Phase != "result" {
		t.Fatalf("panel=%+v want a result push despite the failed payout", d)
	}
	if !strings.Contains(lastSend(chat), "retrying") {
		t.Fatalf("no chat line for the failed payout; last=%q", lastSend(chat))
	}

	st2, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	if _, ok := persistedRound(t, st2); !ok {
		t.Fatal("record cleared after a failed payout — the retry is now impossible")
	}
}

func TestGambleHideSkippedWhenNewRoundOpen(t *testing.T) {
	r, _, st, _ := econRouter(t)
	ov := &fakeOverlay{}
	r.overlay = ov
	st.Credit("id-alice", 500, "accrual", "")
	r.Handle(gmsg("alice", "!g 100"))
	r.resolveGamble(r.round)
	r.Handle(gmsg("alice", "!g 100")) // a new round within the linger

	r.hideGambleIfIdle()

	p, _ := ov.last("gamble")
	if d := p.data.(gamblePanelData); d.Phase != "open" {
		t.Fatalf("panel=%+v — the linger from the old round wiped the live one", d)
	}
}

// An unauthenticated boot still has to move the money: chat is nil, the escrow
// in the ledger is real.
func TestGambleRestoreWithoutChatDoesNotPanic(t *testing.T) {
	r, _, st, _ := econRouter(t)
	st.Credit("id-alice", 500, "accrual", "")
	r.Handle(gmsg("alice", "!g 100"))
	crashGamble(r, -time.Second)

	r2, _, _ := rebootRouter(t, st)
	r2.chat = nil
	r2.loadGamble()

	if b, _ := st.Balance("id-alice"); b != 500 {
		t.Fatalf("balance=%d want 500 refunded even with no chat", b)
	}
}

// The resolve timer draws a winner on its own goroutine while the IRC handler
// draws for $random / !wordle / !connections. Fails under -race if the shared
// *rand.Rand is touched directly instead of through randIntn.
func TestGambleDrawIsRaceFreeWithHandlerDraws(t *testing.T) {
	r, _, st, _ := econRouter(t)
	st.Credit("id-alice", 500, "accrual", "")
	st.Credit("id-bob", 500, "accrual", "")
	r.Handle(gmsg("alice", "!g 100"))
	r.Handle(gmsg("bob", "!g"))
	round := r.round

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			substitute("roll $random", subCtx{randN: r.randIntn})
		}
	}()
	r.resolveGamble(round)
	<-done
}
