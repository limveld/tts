package main

import (
	"fmt"
	"log"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"time"

	"tts/store"
)

// Commands holds the configurable chat command words (all lowercase).
type Commands struct {
	TTSPrefix string // e.g. "!tts"  (voice code may follow with no space: "!ttsb")
	Skip      string // e.g. "!skip"
	Pause     string
	Resume    string
	Clear     string
	SFX       string
}

// Router turns parsed chat messages into TTS server calls.
type Router struct {
	cmds     Commands
	minRole  string              // everyone|sub|vip|mod
	sfx      map[string]struct{} // sound commands (lowercased, with the leading "!")
	cooldown *Cooldown
	// notifyCooldown rate-limits the "slow down" reply to once per cooldown
	// window (same duration as cooldown) so a spammer isn't spammed back.
	notifyCooldown *Cooldown
	sanitize       func(text string) (string, bool) // wraps Clean with blocklist+maxChars
	tts            TTS
	chat           Chat         // may be nil when the bot isn't authenticated (replies disabled)
	store          *store.Store // custom commands + marks economy (may be nil → both disabled)
	rnd            *rand.Rand   // for $random substitution
	logger         *log.Logger

	economy  bool          // marks economy active: enable !marks/!leaderboard/!grant/…
	econ     EconomyConfig // currency name + per-command costs
	charging bool          // paid mode: !tts/!sfx deduct marks (false = free mode)
	resolver UserResolver  // resolves unseen logins for !grant (nil when no Twitch client)
	info     TwitchInfo    // Helix lookups for !uptime/!followage (nil when no Twitch client)

	cdMu        sync.Mutex           // guards cmdCooldown
	cmdCooldown map[string]time.Time // per-command global cooldown

	gambleMu sync.Mutex   // guards round (mutated by handler + resolve/flush timers)
	round    *gambleRound // the open !g gamble, or nil
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

	switch cmd {
	case r.cmds.SFX:
		r.listSounds(m)
		return

	case r.cmds.Skip, r.cmds.Pause, r.cmds.Resume, r.cmds.Clear:
		// Standalone mod-only control commands.
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
		if r.cooldownBlocked(m) {
			return
		}
		if !r.canAfford(m, r.econ.SFXCost) {
			return
		}
		name := strings.TrimPrefix(cmd, "!")
		if err := r.tts.SFX(name); err != nil {
			r.logger.Printf("sfx error: %v", err)
			return
		}
		r.chargeAfter(m, r.econ.SFXCost, "sfx")
		r.logger.Printf("sfx %q for %s", name, m.User)
		return
	}

	// Command engine: admin CRUD (!addcom…), the dynamic !commands/!voices, and
	// custom stored commands. Runs after the built-ins above so those always win.
	if r.handleCommands(cmd, rest, m) {
		return
	}

	// TTS: "!tts" (random voice) or "!tts<code>" (specific voice). The code is
	// forwarded to the server, which owns the code→voice map (per engine).
	if !strings.HasPrefix(cmd, r.cmds.TTSPrefix) {
		return
	}
	code := strings.TrimPrefix(cmd, r.cmds.TTSPrefix)

	if !r.eligible(m) {
		return
	}
	if r.cooldownBlocked(m) {
		return
	}

	clean, ok := r.sanitize(rest)
	if !ok {
		r.logger.Printf("dropped (empty/blocked) from %s", m.User)
		return
	}
	if !r.canAfford(m, r.econ.TTSCost) {
		return
	}
	if err := r.tts.Say(clean, code); err != nil {
		r.logger.Printf("say error: %v", err)
		return
	}
	r.chargeAfter(m, r.econ.TTSCost, "tts")
	r.logger.Printf("spoke [%s] for %s: %q", code, m.User, clean)
}

// economyActive reports whether a marks cost should be applied for this action.
// Free mode (r.charging == false) waives the cost even when the economy is on.
func (r *Router) economyActive(cost int64) bool {
	return r.economy && r.charging && r.store != nil && cost > 0
}

// canAfford checks (without charging) that m's user can pay cost marks, and
// sends a polite refusal if not. It returns true when the caller may proceed
// (economy off, zero cost, or affordable). A concurrent credit can only raise
// the balance, so the later chargeAfter never fails after this passes.
func (r *Router) canAfford(m ChatMessage, cost int64) bool {
	if !r.economyActive(cost) {
		return true
	}
	bal, err := r.store.Balance(m.UserID)
	if err != nil {
		r.logger.Printf("balance %s: %v", m.User, err)
		return false // fail closed — don't grant free use on a DB error
	}
	if bal < cost {
		r.reply(m, fmt.Sprintf("@%s you need %d %s (you have %d).", displayName(m), cost, r.econ.CurrencyName, bal))
		return false
	}
	return true
}

// chargeAfter debits cost marks once the effect succeeded (a failed effect never
// reaches here, so there's nothing to refund).
func (r *Router) chargeAfter(m ChatMessage, cost int64, reason string) {
	if !r.economyActive(cost) {
		return
	}
	if ok, err := r.store.Spend(m.UserID, cost, reason); err != nil {
		r.logger.Printf("charge %s: %v", m.User, err)
	} else if !ok {
		r.logger.Printf("charge %s: insufficient at debit", m.User)
	}
}

func (r *Router) eligible(m ChatMessage) bool { return r.roleAllows(r.minRole, m) }

// cooldownBlocked reports whether m's user is on the shared per-user cooldown
// (mods/broadcaster exempt). On the first block within a window it replies
// "slow down — Ns left"; further blocks in the same window stay silent.
func (r *Router) cooldownBlocked(m ChatMessage) bool {
	if m.IsMod || m.IsBroadcaster {
		return false
	}
	if r.cooldown.Allow(m.User) {
		return false
	}
	if r.notifyCooldown != nil && r.notifyCooldown.Allow(m.User) {
		left := int(r.cooldown.Remaining(m.User).Seconds() + 0.999)
		if left < 1 {
			left = 1
		}
		r.reply(m, fmt.Sprintf("@%s slow down — %ds left.", displayName(m), left))
	}
	r.logger.Printf("cooldown: ignoring %s", m.User)
	return true
}

// roleAllows reports whether m meets the given minimum role (everyone|sub|vip|mod).
func (r *Router) roleAllows(role string, m ChatMessage) bool {
	switch role {
	case "mod":
		return m.IsMod || m.IsBroadcaster
	case "vip":
		return m.IsVIP || m.IsMod || m.IsBroadcaster
	case "sub":
		return m.IsSub || m.IsVIP || m.IsMod || m.IsBroadcaster
	default: // "everyone" (or unset)
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

// listSounds replies in chat with the available sound commands. Requires an
// authenticated chat sender; shares the TTS per-user cooldown (mods exempt).
func (r *Router) listSounds(m ChatMessage) {
	if r.chat == nil {
		r.logger.Printf("!sfx: replies not configured — run 'mise run bot:auth'")
		return
	}
	if !(m.IsMod || m.IsBroadcaster) && !r.cooldown.Allow(m.User) {
		r.logger.Printf("cooldown: ignoring %s", m.User)
		return
	}

	keys := make([]string, 0, len(r.sfx))
	for k := range r.sfx {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	msg := "No sounds configured."
	if len(keys) > 0 {
		msg = "Sounds: " + strings.Join(keys, ", ")
	}
	if err := r.chat.Reply(m.RoomID, m.ID, msg); err != nil {
		r.logger.Printf("!sfx reply error: %v", err)
		return
	}
	r.logger.Printf("!sfx list for %s", m.User)
}
