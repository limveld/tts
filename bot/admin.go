package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"tts/twitch"
)

// twitchResolver adapts *twitch.Client to UserResolver (via Get Users).
type twitchResolver struct{ client *twitch.Client }

func (t twitchResolver) LookupUser(ctx context.Context, login string) (userID, resolvedLogin, display string, ok bool, err error) {
	users, err := t.client.GetUsers(ctx, login)
	if err != nil {
		return "", "", "", false, err
	}
	if len(users) == 0 {
		return "", "", "", false, nil
	}
	u := users[0]
	return u.ID, u.Login, u.Display, true, nil
}

// This file holds the broadcaster-only economy controls: !grant (mint/claw-back
// marks to anyone) and !free / !paid (toggle whether !tts/!sfx charge). Both are
// dispatched from handleCommands only when the economy is enabled.

// UserResolver resolves a Twitch login the bot hasn't seen in chat to an account
// (via Helix Get Users), so !grant can target anyone. ok is false when no such
// login exists. Backed by *twitch.Client in main; faked in tests.
type UserResolver interface {
	LookupUser(ctx context.Context, login string) (userID, resolvedLogin, display string, ok bool, err error)
}

// grant mints (positive) or claws back (negative, clamped at 0) marks for a
// target user. Broadcaster only.
func (r *Router) grant(rest string, m ChatMessage) {
	if !m.IsBroadcaster {
		return
	}
	fields := strings.Fields(rest)
	if len(fields) < 2 {
		r.reply(m, "usage: !grant @user <amount> (negative removes)")
		return
	}
	login := strings.ToLower(strings.TrimPrefix(fields[0], "@"))
	amount, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil || amount == 0 || login == "" {
		r.reply(m, "usage: !grant @user <amount> (non-zero; negative removes)")
		return
	}

	userID, display, ok := r.resolveTarget(login)
	if !ok {
		r.reply(m, "no such user @"+login+".")
		return
	}
	// Record the identity so a freshly-granted (previously unseen) user shows up
	// on the leaderboard right away.
	if err := r.store.UpsertUser(userID, login, display); err != nil {
		r.logger.Printf("grant upsert %q: %v", login, err)
		return
	}

	newBal, err := r.store.Grant(userID, amount, "grant")
	if err != nil {
		r.logger.Printf("grant %q: %v", login, err)
		return
	}
	verb := "granted"
	if amount < 0 {
		verb = "removed"
	}
	r.reply(m, fmt.Sprintf("@%s now has %s %s (%s %s).",
		login, comma(newBal), r.econ.CurrencyName, verb, comma(abs64(amount))))
}

// resolveTarget finds a user_id + display for a login: the local users table
// first (no API), then Helix. ok is false when the login doesn't exist.
func (r *Router) resolveTarget(login string) (userID, display string, ok bool) {
	if id, found, err := r.store.ResolveLogin(login); err != nil {
		r.logger.Printf("grant resolve %q: %v", login, err)
		return "", "", false
	} else if found {
		return id, login, true
	}
	if r.resolver == nil {
		return "", "", false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	id, rlogin, disp, found, err := r.resolver.LookupUser(ctx, login)
	if err != nil {
		r.logger.Printf("grant lookup %q: %v", login, err)
		return "", "", false
	}
	if !found {
		return "", "", false
	}
	if disp == "" {
		disp = rlogin
	}
	return id, disp, true
}

// setCharging flips the free/paid charge mode (broadcaster only), persists it,
// and confirms in chat. free=true means !tts/!sfx cost nothing.
func (r *Router) setCharging(free bool, m ChatMessage) {
	if !m.IsBroadcaster {
		return
	}
	r.charging = !free
	mode := "paid"
	if free {
		mode = "free"
	}
	if err := r.store.SetSetting("charge_mode", mode); err != nil {
		r.logger.Printf("charge_mode persist: %v", err)
	}
	if free {
		r.reply(m, fmt.Sprintf("Marks are now FREE — !tts/!sfx cost nothing (%s still accrue).", r.econ.CurrencyName))
	} else {
		r.reply(m, fmt.Sprintf("Marks are back ON — !tts costs %s, !sfx %s.",
			comma(r.econ.TTSCost), comma(r.econ.SFXCost)))
	}
}

func abs64(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}
