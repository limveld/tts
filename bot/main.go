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

	db, err := store.Open(cfg.DBPath)
	if err != nil {
		logger.Fatalf("store %s: %v", cfg.DBPath, err)
	}
	defer db.Close()
	seedCommands(db, logger)

	rnd := rand.New(rand.NewSource(time.Now().UnixNano()))
	router := &Router{
		cmds:     cfg.Cmds,
		minRole:  cfg.MinRole,
		sfx:      cfg.SFX,
		cooldown: NewCooldown(cfg.Cooldown),
		sanitize: func(text string) (string, bool) {
			return Clean(text, cfg.Blocklist, cfg.MaxChars)
		},
		tts:    NewTTSClient(cfg.TTSURL, cfg.TTSToken),
		chat:   buildChat(cfg, logger),
		store:  db,
		rnd:    rnd,
		logger: logger,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Count chat lines and remember the channel's broadcaster id (from room-id
	// tags) so timers can gate on activity and post without a triggering message.
	var lineCount atomic.Int64
	var roomID atomic.Pointer[string]
	handle := func(m ChatMessage) {
		lineCount.Add(1)
		if m.RoomID != "" {
			id := m.RoomID
			roomID.Store(&id)
		}
		router.Handle(m)
	}

	if router.chat != nil && len(cfg.Timers) > 0 {
		timers := NewTimers(cfg.Timers, router.chat, lineCount.Load,
			func() string {
				if p := roomID.Load(); p != nil {
					return *p
				}
				return ""
			}, logger)
		go timers.Run(ctx)
		logger.Printf("timers: %d configured", len(cfg.Timers))
	}

	irc := &IRCClient{channel: cfg.Channel, logger: logger, rnd: rnd, handle: handle}
	logger.Printf("tts-bot: channel=#%s tts=%s cooldown=%s min-role=%s sfx=%d",
		cfg.Channel, cfg.TTSURL, cfg.Cooldown, cfg.MinRole, len(cfg.SFX))
	irc.Run(ctx)
	logger.Printf("shutting down")
}

// seedCommands inserts a few starter stored-text commands (migrated from
// StreamElements) when the commands table is empty, so a fresh DB isn't blank.
// !commands and !voices are dynamic built-ins, so they're not seeded.
func seedCommands(db *store.Store, logger *log.Logger) {
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
		{Name: "discord", Response: "Discord: (set me with !editcom !discord <link>)"},
		{Name: "socials", Response: "Socials: (set me with !editcom !socials <links>)"},
		{Name: "schedule", Response: "Schedule: (set me with !editcom !schedule <text>)"},
	} {
		if _, err := db.Add(c); err != nil {
			logger.Printf("seed %q: %v", c.Name, err)
		}
	}
	logger.Printf("seeded starter commands (edit with !editcom)")
}

// buildChat returns a Chat sender when Twitch credentials and a saved token are
// both present; otherwise nil, so the bot still runs read-only (replies just
// no-op with a log line until `mise run bot:auth` is done).
func buildChat(cfg Config, logger *log.Logger) Chat {
	if cfg.TwitchClientID == "" || cfg.TwitchSecret == "" {
		return nil
	}
	tokStore := twitch.NewStore(cfg.TokenStore)
	tok, err := tokStore.Load()
	if err != nil {
		logger.Printf("twitch: token store: %v", err)
		return nil
	}
	if tok == nil {
		logger.Printf("twitch: no saved token — run 'mise run bot:auth' to enable chat replies")
		return nil
	}
	client := twitch.NewClient(cfg.TwitchClientID, cfg.TwitchSecret, tokStore)
	client.SetToken(tok)
	logger.Printf("twitch: chat replies enabled as %s", tok.Login)
	return NewChatSender(client)
}
