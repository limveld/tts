package main

import (
	"fmt"
	"strconv"
	"strings"
)

// This file holds !give (a peer transfer). The multiplayer !g gamble lives in
// gamble.go. Both are economy built-ins — dispatched from handleCommands only
// when the economy is enabled. Every balance mutation goes through the store's
// atomic helpers, so nobody can overdraw.

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
