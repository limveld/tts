package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"tts/store"
)

// handleCommands dispatches the admin CRUD words, the dynamic !commands/!voices
// built-ins, and custom stored commands. It returns true if it handled cmd (so
// Handle stops), false to fall through to the TTS branch.
func (r *Router) handleCommands(cmd, rest string, m ChatMessage) bool {
	// Informational built-ins need only a Twitch client, not the economy store.
	if r.info != nil {
		switch cmd {
		case "!uptime":
			r.uptime(m)
			return true
		case "!followage":
			r.followage(rest, m)
			return true
		case "!so":
			r.soCommand(rest, m)
			return true
		}
	}
	if r.store == nil {
		return false
	}
	switch cmd {
	case "!don", "!r":
		r.setDepth(rest, m)
		return true
	case "!wordle":
		r.startWordle(m)
		return true
	case "!guess":
		r.guessWordle(rest, m)
		return true
	case "!wordlewins":
		r.showWordleWins(m)
		return true
	case "!addcom", "!editcom", "!delcom":
		if m.IsMod || m.IsBroadcaster {
			r.adminCommand(cmd, rest, m)
		}
		return true
	case "!commands":
		r.listCommands(m)
		return true
	case "!voices":
		r.listVoices(m)
		return true
	case "!marks", "!m":
		if r.economy {
			r.showMarks(rest, m)
			return true
		}
	case "!leaderboard":
		if r.economy {
			r.showLeaderboard(m)
			return true
		}
	case "!g", "!gamble":
		if r.economy {
			r.openOrJoinGamble(rest, m)
			return true
		}
	case "!give":
		if r.economy {
			r.give(rest, m)
			return true
		}
	case "!grant":
		if r.economy {
			r.grant(rest, m)
			return true
		}
	case "!free":
		if r.economy {
			r.setCharging(true, m)
			return true
		}
	case "!paid":
		if r.economy {
			r.setCharging(false, m)
			return true
		}
	}

	// Custom command: exact match on the stored name (without the leading "!").
	name := strings.TrimPrefix(cmd, "!")
	c, ok, err := r.store.Get(name)
	if err != nil {
		r.logger.Printf("command lookup %q: %v", name, err)
		return false
	}
	if !ok {
		return false
	}
	r.runCommand(c, rest, m)
	return true
}

// adminCommand handles !addcom/!editcom/!delcom (mod/broadcaster only). rest is
// "!name <response>" for add/edit, "!name" for del.
func (r *Router) adminCommand(cmd, rest string, m ChatMessage) {
	head, tail, _ := strings.Cut(strings.TrimSpace(rest), " ")
	name := strings.ToLower(strings.TrimPrefix(head, "!"))
	tail = strings.TrimSpace(tail)
	if name == "" {
		r.reply(m, "usage: "+cmd+" !name "+map[string]string{"!delcom": ""}[cmd]+"<response>")
		return
	}
	if r.isBuiltin("!" + name) {
		r.reply(m, "!"+name+" is a built-in command")
		return
	}

	switch cmd {
	case "!addcom":
		if tail == "" {
			r.reply(m, "usage: !addcom !name <response>")
			return
		}
		created, err := r.store.Add(store.Command{Name: name, Response: tail, MinRole: "everyone"})
		if err != nil {
			r.logger.Printf("addcom %q: %v", name, err)
			return
		}
		if !created {
			r.reply(m, "!"+name+" already exists — use !editcom")
			return
		}
		r.reply(m, "added !"+name)
	case "!editcom":
		if tail == "" {
			r.reply(m, "usage: !editcom !name <response>")
			return
		}
		found, err := r.store.SetResponse(name, tail)
		if err != nil {
			r.logger.Printf("editcom %q: %v", name, err)
			return
		}
		if !found {
			r.reply(m, "!"+name+" doesn't exist")
			return
		}
		r.reply(m, "updated !"+name)
	case "!delcom":
		found, err := r.store.Delete(name)
		if err != nil {
			r.logger.Printf("delcom %q: %v", name, err)
			return
		}
		if !found {
			r.reply(m, "!"+name+" doesn't exist")
			return
		}
		r.reply(m, "deleted !"+name)
	}
}

// runCommand executes a custom command: role gate, per-command cooldown (mods
// exempt), variable substitution, threaded reply, and a use-count bump.
func (r *Router) runCommand(c store.Command, rest string, m ChatMessage) {
	if !r.roleAllows(c.MinRole, m) {
		return
	}
	if !(m.IsMod || m.IsBroadcaster) && !r.commandCooldownAllow(c.Name, time.Duration(c.Cooldown)*time.Second) {
		r.logger.Printf("cooldown: ignoring !%s from %s", c.Name, m.User)
		return
	}
	user := m.Display
	if user == "" {
		user = m.User
	}
	r.reply(m, substitute(c.Response, subCtx{
		User:  user,
		Args:  strings.Fields(rest),
		Rest:  rest,
		Count: c.Count + 1,
		rnd:   r.rnd,
	}))
	if err := r.store.IncCount(c.Name); err != nil {
		r.logger.Printf("inc count %q: %v", c.Name, err)
	}
}

// listCommands replies with the public built-in commands followed by the custom
// stored commands (dynamic !commands). Mod/broadcaster-only controls are omitted.
func (r *Router) listCommands(m ChatMessage) {
	if !(m.IsMod || m.IsBroadcaster) && !r.cooldown.Allow(m.User) {
		return
	}
	// Public built-ins, gated on the same flags used to dispatch them.
	cmds := []string{r.cmds.TTSPrefix, r.cmds.SFX, "!voices", "!wordle", "!guess", "!wordlewins"}
	if r.economy {
		cmds = append(cmds, "!marks", "!leaderboard", "!g", "!give")
	}
	if r.info != nil {
		cmds = append(cmds, "!uptime", "!followage")
	}

	names, err := r.store.List()
	if err != nil {
		r.logger.Printf("list commands: %v", err)
		return
	}
	for _, n := range names {
		cmds = append(cmds, "!"+n)
	}
	r.reply(m, "Commands: "+strings.Join(cmds, ", "))
}

// listVoices replies with the server's code→voice map (dynamic !voices).
func (r *Router) listVoices(m ChatMessage) {
	if !(m.IsMod || m.IsBroadcaster) && !r.cooldown.Allow(m.User) {
		return
	}
	voices, err := r.tts.Voices()
	if err != nil {
		r.logger.Printf("voices: %v", err)
		r.reply(m, "voices unavailable right now")
		return
	}
	if len(voices) == 0 {
		r.reply(m, "No voices configured.")
		return
	}
	parts := make([]string, len(voices))
	for i, v := range voices {
		parts[i] = v.Code + "=" + v.Voice
	}
	r.reply(m, "Voices (!tts<code>): "+strings.Join(parts, ", "))
}

// reply threads a message back to the caller (no-op with a log when chat replies
// aren't configured).
func (r *Router) reply(m ChatMessage, text string) {
	if r.chat == nil {
		r.logger.Printf("reply skipped (no chat): %s", text)
		return
	}
	if err := r.chat.Reply(m.RoomID, m.ID, text); err != nil {
		r.logger.Printf("reply error: %v", err)
	}
}

// isBuiltin reports whether cmd is a reserved built-in (so !addcom can't shadow it).
func (r *Router) isBuiltin(cmd string) bool {
	switch cmd {
	case r.cmds.SFX, r.cmds.Skip, r.cmds.Pause, r.cmds.Resume, r.cmds.Clear,
		"!addcom", "!editcom", "!delcom", "!commands", "!voices", "!don", "!r", "!so",
		"!wordle", "!guess", "!wordlewins":
		return true
	}
	if r.info != nil && (cmd == "!uptime" || cmd == "!followage") {
		return true
	}
	if r.economy {
		switch cmd {
		case "!marks", "!m", "!leaderboard", "!g", "!gamble", "!give", "!grant", "!free", "!paid":
			return true
		}
	}
	if _, ok := r.sfx[cmd]; ok {
		return true
	}
	return strings.HasPrefix(cmd, r.cmds.TTSPrefix)
}

// showMarks replies with a user's mark balance: bare !marks/!m for the caller,
// or "!marks @name" for someone we've seen.
func (r *Router) showMarks(rest string, m ChatMessage) {
	if !(m.IsMod || m.IsBroadcaster) && !r.cooldown.Allow(m.User) {
		return
	}
	login := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(rest), "@"))
	if login != "" && login != strings.ToLower(m.User) {
		id, ok, err := r.store.ResolveLogin(login)
		if err != nil {
			r.logger.Printf("marks resolve %q: %v", login, err)
			return
		}
		if !ok {
			r.reply(m, "haven't seen @"+login+" yet.")
			return
		}
		bal, err := r.store.Balance(id)
		if err != nil {
			r.logger.Printf("marks balance %q: %v", login, err)
			return
		}
		r.reply(m, fmt.Sprintf("@%s has %s %s.", login, comma(bal), r.econ.CurrencyName))
		return
	}
	bal, err := r.store.Balance(m.UserID)
	if err != nil {
		r.logger.Printf("marks balance %s: %v", m.User, err)
		return
	}
	r.reply(m, fmt.Sprintf("@%s you have %s %s.", displayName(m), comma(bal), r.econ.CurrencyName))
}

// showLeaderboard replies with the top mark holders.
func (r *Router) showLeaderboard(m ChatMessage) {
	if !(m.IsMod || m.IsBroadcaster) && !r.cooldown.Allow(m.User) {
		return
	}
	lb, err := r.store.Leaderboard(10)
	if err != nil {
		r.logger.Printf("leaderboard: %v", err)
		return
	}
	if len(lb) == 0 {
		r.reply(m, "No "+r.econ.CurrencyName+" yet.")
		return
	}
	parts := make([]string, len(lb))
	for i, e := range lb {
		parts[i] = fmt.Sprintf("%d. %s %s", i+1, e.Display, comma(e.Balance))
	}
	r.reply(m, "Top "+r.econ.CurrencyName+": "+strings.Join(parts, "  "))
}

// displayName returns the best available name for m (display tag, else login).
func displayName(m ChatMessage) string {
	if m.Display != "" {
		return m.Display
	}
	return m.User
}

// comma formats n with thousands separators (e.g. 1024 -> "1,024").
func comma(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	var b strings.Builder
	for i, d := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(d)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

// commandCooldownAllow enforces a per-command global cooldown (shared across
// users). A zero window always allows.
func (r *Router) commandCooldownAllow(name string, window time.Duration) bool {
	if window <= 0 {
		return true
	}
	r.cdMu.Lock()
	defer r.cdMu.Unlock()
	if r.cmdCooldown == nil {
		r.cmdCooldown = make(map[string]time.Time)
	}
	now := time.Now()
	if last, ok := r.cmdCooldown[name]; ok && now.Sub(last) < window {
		return false
	}
	r.cmdCooldown[name] = now
	return true
}
