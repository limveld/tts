package main

import (
	"strings"
	"time"

	"tts/store"
)

// handleCommands dispatches the admin CRUD words, the dynamic !commands/!voices
// built-ins, and custom stored commands. It returns true if it handled cmd (so
// Handle stops), false to fall through to the TTS branch.
func (r *Router) handleCommands(cmd, rest string, m ChatMessage) bool {
	if r.store == nil {
		return false
	}
	switch cmd {
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

// listCommands replies with the custom command names (dynamic !commands).
func (r *Router) listCommands(m ChatMessage) {
	if !(m.IsMod || m.IsBroadcaster) && !r.cooldown.Allow(m.User) {
		return
	}
	names, err := r.store.List()
	if err != nil {
		r.logger.Printf("list commands: %v", err)
		return
	}
	if len(names) == 0 {
		r.reply(m, "No custom commands.")
		return
	}
	for i := range names {
		names[i] = "!" + names[i]
	}
	r.reply(m, "Commands: "+strings.Join(names, ", "))
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
		"!addcom", "!editcom", "!delcom", "!commands", "!voices":
		return true
	}
	if _, ok := r.sfx[cmd]; ok {
		return true
	}
	return strings.HasPrefix(cmd, r.cmds.TTSPrefix)
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
