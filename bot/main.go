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
	"syscall"
	"time"

	"tts/twitch"
)

func main() {
	logger := log.New(os.Stderr, "", log.LstdFlags)

	cfg, err := LoadConfig(os.Args[1:])
	if err != nil {
		logger.Fatalf("config: %v", err)
	}

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
		logger: logger,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	irc := &IRCClient{channel: cfg.Channel, logger: logger, rnd: rnd, handle: router.Handle}
	logger.Printf("tts-bot: channel=#%s tts=%s cooldown=%s min-role=%s sfx=%d",
		cfg.Channel, cfg.TTSURL, cfg.Cooldown, cfg.MinRole, len(cfg.SFX))
	irc.Run(ctx)
	logger.Printf("shutting down")
}

// buildChat returns a Chat sender when Twitch credentials and a saved token are
// both present; otherwise nil, so the bot still runs read-only (replies just
// no-op with a log line until `mise run bot:auth` is done).
func buildChat(cfg Config, logger *log.Logger) Chat {
	if cfg.TwitchClientID == "" || cfg.TwitchSecret == "" {
		return nil
	}
	store := twitch.NewStore(cfg.TokenStore)
	tok, err := store.Load()
	if err != nil {
		logger.Printf("twitch: token store: %v", err)
		return nil
	}
	if tok == nil {
		logger.Printf("twitch: no saved token — run 'mise run bot:auth' to enable chat replies")
		return nil
	}
	client := twitch.NewClient(cfg.TwitchClientID, cfg.TwitchSecret, store)
	client.SetToken(tok)
	logger.Printf("twitch: chat replies enabled as %s", tok.Login)
	return NewChatSender(client)
}
