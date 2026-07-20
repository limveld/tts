package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// defaultAdMessage is the ad-reminder copy used when notifications.toml doesn't
// override it.
const defaultAdMessage = "📺 Heads up! Ads in about a minute — don't go anywhere, back soon ❤️"

// NotificationsConfig holds the event-notification settings from
// notifications.toml: the shoutout allow-list and the ad-reminder timing/message.
type NotificationsConfig struct {
	Allow     []string      // logins auto-shouted on their first message each stream
	AdLead    time.Duration // how far ahead of an ad to warn
	AdPoll    time.Duration // ad-schedule poll interval
	AdMessage string        // ad reminder text (chat + overlay)
}

// LoadNotificationsConfig parses notifications.toml. A missing file is not an
// error — the feature is opt-in (enabled=false). Absent fields fall back to
// sensible defaults.
func LoadNotificationsConfig(path string) (cfg NotificationsConfig, enabled bool, err error) {
	if _, statErr := os.Stat(path); os.IsNotExist(statErr) {
		return NotificationsConfig{}, false, nil
	}
	var doc struct {
		Shoutout struct {
			Allow []string `toml:"allow"`
		} `toml:"shoutout"`
		Ads struct {
			Lead    string `toml:"lead"`
			Poll    string `toml:"poll"`
			Message string `toml:"message"`
		} `toml:"ads"`
	}
	if _, err := toml.DecodeFile(path, &doc); err != nil {
		return NotificationsConfig{}, false, err
	}

	lead, err := durationOr(doc.Ads.Lead, 60*time.Second)
	if err != nil {
		return NotificationsConfig{}, false, fmt.Errorf("ads.lead: %w", err)
	}
	poll, err := durationOr(doc.Ads.Poll, 30*time.Second)
	if err != nil {
		return NotificationsConfig{}, false, fmt.Errorf("ads.poll: %w", err)
	}

	allow := make([]string, 0, len(doc.Shoutout.Allow))
	for _, l := range doc.Shoutout.Allow {
		if login := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(l), "@")); login != "" {
			allow = append(allow, login)
		}
	}
	return NotificationsConfig{
		Allow:     allow,
		AdLead:    lead,
		AdPoll:    poll,
		AdMessage: orString(doc.Ads.Message, defaultAdMessage),
	}, true, nil
}
