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
		voices:   &VoiceResolver{codes: defaultVoiceCodes(), rnd: rnd},
		cooldown: NewCooldown(cfg.Cooldown),
		sanitize: func(text string) (string, bool) {
			return Clean(text, cfg.Blocklist, cfg.MaxChars)
		},
		tts:    NewTTSClient(cfg.TTSURL, cfg.TTSToken),
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
