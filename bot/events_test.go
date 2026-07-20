package main

import (
	"os"
	"testing"
	"time"
)

func TestAdDue(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	lead := 60 * time.Second
	var never time.Time

	cases := []struct {
		name       string
		next       time.Time
		lastWarned time.Time
		want       bool
	}{
		{"none scheduled", time.Time{}, never, false},
		{"within lead", now.Add(45 * time.Second), never, true},
		{"exactly at lead", now.Add(60 * time.Second), never, true},
		{"beyond lead", now.Add(90 * time.Second), never, false},
		{"already passed", now.Add(-5 * time.Second), never, false},
		{"already warned", now.Add(45 * time.Second), now.Add(45 * time.Second), false},
		{"new ad after warning old", now.Add(45 * time.Second), now.Add(-600 * time.Second), true},
	}
	for _, c := range cases {
		if got := adDue(c.next, now, lead, c.lastWarned); got != c.want {
			t.Errorf("%s: adDue = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestLoadNotificationsConfigDefaults(t *testing.T) {
	// Missing file → disabled, no error.
	_, enabled, err := LoadNotificationsConfig("does-not-exist.toml")
	if err != nil || enabled {
		t.Fatalf("missing file: enabled=%v err=%v, want false/nil", enabled, err)
	}
}

func TestLoadNotificationsConfigParses(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/notifications.toml"
	body := "[shoutout]\nallow = [\"@Friend\", \"pal\"]\n[ads]\nlead = \"90s\"\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, enabled, err := LoadNotificationsConfig(path)
	if err != nil || !enabled {
		t.Fatalf("enabled=%v err=%v", enabled, err)
	}
	if len(cfg.Allow) != 2 || cfg.Allow[0] != "friend" || cfg.Allow[1] != "pal" {
		t.Errorf("allow=%v want normalized [friend pal]", cfg.Allow)
	}
	if cfg.AdLead != 90*time.Second {
		t.Errorf("lead=%s want 90s", cfg.AdLead)
	}
	if cfg.AdPoll != 30*time.Second {
		t.Errorf("poll=%s want 30s (default)", cfg.AdPoll)
	}
	if cfg.AdMessage != defaultAdMessage {
		t.Errorf("message=%q want default", cfg.AdMessage)
	}
}
