// Command tts-bot connects to Twitch chat (anonymous, read-only) and drives the
// TTS server: `!tts <message>` speaks (random voice, or a specific one via a
// code suffix like `!ttsb`), and mod-only `!skip`/`!pause`/`!resume`/`!clear`
// control the queue.
package main

import (
	"context"
	"log"
	"math/rand"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"tts/store"
	"tts/twitch"
)

func main() {
	logger := log.New(os.Stderr, "", log.LstdFlags)

	cfg, err := LoadConfig(os.Args[1:])
	if err != nil {
		logger.Fatalf("config: %v", err)
	}

	db, err := openStore(cfg.DB)
	if err != nil {
		logger.Fatalf("store %s: %v", cfg.DB, err)
	}
	defer db.Close()
	seedCommands(db, logger)

	rnd := rand.New(rand.NewSource(time.Now().UnixNano()))
	chat, client, tok := buildChat(cfg, logger)
	router := &Router{
		cmds:     cfg.Cmds,
		minRole:  cfg.MinRole,
		sfx:      cfg.SFX,
		cooldown: NewCooldown(cfg.Cooldown),
		sanitize: func(text string) (string, bool) {
			return Clean(text, cfg.Blocklist, cfg.MaxChars)
		},
		tts:            NewTTSClient(cfg.TTSURL, cfg.TTSToken),
		chat:           chat,
		store:          db,
		rnd:            rnd,
		logger:         logger,
		notifyCooldown: NewCooldown(cfg.Cooldown),
		overlay:        NewOverlayClient(cfg.TTSURL, cfg.TTSToken, logger),
	}

	// Informational commands (!uptime/!followage) need only the Twitch client.
	if client != nil {
		router.info = twitchInfo{client: client}
	}

	// Shoutout allow-list (auto-shoutout on first message). !so works regardless.
	if len(cfg.Notifications.Allow) > 0 {
		router.shoutAllow = make(map[string]bool, len(cfg.Notifications.Allow))
		for _, login := range cfg.Notifications.Allow {
			router.shoutAllow[login] = true
		}
	}
	router.shouted = make(map[string]bool)

	// Seed the overlay with the persisted depth value so it renders on connect
	// (the server caches the push and replays it to the browser source).
	router.pushDepth(router.depthPoints())
	// Restore an in-progress Wordle round (survives a bot restart).
	router.loadWordle()
	// Load the Connections puzzle bank and restore an in-progress round.
	router.loadConnections(cfg.ConnectionsFile)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Count chat lines and remember the channel's broadcaster id (from room-id
	// tags) so timers can gate on activity and post without a triggering message.
	var lineCount atomic.Int64
	var roomID atomic.Pointer[string]
	roomIDOf := func() string {
		if p := roomID.Load(); p != nil {
			return *p
		}
		return ""
	}

	// The chat log. Its writer owns every database call it makes and runs on its
	// own goroutine, so nothing here can stall the read loop below — which is also
	// the loop that answers !tts. See bot/chatlog.go.
	//
	// Wait is deferred after openStore's Close, and defers run last-in-first-out,
	// so the final batch is flushed while the handle it needs is still open. It is
	// bounded because a wedged database must not also mean a bot that will not
	// exit.
	var chatLog *ChatLogger
	if cfg.ChatLog {
		chatLog = NewChatLogger(db, roomIDOf, logger)
		go chatLog.Run(ctx)
		defer chatLog.Wait(5 * time.Second)
		logger.Printf("chat log: persisting every line to %s", cfg.DB)
	}

	handle := func(m ChatMessage) {
		lineCount.Add(1)
		if m.RoomID != "" {
			id := m.RoomID
			roomID.Store(&id)
		}
		if chatLog != nil {
			// Before Handle, so a line is logged whether or not the router does
			// anything with it — which for most of chat is nothing at all.
			chatLog.Message(m)
		}
		router.Handle(m)
	}

	// Marks economy: needs points.toml plus a broadcaster token carrying the
	// accrual/conversion scopes. When present, charge tts/sfx and run the earning
	// loops; otherwise the bot runs with tts/sfx free (today's behavior).
	if cfg.EconomyEnabled && client != nil && hasEconomyScopes(tok) {
		router.economy = true
		router.econ = cfg.Economy
		router.resolver = twitchResolver{client: client}
		// Charge mode is a persisted runtime toggle (!free/!paid); default paid.
		router.charging = true
		if mode, ok, err := db.GetSetting("charge_mode"); err != nil {
			logger.Printf("charge_mode: %v", err)
		} else if ok && mode == "free" {
			router.charging = false
		}
		// Broadcaster id: prefer the room-id we read from chat; fall back to the
		// token owner's id (correct under broadcaster auth) so accrual works even
		// before the first chat line.
		broadcasterID := func() string {
			if id := roomIDOf(); id != "" {
				return id
			}
			return client.SenderID()
		}
		econ := NewEconomy(db, client, cfg.Economy, broadcasterID, logger)
		go econ.Run(ctx)
		mode := "paid"
		if !router.charging {
			mode = "free"
		}
		logger.Printf("economy enabled (%s): currency=%s tts=%d sfx=%d accrual=%s/%d",
			mode, cfg.Economy.CurrencyName, cfg.Economy.TTSCost, cfg.Economy.SFXCost,
			cfg.Economy.AccrualInterval, cfg.Economy.AccrualRate)
	} else if cfg.EconomyEnabled {
		logger.Printf("economy configured but disabled: authorize as broadcaster with marks scopes (run 'mise run bot:auth'); !tts/!sfx are free")
	}

	// Restore a !g round interrupted by a restart — its buy-ins are escrowed in
	// the ledger, so without this they'd be stranded. Deliberately placed here:
	// after the economy block above (the settle path formats chat with
	// econ.CurrencyName) but before irc.Run, so a resumed round is live before the
	// first join arrives. Outside the economy branch on purpose — the escrow is
	// real whether or not the economy came up this boot.
	router.loadGamble()

	if router.chat != nil && len(cfg.Timers) > 0 {
		timers := NewTimers(cfg.Timers, router.chat, lineCount.Load, roomIDOf, logger)
		go timers.Run(ctx)
		logger.Printf("timers: %d configured", len(cfg.Timers))
	}

	// Event notifications: the loop tracks live state (resetting the shoutout
	// session on going live) and, when the token carries channel:read:ads, warns
	// ~1 min before scheduled ads. Needs a Twitch client for the Helix reads.
	if cfg.NotificationsEnabled && client != nil {
		ads := tokenHasScope(tok, "channel:read:ads")
		events := NewEvents(router, roomIDOf, cfg.Notifications, ads, logger)
		go events.Run(ctx)
		logger.Printf("notifications: shoutouts=%d ad-reminders=%v (lead=%s poll=%s)",
			len(cfg.Notifications.Allow), ads, cfg.Notifications.AdLead, cfg.Notifications.AdPoll)
		if !ads {
			logger.Printf("notifications: ad reminders disabled — re-authorize for channel:read:ads (run 'mise run bot:auth')")
		}
	}

	irc := &IRCClient{channel: cfg.Channel, logger: logger, rnd: rnd, handle: handle}
	if chatLog != nil {
		// Left nil when the chat log is off: nothing else in the bot cares that a
		// message was deleted, so there would be no one to tell.
		irc.onDelete, irc.onClear = chatLog.Delete, chatLog.Clear
	}
	logger.Printf("tts-bot: channel=#%s tts=%s cooldown=%s min-role=%s sfx=%d",
		cfg.Channel, cfg.TTSURL, cfg.Cooldown, cfg.MinRole, len(cfg.SFX))
	irc.Run(ctx)
	logger.Printf("shutting down")
}

// seedCommands inserts a few starter stored-text commands (migrated from
// StreamElements) when the commands table is empty, so a fresh DB isn't blank.
// !commands and !voices are dynamic built-ins, so they're not seeded.
func seedCommands(db CommandStore, logger *log.Logger) {
	names, err := db.List()
	if err != nil {
		logger.Printf("seed: %v", err)
		return
	}
	if len(names) > 0 {
		return
	}
	for _, c := range []store.Command{
		{Name: "ttshelp", Response: "Type !tts <message> and I'll read it. Pick a voice with a code like !ttsk — see !voices."},
		{Name: "socials", Response: "Socials: (set me with !editcom !socials <links>)"},
		{Name: "schedule", Response: "Schedule: (set me with !editcom !schedule <text>)"},
	} {
		if _, err := db.Add(c); err != nil {
			logger.Printf("seed %q: %v", c.Name, err)
		}
	}
	logger.Printf("seeded starter commands (edit with !editcom)")
}

// buildChat returns a Chat sender plus the underlying client and token when
// Twitch credentials and a saved token are both present; otherwise all nil, so
// the bot still runs read-only (replies no-op with a log line until
// `mise run bot:auth` is done). The client + token are also what the marks
// economy needs (Get Chatters / channel points), so they're returned too.
func buildChat(cfg Config, logger *log.Logger) (Chat, *twitch.Client, *twitch.Token) {
	if cfg.TwitchClientID == "" || cfg.TwitchSecret == "" {
		return nil, nil, nil
	}
	tokStore := twitch.NewStore(cfg.TokenStore)
	tok, err := tokStore.Load()
	if err != nil {
		logger.Printf("twitch: token store: %v", err)
		return nil, nil, nil
	}
	if tok == nil {
		logger.Printf("twitch: no saved token — run 'mise run bot:auth' to enable chat replies")
		return nil, nil, nil
	}
	client := twitch.NewClient(cfg.TwitchClientID, cfg.TwitchSecret, tokStore)
	client.SetLogger(logger) // so a failed token persist is visible without failing the request
	client.SetToken(tok)
	logger.Printf("twitch: chat replies enabled as %s", tok.Login)
	return NewChatSender(client), client, tok
}

// economyScopes are the token scopes the marks economy needs beyond chat send:
// watch-time accrual (Get Chatters) and channel-point conversion.
var economyScopes = []string{
	"moderator:read:chatters",
	"channel:read:redemptions",
	"channel:manage:redemptions",
}

// tokenHasScope reports whether tok carries the given scope.
func tokenHasScope(tok *twitch.Token, scope string) bool {
	if tok == nil {
		return false
	}
	for _, s := range tok.Scope {
		if s == scope {
			return true
		}
	}
	return false
}

// hasEconomyScopes reports whether tok carries all economyScopes (so the bot was
// authorized as the broadcaster with the marks scopes).
func hasEconomyScopes(tok *twitch.Token) bool {
	if tok == nil {
		return false
	}
	have := make(map[string]bool, len(tok.Scope))
	for _, s := range tok.Scope {
		have[s] = true
	}
	for _, need := range economyScopes {
		if !have[need] {
			return false
		}
	}
	return true
}
