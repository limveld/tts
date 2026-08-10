package main

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// The !g multiplayer pot gamble. Someone opens with "!g <amount>" (a buy-in);
// others join by typing "!g" during econ.GambleDuration. Buy-ins are escrowed on
// join; at the deadline a uniform-random winner takes the whole pot, or — if
// fewer than 2 players joined — the round is cancelled and everyone refunded.
//
// One round runs at a time. Round state lives on the Router under gambleMu
// because the resolve/reminder/coalesced-join timers fire on their own goroutines
// while joins arrive on the (sequential) IRC handler. DB writes go through the
// store's atomic helpers; chat sends happen after the lock is released.
//
// Because the round holds real escrowed marks, it is durable: every mutation
// mirrors to the gamble_round settings key, and loadGamble restores the round at
// startup and reschedules its deadline — otherwise a restart mid-round would kill
// the only timer that settles it and strand every buy-in. Three rules keep a
// re-settle safe:
//
//   - Refunds and payouts carry a "gamble:<round id>:…" ledger ref, so the store
//     applies each at most once no matter how often settle runs.
//   - The winner is drawn and persisted *before* the payout, so a retry pays the
//     same person rather than re-rolling.
//   - The record is cleared only after the credit lands. A crash in between
//     replays a credit the refs absorb; the reverse order would lose the money.

// gambleJoinCoalesce groups joins that land close together into one chat line.
const gambleJoinCoalesce = 2 * time.Second

// gambleReminderLead is how long before the deadline the "closing soon" nudge
// posts (skipped when the round is too short for it to be useful).
const gambleReminderLead = 15 * time.Second

// gambleResultLinger is how long the overlay shows the winner/cancelled result
// before the panel is cleared from the overlay's cached state.
const gambleResultLinger = 8 * time.Second

// gambleStaleAfter is how far past its deadline a restored round can be and still
// be drawn for a winner. Beyond it the bot was down long enough that chat has
// moved on, so an undrawn round refunds instead of surprising the channel with a
// payout from a round nobody remembers. A round whose winner was already drawn
// pays out regardless of age — those marks are owed.
const gambleStaleAfter = 5 * time.Minute

// gambleSettingKey is the settings row holding the open round (see the file
// header). Empty or absent means no round is in flight.
const gambleSettingKey = "gamble_round"

// gamblePanelData is the overlay render state for the gamble panel. Phase is
// "open" (round accepting joins, with a countdown to EndsAt), "result" (winner or
// cancelled flash), or "hidden" (no active round; clears the panel).
type gamblePanelData struct {
	Phase     string `json:"phase"`
	BuyIn     int64  `json:"buyIn,omitempty"`
	Players   int    `json:"players,omitempty"`
	Pot       int64  `json:"pot,omitempty"`
	EndsAt    int64  `json:"endsAt,omitempty"` // unix millis when the round closes
	Winner    string `json:"winner,omitempty"`
	Cancelled bool   `json:"cancelled,omitempty"`
}

// pushGamble sends the gamble panel state to the overlay (no-op when the overlay
// isn't configured). Safe to call while holding gambleMu — the push is queued,
// not sent inline.
func (r *Router) pushGamble(g *gambleRound, phase, winner string, cancelled bool) {
	if r.overlay == nil {
		return
	}
	d := gamblePanelData{
		Phase:     phase,
		BuyIn:     g.buyIn,
		Players:   len(g.entrants),
		Pot:       g.pot(),
		Winner:    winner,
		Cancelled: cancelled,
	}
	if phase == "open" && !g.endsAt.IsZero() {
		d.EndsAt = g.endsAt.UnixMilli()
	}
	r.overlay.Push("gamble", d)
}

// pushGambleHidden clears the gamble panel from the overlay.
func (r *Router) pushGambleHidden() {
	if r.overlay != nil {
		r.overlay.Push("gamble", gamblePanelData{Phase: "hidden"})
	}
}

type entrant struct {
	userID  string
	login   string
	display string
}

type gambleRound struct {
	id       string // stable round id, minted at open; namespaces the ledger refs
	roomID   string
	buyIn    int64
	endsAt   time.Time // deadline (for the overlay countdown)
	entrants []entrant
	winner   int             // index into entrants once drawn; -1 until then
	joined   map[string]bool // userID set (dup-join guard)
	pending  []string        // display names buffered for the next coalesced join line
	flushing bool            // a coalesce-flush timer is scheduled
}

func (g *gambleRound) pot() int64 { return g.buyIn * int64(len(g.entrants)) }

// refundRef / winRef are the ledger idempotency keys for this round's credits.
// They're stable across restarts (id is persisted), which is what lets a settle
// run twice without paying twice.
func (g *gambleRound) refundRef(userID string) string { return "gamble:" + g.id + ":refund:" + userID }
func (g *gambleRound) winRef(userID string) string    { return "gamble:" + g.id + ":win:" + userID }

// currencyName is the configured marks name, with a fallback for the restore
// path: a round can legitimately settle on a boot where the economy failed to
// enable (the escrow in the ledger is real regardless of this process's config).
func (r *Router) currencyName() string {
	if r.econ.CurrencyName != "" {
		return r.econ.CurrencyName
	}
	return "marks"
}

// openOrJoinGamble handles "!g [amount]": opens a round if none is active, else
// joins the running one. Chat sends happen after the lock is released.
func (r *Router) openOrJoinGamble(rest string, m ChatMessage) {
	if r.chat == nil {
		r.logger.Printf("!g: replies not configured — run 'mise run bot:auth'")
		return
	}
	r.gambleMu.Lock()
	var announce, reply string
	if r.round == nil {
		announce, reply = r.openGambleLocked(rest, m)
	} else {
		reply = r.joinGambleLocked(m)
	}
	r.gambleMu.Unlock()

	if reply != "" {
		r.reply(m, reply)
	}
	if announce != "" {
		r.chat.Send(m.RoomID, announce)
	}
}

// openGambleLocked validates the buy-in, escrows the opener, starts the round,
// and schedules its timers. Returns (channel announcement, threaded reply); one
// is "" depending on success. Caller holds gambleMu.
func (r *Router) openGambleLocked(rest string, m ChatMessage) (announce, reply string) {
	arg := strings.TrimSpace(rest)
	if arg == "" {
		return "", "open a gamble with !g <amount> (or !g all)."
	}
	bal, err := r.store.Balance(m.UserID)
	if err != nil {
		r.logger.Printf("gamble balance %s: %v", m.User, err)
		return "", ""
	}

	var buyIn int64
	if strings.EqualFold(arg, "all") {
		buyIn = bal
	} else {
		n, perr := strconv.ParseInt(arg, 10, 64)
		if perr != nil || n <= 0 {
			return "", "usage: !g <amount> (a number or 'all')."
		}
		buyIn = n
	}
	if buyIn < r.econ.GambleMinBet {
		return "", fmt.Sprintf("@%s minimum buy-in is %s %s.", displayName(m), comma(r.econ.GambleMinBet), r.currencyName())
	}
	if buyIn > bal {
		return "", fmt.Sprintf("@%s you only have %s %s.", displayName(m), comma(bal), r.currencyName())
	}
	if ok, err := r.store.Spend(m.UserID, buyIn, "gamble_bet"); err != nil {
		r.logger.Printf("gamble open escrow %s: %v", m.User, err)
		return "", ""
	} else if !ok {
		return "", fmt.Sprintf("@%s you only have %s %s.", displayName(m), comma(bal), r.currencyName())
	}

	dur := r.econ.GambleDuration
	round := &gambleRound{
		id:       strconv.FormatInt(time.Now().UnixNano(), 10),
		roomID:   m.RoomID,
		buyIn:    buyIn,
		endsAt:   time.Now().Add(dur),
		entrants: []entrant{{m.UserID, m.User, displayName(m)}},
		winner:   -1,
		joined:   map[string]bool{m.UserID: true},
	}
	r.round = round
	r.persistGamble(round) // before the timer: a crash in between must still settle

	time.AfterFunc(dur, func() { r.resolveGamble(round) })
	if dur > gambleReminderLead+5*time.Second {
		time.AfterFunc(dur-gambleReminderLead, func() { r.remindGamble(round) })
	}
	r.pushGamble(round, "open", "", false)

	return fmt.Sprintf("%s started a gamble! Buy-in %s %s — type !g in the next %ds to join.",
		displayName(m), comma(buyIn), r.currencyName(), int(dur.Seconds())), ""
}

// joinGambleLocked enters m into the active round (escrowing the buy-in) and
// buffers the join for the next coalesced announcement. Returns a threaded reply
// only on a problem (already in / can't afford). Caller holds gambleMu.
func (r *Router) joinGambleLocked(m ChatMessage) (reply string) {
	rd := r.round
	if rd.joined[m.UserID] {
		return fmt.Sprintf("@%s you're already in (pot %s %s).", displayName(m), comma(rd.pot()), r.currencyName())
	}
	bal, err := r.store.Balance(m.UserID)
	if err != nil {
		r.logger.Printf("gamble balance %s: %v", m.User, err)
		return ""
	}
	if bal < rd.buyIn {
		return fmt.Sprintf("@%s the buy-in is %s %s — you have %s.", displayName(m), comma(rd.buyIn), r.currencyName(), comma(bal))
	}
	if ok, err := r.store.Spend(m.UserID, rd.buyIn, "gamble_bet"); err != nil {
		r.logger.Printf("gamble join escrow %s: %v", m.User, err)
		return ""
	} else if !ok {
		return fmt.Sprintf("@%s the buy-in is %s %s — you have %s.", displayName(m), comma(rd.buyIn), r.currencyName(), comma(bal))
	}

	rd.joined[m.UserID] = true
	rd.entrants = append(rd.entrants, entrant{m.UserID, m.User, displayName(m)})
	rd.pending = append(rd.pending, displayName(m))
	r.persistGamble(rd) // after the Spend: the record only lists escrowed entrants
	if !rd.flushing {
		rd.flushing = true
		time.AfterFunc(gambleJoinCoalesce, func() { r.flushJoins(rd) })
	}
	r.pushGamble(rd, "open", "", false)
	return ""
}

// flushJoins posts one line naming everyone who joined since the last flush.
func (r *Router) flushJoins(round *gambleRound) {
	r.gambleMu.Lock()
	if r.round != round || len(round.pending) == 0 {
		round.flushing = false
		r.gambleMu.Unlock()
		return
	}
	names := round.pending
	round.pending = nil
	round.flushing = false
	pot, count, roomID := round.pot(), len(round.entrants), round.roomID
	r.gambleMu.Unlock()

	ats := make([]string, len(names))
	for i, n := range names {
		ats[i] = "@" + n
	}
	r.sendGamble(roomID, fmt.Sprintf("%s joined — pot %s %s (%d players).",
		strings.Join(ats, " "), comma(pot), r.currencyName(), count))
}

// --- persistence ------------------------------------------------------------

// gambleRec is the persisted round (settings row). It carries the entrants and
// the drawn winner, so a restored round can settle without the original timer;
// pending/flushing are transient chat coalescing and deliberately left out.
type gambleRec struct {
	ID       string             `json:"id"`
	RoomID   string             `json:"roomID"`
	BuyIn    int64              `json:"buyIn"`
	EndsAt   int64              `json:"endsAt"` // unix millis
	Entrants []gambleEntrantRec `json:"entrants"`
	Winner   int                `json:"winner"` // index into Entrants; -1 = not drawn yet
}

type gambleEntrantRec struct {
	UserID  string `json:"userID"`
	Login   string `json:"login"`
	Display string `json:"display"`
}

// persistGamble mirrors the round to the store. Caller holds gambleMu.
func (r *Router) persistGamble(g *gambleRound) {
	if r.store == nil {
		return
	}
	rec := gambleRec{ID: g.id, RoomID: g.roomID, BuyIn: g.buyIn, Winner: g.winner}
	if !g.endsAt.IsZero() {
		rec.EndsAt = g.endsAt.UnixMilli()
	}
	rec.Entrants = make([]gambleEntrantRec, len(g.entrants))
	for i, e := range g.entrants {
		rec.Entrants[i] = gambleEntrantRec{UserID: e.userID, Login: e.login, Display: e.display}
	}
	b, err := json.Marshal(rec)
	if err != nil {
		r.logger.Printf("gamble persist marshal: %v", err)
		return
	}
	if err := r.store.SetSetting(gambleSettingKey, string(b)); err != nil {
		r.logger.Printf("gamble persist: %v", err)
	}
}

// clearGamblePersist drops the persisted round once it has settled — but only
// when no round is live. Settling runs with gambleMu released, so a new !g can
// open (and persist itself) mid-settle; wiping blindly would strand that round
// exactly the way this whole mechanism exists to prevent.
func (r *Router) clearGamblePersist() {
	if r.store == nil {
		return
	}
	r.gambleMu.Lock()
	defer r.gambleMu.Unlock()
	if r.round != nil {
		return
	}
	if err := r.store.SetSetting(gambleSettingKey, ""); err != nil {
		r.logger.Printf("gamble clear persist: %v", err)
	}
}

// loadGamble restores a round interrupted by a restart and settles or resumes it.
// Without this the round's only timer dies with the process and every escrowed
// buy-in is stranded. Called once at startup, before the IRC loop.
func (r *Router) loadGamble() {
	if r.store == nil {
		return
	}
	v, ok, err := r.store.GetSetting(gambleSettingKey)
	if err != nil {
		r.logger.Printf("gamble load: %v", err)
		return
	}
	if !ok || v == "" {
		return
	}
	var rec gambleRec
	if err := json.Unmarshal([]byte(v), &rec); err != nil {
		r.logger.Printf("gamble load unmarshal: %v", err)
		r.clearGamblePersist()
		return
	}
	if len(rec.Entrants) == 0 || rec.BuyIn <= 0 {
		r.clearGamblePersist()
		return
	}

	round := &gambleRound{
		id: rec.ID, roomID: rec.RoomID, buyIn: rec.BuyIn, winner: rec.Winner,
		entrants: make([]entrant, len(rec.Entrants)),
		joined:   make(map[string]bool, len(rec.Entrants)),
	}
	if rec.EndsAt > 0 {
		round.endsAt = time.UnixMilli(rec.EndsAt)
	}
	if round.winner >= len(rec.Entrants) {
		round.winner = -1 // corrupt index; re-draw rather than panic
	}
	for i, e := range rec.Entrants {
		round.entrants[i] = entrant{e.UserID, e.Login, e.Display}
		round.joined[e.UserID] = true
	}

	r.gambleMu.Lock()
	if r.round != nil { // nothing else runs this early, but don't clobber a live round
		r.gambleMu.Unlock()
		return
	}
	r.round = round
	r.gambleMu.Unlock()

	remaining := time.Until(round.endsAt)
	if remaining > 0 && round.winner < 0 {
		// Still open: pick the timers back up for whatever time is left.
		r.logger.Printf("gamble: restored open round (%d in, pot %s, %s left)",
			len(round.entrants), comma(round.pot()), shortDuration(remaining))
		r.pushGamble(round, "open", "", false)
		time.AfterFunc(remaining, func() { r.resolveGamble(round) })
		if remaining > gambleReminderLead+5*time.Second {
			time.AfterFunc(remaining-gambleReminderLead, func() { r.remindGamble(round) })
		}
		return
	}

	// Deadline already passed. Settle inline — main hasn't started the IRC loop
	// yet, so there's nobody to race, and a goroutine here would race main's
	// remaining startup writes to the Router.
	stale := round.winner < 0 && time.Since(round.endsAt) > gambleStaleAfter
	r.logger.Printf("gamble: settling round interrupted by a restart (%d in, pot %s, stale=%v)",
		len(round.entrants), comma(round.pot()), stale)
	r.gambleMu.Lock()
	r.round = nil
	r.gambleMu.Unlock()
	r.settleGamble(round, stale)
}

// remindGamble posts a "closing soon" nudge with the current pot.
func (r *Router) remindGamble(round *gambleRound) {
	r.gambleMu.Lock()
	if r.round != round {
		r.gambleMu.Unlock()
		return
	}
	pot, count, buyIn, roomID := round.pot(), len(round.entrants), round.buyIn, round.roomID
	r.gambleMu.Unlock()

	r.sendGamble(roomID, fmt.Sprintf("Gamble closing soon — %d in, pot %s %s. Type !g (buy-in %s) to join!",
		count, comma(pot), r.currencyName(), comma(buyIn)))
}

// resolveGamble takes the round off the stage and settles it. It's the timer
// entry point: the r.round guard makes a late or duplicate timer a no-op, which
// is why the settling itself lives in settleGamble (that half must stay callable
// more than once — see loadGamble).
func (r *Router) resolveGamble(round *gambleRound) {
	r.gambleMu.Lock()
	if r.round != round {
		r.gambleMu.Unlock()
		return
	}
	r.round = nil
	r.gambleMu.Unlock()

	r.settleGamble(round, false)
}

// settleGamble pays out or refunds a round that's already off the stage. With <2
// players (or refundAll, for a round the bot slept through) everyone gets their
// buy-in back; otherwise a uniform-random winner takes the whole pot.
//
// Safe to run more than once on the same round: the ledger refs make both credits
// idempotent, and the persisted record is cleared only once the money has moved.
// Holds no lock — chat and DB work happen here.
func (r *Router) settleGamble(round *gambleRound, refundAll bool) {
	entrants, buyIn, roomID := round.entrants, round.buyIn, round.roomID

	if len(entrants) < 2 || (refundAll && round.winner < 0) {
		for _, e := range entrants {
			if _, err := r.store.Credit(e.userID, buyIn, "gamble_refund", round.refundRef(e.userID)); err != nil {
				// Left persisted so the next boot retries this same ref.
				r.logger.Printf("gamble refund %s: %v", e.login, err)
				return
			}
		}
		msg := "Gamble cancelled — need 2+ players."
		switch {
		case refundAll:
			msg = fmt.Sprintf("Gamble cancelled — the bot restarted mid-round. %s refunded.", plural(len(entrants), "player"))
		case len(entrants) == 1:
			msg = fmt.Sprintf("Gamble cancelled — nobody else joined. @%s refunded.", entrants[0].display)
		}
		r.pushGamble(round, "result", "", true)
		r.scheduleGambleHide()
		r.sendGamble(roomID, msg)
		r.clearGamblePersist()
		return
	}

	// Draw once and write it down before paying: a retry after a failed payout
	// must pay the same person, or it would credit a second ref.
	if round.winner < 0 {
		r.gambleMu.Lock()
		round.winner = r.randIntn(len(entrants))
		r.persistGamble(round)
		r.gambleMu.Unlock()
	}
	winner := entrants[round.winner]
	pot := buyIn * int64(len(entrants))

	credited, err := r.store.Credit(winner.userID, pot, "gamble_win", round.winRef(winner.userID))
	if err != nil {
		// Keep the record: the next boot retries this exact ref. Still show and
		// announce the result rather than leaving the overlay stuck on "open".
		r.logger.Printf("gamble payout %s: %v", winner.login, err)
		r.pushGamble(round, "result", winner.display, false)
		r.scheduleGambleHide()
		r.sendGamble(roomID, fmt.Sprintf("🎉 @%s wins the pot of %s %s! (payout is retrying — it'll land shortly)",
			winner.display, comma(pot), r.currencyName()))
		return
	}
	r.pushGamble(round, "result", winner.display, false)
	r.scheduleGambleHide()
	if credited { // a repeat settle already announced this winner
		r.sendGamble(roomID, fmt.Sprintf("🎉 @%s wins the pot of %s %s! (%d players)",
			winner.display, comma(pot), r.currencyName(), len(entrants)))
	}
	r.clearGamblePersist()
}

// sendGamble posts to chat when the bot is authenticated. The settle path can run
// at startup on an unauthenticated boot (restoring a round to refund it), where
// r.chat is nil and the money still has to move.
func (r *Router) sendGamble(roomID, msg string) {
	if r.chat != nil {
		r.chat.Send(roomID, msg)
	}
}

// scheduleGambleHide clears the result panel after a linger — unless a new round
// opened in the meantime, whose live panel must not be wiped. No round argument
// (unlike scheduleWordleClear): resolve nils r.round, so "something newer exists"
// is exactly r.round != nil.
func (r *Router) scheduleGambleHide() {
	time.AfterFunc(gambleResultLinger, r.hideGambleIfIdle)
}

func (r *Router) hideGambleIfIdle() {
	r.gambleMu.Lock()
	live := r.round != nil
	r.gambleMu.Unlock()
	if !live {
		r.pushGambleHidden()
	}
}
