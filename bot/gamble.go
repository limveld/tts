package main

import (
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

// gambleJoinCoalesce groups joins that land close together into one chat line.
const gambleJoinCoalesce = 2 * time.Second

// gambleReminderLead is how long before the deadline the "closing soon" nudge
// posts (skipped when the round is too short for it to be useful).
const gambleReminderLead = 15 * time.Second

type entrant struct {
	userID  string
	login   string
	display string
}

type gambleRound struct {
	roomID   string
	buyIn    int64
	entrants []entrant
	joined   map[string]bool // userID set (dup-join guard)
	pending  []string        // display names buffered for the next coalesced join line
	flushing bool            // a coalesce-flush timer is scheduled
}

func (g *gambleRound) pot() int64 { return g.buyIn * int64(len(g.entrants)) }

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
		return "", fmt.Sprintf("@%s minimum buy-in is %s %s.", displayName(m), comma(r.econ.GambleMinBet), r.econ.CurrencyName)
	}
	if buyIn > bal {
		return "", fmt.Sprintf("@%s you only have %s %s.", displayName(m), comma(bal), r.econ.CurrencyName)
	}
	if ok, err := r.store.Spend(m.UserID, buyIn, "gamble_bet"); err != nil {
		r.logger.Printf("gamble open escrow %s: %v", m.User, err)
		return "", ""
	} else if !ok {
		return "", fmt.Sprintf("@%s you only have %s %s.", displayName(m), comma(bal), r.econ.CurrencyName)
	}

	round := &gambleRound{
		roomID:   m.RoomID,
		buyIn:    buyIn,
		entrants: []entrant{{m.UserID, m.User, displayName(m)}},
		joined:   map[string]bool{m.UserID: true},
	}
	r.round = round

	dur := r.econ.GambleDuration
	time.AfterFunc(dur, func() { r.resolveGamble(round) })
	if dur > gambleReminderLead+5*time.Second {
		time.AfterFunc(dur-gambleReminderLead, func() { r.remindGamble(round) })
	}

	return fmt.Sprintf("%s started a gamble! Buy-in %s %s — type !g in the next %ds to join.",
		displayName(m), comma(buyIn), r.econ.CurrencyName, int(dur.Seconds())), ""
}

// joinGambleLocked enters m into the active round (escrowing the buy-in) and
// buffers the join for the next coalesced announcement. Returns a threaded reply
// only on a problem (already in / can't afford). Caller holds gambleMu.
func (r *Router) joinGambleLocked(m ChatMessage) (reply string) {
	rd := r.round
	if rd.joined[m.UserID] {
		return fmt.Sprintf("@%s you're already in (pot %s %s).", displayName(m), comma(rd.pot()), r.econ.CurrencyName)
	}
	bal, err := r.store.Balance(m.UserID)
	if err != nil {
		r.logger.Printf("gamble balance %s: %v", m.User, err)
		return ""
	}
	if bal < rd.buyIn {
		return fmt.Sprintf("@%s the buy-in is %s %s — you have %s.", displayName(m), comma(rd.buyIn), r.econ.CurrencyName, comma(bal))
	}
	if ok, err := r.store.Spend(m.UserID, rd.buyIn, "gamble_bet"); err != nil {
		r.logger.Printf("gamble join escrow %s: %v", m.User, err)
		return ""
	} else if !ok {
		return fmt.Sprintf("@%s the buy-in is %s %s — you have %s.", displayName(m), comma(rd.buyIn), r.econ.CurrencyName, comma(bal))
	}

	rd.joined[m.UserID] = true
	rd.entrants = append(rd.entrants, entrant{m.UserID, m.User, displayName(m)})
	rd.pending = append(rd.pending, displayName(m))
	if !rd.flushing {
		rd.flushing = true
		time.AfterFunc(gambleJoinCoalesce, func() { r.flushJoins(rd) })
	}
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
	r.chat.Send(roomID, fmt.Sprintf("%s joined — pot %s %s (%d players).",
		strings.Join(ats, " "), comma(pot), r.econ.CurrencyName, count))
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

	r.chat.Send(roomID, fmt.Sprintf("Gamble closing soon — %d in, pot %s %s. Type !g (buy-in %s) to join!",
		count, comma(pot), r.econ.CurrencyName, comma(buyIn)))
}

// resolveGamble ends the round: with <2 players it refunds everyone; otherwise a
// uniform-random winner takes the whole pot.
func (r *Router) resolveGamble(round *gambleRound) {
	r.gambleMu.Lock()
	if r.round != round {
		r.gambleMu.Unlock()
		return
	}
	r.round = nil
	entrants, buyIn, roomID := round.entrants, round.buyIn, round.roomID
	r.gambleMu.Unlock()

	if len(entrants) < 2 {
		for _, e := range entrants {
			if _, err := r.store.Credit(e.userID, buyIn, "gamble_refund", ""); err != nil {
				r.logger.Printf("gamble refund %s: %v", e.login, err)
			}
		}
		msg := "Gamble cancelled — need 2+ players."
		if len(entrants) == 1 {
			msg = fmt.Sprintf("Gamble cancelled — nobody else joined. @%s refunded.", entrants[0].display)
		}
		r.chat.Send(roomID, msg)
		return
	}

	winner := entrants[r.rnd.Intn(len(entrants))]
	pot := buyIn * int64(len(entrants))
	if _, err := r.store.Credit(winner.userID, pot, "gamble_win", ""); err != nil {
		r.logger.Printf("gamble payout %s: %v", winner.login, err)
		return
	}
	r.chat.Send(roomID, fmt.Sprintf("🎉 @%s wins the pot of %s %s! (%d players)",
		winner.display, comma(pot), r.econ.CurrencyName, len(entrants)))
}
