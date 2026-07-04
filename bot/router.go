package main

import (
	"log"
	"strings"
)

// Commands holds the configurable chat command words (all lowercase).
type Commands struct {
	TTSPrefix string // e.g. "!tts"  (voice code may follow with no space: "!ttsb")
	Skip      string // e.g. "!skip"
	Pause     string
	Resume    string
	Clear     string
}

// Router turns parsed chat messages into TTS server calls.
type Router struct {
	cmds     Commands
	minRole  string              // everyone|sub|vip|mod
	sfx      map[string]struct{} // sound commands (lowercased, with the leading "!")
	voices   *VoiceResolver
	cooldown *Cooldown
	sanitize func(text string) (string, bool) // wraps Clean with blocklist+maxChars
	tts      TTS
	logger   *log.Logger
}

// Handle processes one chat message.
func (r *Router) Handle(m ChatMessage) {
	// Remove emotes from the full message first (positions are relative to it),
	// then work with the cleaned text.
	text := strings.TrimSpace(removeEmotes(m.Text, m.Emotes))
	if !strings.HasPrefix(text, "!") {
		return
	}

	cmd := text
	rest := ""
	if sp := strings.IndexByte(text, ' '); sp >= 0 {
		cmd, rest = text[:sp], strings.TrimSpace(text[sp+1:])
	}
	cmd = strings.ToLower(cmd)

	// Standalone mod-only control commands.
	switch cmd {
	case r.cmds.Skip, r.cmds.Pause, r.cmds.Resume, r.cmds.Clear:
		if m.IsMod || m.IsBroadcaster {
			r.control(cmd, m)
		}
		return
	}

	// SFX: standalone sound commands (e.g. "!airhorn"), everyone-eligible and
	// sharing the TTS per-user cooldown. Takes no args.
	if _, ok := r.sfx[cmd]; ok {
		if !r.eligible(m) {
			return
		}
		if !(m.IsMod || m.IsBroadcaster) && !r.cooldown.Allow(m.User) {
			r.logger.Printf("cooldown: ignoring %s", m.User)
			return
		}
		name := strings.TrimPrefix(cmd, "!")
		if err := r.tts.SFX(name); err != nil {
			r.logger.Printf("sfx error: %v", err)
			return
		}
		r.logger.Printf("sfx %q for %s", name, m.User)
		return
	}

	// TTS: "!tts" (random) or "!tts<code>" (specific voice).
	if !strings.HasPrefix(cmd, r.cmds.TTSPrefix) {
		return
	}
	code := strings.TrimPrefix(cmd, r.cmds.TTSPrefix)

	if !r.eligible(m) {
		return
	}
	if !(m.IsMod || m.IsBroadcaster) && !r.cooldown.Allow(m.User) {
		r.logger.Printf("cooldown: ignoring %s", m.User)
		return
	}

	clean, ok := r.sanitize(rest)
	if !ok {
		r.logger.Printf("dropped (empty/blocked) from %s", m.User)
		return
	}
	voice := r.voices.Resolve(code)
	if err := r.tts.Say(clean, voice); err != nil {
		r.logger.Printf("say error: %v", err)
		return
	}
	r.logger.Printf("spoke [%s] for %s: %q", voice, m.User, clean)
}

func (r *Router) eligible(m ChatMessage) bool {
	switch r.minRole {
	case "mod":
		return m.IsMod || m.IsBroadcaster
	case "vip":
		return m.IsVIP || m.IsMod || m.IsBroadcaster
	case "sub":
		return m.IsSub || m.IsVIP || m.IsMod || m.IsBroadcaster
	default: // "everyone"
		return true
	}
}

func (r *Router) control(cmd string, m ChatMessage) {
	var err error
	switch cmd {
	case r.cmds.Skip:
		err = r.tts.Skip()
	case r.cmds.Pause:
		err = r.tts.Pause()
	case r.cmds.Resume:
		err = r.tts.Resume()
	case r.cmds.Clear:
		err = r.tts.Clear()
	}
	if err != nil {
		r.logger.Printf("control %s error: %v", cmd, err)
		return
	}
	r.logger.Printf("%s by %s", cmd, m.User)
}
