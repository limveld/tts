package main

import (
	"fmt"
	"strconv"
	"strings"
)

// This file holds the marks "games": !gamble (a coinflip double-or-nothing) and
// !give (a peer transfer). Both are economy built-ins — dispatched from
// handleCommands only when the economy is enabled — and share the standard
// per-user cooldown (mods/broadcaster exempt). Every balance mutation goes
// through the store's atomic helpers, so nobody can overdraw.

// gamble bets amount (or "all") marks on a coinflip: a win (probability
// GambleWinChance) nets +amount, a loss forfeits it.
func (r *Router) gamble(rest string, m ChatMessage) {
	if !(m.IsMod || m.IsBroadcaster) && !r.cooldown.Allow(m.User) {
		return
	}
	arg := strings.TrimSpace(rest)
	if arg == "" {
		r.reply(m, "usage: !gamble <amount|all>")
		return
	}
	bal, err := r.store.Balance(m.UserID)
	if err != nil {
		r.logger.Printf("gamble balance %s: %v", m.User, err)
		return
	}

	var bet int64
	if strings.EqualFold(arg, "all") {
		bet = bal
	} else {
		n, perr := strconv.ParseInt(arg, 10, 64)
		if perr != nil || n <= 0 {
			r.reply(m, "usage: !gamble <amount|all>")
			return
		}
		bet = n
	}

	if bet < r.econ.GambleMinBet {
		r.reply(m, fmt.Sprintf("@%s minimum bet is %s %s.", displayName(m), comma(r.econ.GambleMinBet), r.econ.CurrencyName))
		return
	}
	if bet > bal {
		r.reply(m, fmt.Sprintf("@%s you only have %s %s.", displayName(m), comma(bal), r.econ.CurrencyName))
		return
	}

	win := r.rnd.Float64() < r.econ.GambleWinChance
	if win {
		if _, err := r.store.Credit(m.UserID, bet, "gamble", ""); err != nil {
			r.logger.Printf("gamble credit %s: %v", m.User, err)
			return
		}
	} else if _, err := r.store.Spend(m.UserID, bet, "gamble"); err != nil {
		r.logger.Printf("gamble debit %s: %v", m.User, err)
		return
	}

	newBal, _ := r.store.Balance(m.UserID)
	if win {
		r.reply(m, fmt.Sprintf("@%s won %s %s! You now have %s.", displayName(m), comma(bet), r.econ.CurrencyName, comma(newBal)))
	} else {
		r.reply(m, fmt.Sprintf("@%s lost %s %s. You now have %s.", displayName(m), comma(bet), r.econ.CurrencyName, comma(newBal)))
	}
}

// give transfers amount marks from the caller to another user we've seen.
func (r *Router) give(rest string, m ChatMessage) {
	if !(m.IsMod || m.IsBroadcaster) && !r.cooldown.Allow(m.User) {
		return
	}
	fields := strings.Fields(rest)
	if len(fields) < 2 {
		r.reply(m, "usage: !give @user <amount>")
		return
	}
	login := strings.ToLower(strings.TrimPrefix(fields[0], "@"))
	amount, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil || amount <= 0 {
		r.reply(m, "usage: !give @user <amount>")
		return
	}
	if login == "" || login == strings.ToLower(m.User) {
		r.reply(m, "@"+displayName(m)+" you can't give to yourself.")
		return
	}

	toID, ok, err := r.store.ResolveLogin(login)
	if err != nil {
		r.logger.Printf("give resolve %q: %v", login, err)
		return
	}
	if !ok {
		r.reply(m, "haven't seen @"+login+" yet.")
		return
	}

	ok, err = r.store.Transfer(m.UserID, toID, amount, "give")
	if err != nil {
		r.logger.Printf("give transfer %s->%s: %v", m.User, login, err)
		return
	}
	if !ok {
		bal, _ := r.store.Balance(m.UserID)
		r.reply(m, fmt.Sprintf("@%s you only have %s %s.", displayName(m), comma(bal), r.econ.CurrencyName))
		return
	}
	r.reply(m, fmt.Sprintf("@%s gave %s %s to @%s.", displayName(m), comma(amount), r.econ.CurrencyName, login))
}
