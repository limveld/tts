package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"tts/sfxlib"
)

// Config is the resolved bot configuration.
type Config struct {
	Channel   string
	TTSURL    string
	TTSToken  string
	Cooldown  time.Duration
	MaxChars  int
	MinRole   string
	Cmds      Commands
	Blocklist []string
	SFX       map[string]struct{} // sound commands (lowercased, with leading "!")
}

// LoadConfig parses flags/env and an optional JSON config file (blocklist).
func LoadConfig(args []string) (Config, error) {
	fs := flag.NewFlagSet("tts-bot", flag.ContinueOnError)
	var c Config
	var configPath string
	fs.StringVar(&c.Channel, "channel", "", "Twitch channel to join (required)")
	fs.StringVar(&c.TTSURL, "tts-url", "http://127.0.0.1:8080", "TTS server base URL")
	fs.StringVar(&c.TTSToken, "tts-token", os.Getenv("TTS_TOKEN"), "TTS server bearer token (env TTS_TOKEN)")
	fs.DurationVar(&c.Cooldown, "cooldown", 30*time.Second, "per-user cooldown for !tts")
	fs.IntVar(&c.MaxChars, "max-chars", 200, "max spoken characters")
	fs.StringVar(&c.MinRole, "min-role", "everyone", "who can use !tts: everyone|sub|vip|mod")
	fs.StringVar(&c.Cmds.TTSPrefix, "cmd-tts", "!tts", "TTS command prefix (voice code may follow: !ttsb)")
	fs.StringVar(&c.Cmds.Skip, "cmd-skip", "!skip", "skip command (mod-only)")
	fs.StringVar(&c.Cmds.Pause, "cmd-pause", "!pause", "pause command (mod-only)")
	fs.StringVar(&c.Cmds.Resume, "cmd-resume", "!resume", "resume command (mod-only)")
	fs.StringVar(&c.Cmds.Clear, "cmd-clear", "!clear", "clear command (mod-only)")
	fs.StringVar(&configPath, "config", "", "path to JSON config file (blocklist)")
	var sfxPath string
	fs.StringVar(&sfxPath, "sfx-config", "sfx.toml", "soundboard TOML (registers a !command per sound); optional")
	if err := fs.Parse(args); err != nil {
		return c, err
	}

	c.Channel = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(c.Channel)), "#")
	if c.Channel == "" {
		return c, fmt.Errorf("-channel is required")
	}

	if configPath != "" {
		raw, err := os.ReadFile(configPath)
		if err != nil {
			return c, fmt.Errorf("reading config: %w", err)
		}
		var fileCfg struct {
			Blocklist []string `json:"blocklist"`
		}
		if err := json.Unmarshal(raw, &fileCfg); err != nil {
			return c, fmt.Errorf("parsing config: %w", err)
		}
		c.Blocklist = fileCfg.Blocklist
	}

	// Soundboard: register a "!<name>" command per sound. A missing file just
	// means no SFX (opt-in); a present-but-invalid file is a real error.
	if _, err := os.Stat(sfxPath); err == nil {
		lib, err := sfxlib.Load(sfxPath)
		if err != nil {
			return c, fmt.Errorf("parsing sfx config: %w", err)
		}
		c.SFX = make(map[string]struct{}, len(lib))
		for name := range lib {
			c.SFX["!"+strings.ToLower(name)] = struct{}{}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return c, fmt.Errorf("reading sfx config: %w", err)
	}

	// Chat is matched lowercased, so normalize command words.
	c.Cmds.TTSPrefix = strings.ToLower(c.Cmds.TTSPrefix)
	c.Cmds.Skip = strings.ToLower(c.Cmds.Skip)
	c.Cmds.Pause = strings.ToLower(c.Cmds.Pause)
	c.Cmds.Resume = strings.ToLower(c.Cmds.Resume)
	c.Cmds.Clear = strings.ToLower(c.Cmds.Clear)
	return c, nil
}
