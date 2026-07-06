package main

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// TestIntegrationRawIRCToHTTP drives the full pipeline: raw Twitch IRC lines →
// parse → route → real TTSClient → an httptest server standing in for the TTS
// server. It asserts the exact HTTP calls, with no live Twitch or audio.
func TestIntegrationRawIRCToHTTP(t *testing.T) {
	var mu sync.Mutex
	var sayBodies []map[string]string
	var sfxBodies []map[string]string
	var paths []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		paths = append(paths, r.URL.Path)
		switch r.URL.Path {
		case "/say":
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			sayBodies = append(sayBodies, body)
		case "/sfx":
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			sfxBodies = append(sfxBodies, body)
		}
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	r := &Router{
		cmds:     Commands{TTSPrefix: "!tts", Skip: "!skip", Pause: "!pause", Resume: "!resume", Clear: "!clear"},
		minRole:  "everyone",
		sfx:      map[string]struct{}{"!airhorn": {}},
		cooldown: NewCooldown(30 * time.Second),
		sanitize: func(text string) (string, bool) { return Clean(text, nil, 200) },
		tts:      NewTTSClient(srv.URL, ""),
		logger:   log.New(io.Discard, "", 0),
	}

	lines := []string{
		// "!ttsb Kappa hello": the Kappa emote occupies code points 6-10.
		`@display-name=Bob;mod=0;emotes=25:6-10 :bob!bob@bob.tmi.twitch.tv PRIVMSG #chan :!ttsb Kappa hello`,
		// a sound command (different user, so the shared cooldown doesn't block it)
		`@display-name=Sam;mod=0 :sam!sam@sam.tmi.twitch.tv PRIVMSG #chan :!airhorn`,
		// mod skip
		`@display-name=Mod;mod=1 :mod!mod@mod.tmi.twitch.tv PRIVMSG #chan :!skip`,
		// non-mod skip is ignored
		`@display-name=Viewer;mod=0 :viewer!viewer@viewer.tmi.twitch.tv PRIVMSG #chan :!skip`,
	}
	for _, l := range lines {
		m, ok := parsePrivmsg(l)
		if !ok {
			t.Fatalf("parse failed: %s", l)
		}
		r.Handle(m)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(sayBodies) != 1 {
		t.Fatalf("expected 1 /say, got %d", len(sayBodies))
	}
	if sayBodies[0]["code"] != "b" { // "!ttsb" forwards the raw code "b"
		t.Errorf("code=%q want %q", sayBodies[0]["code"], "b")
	}
	if sayBodies[0]["text"] != "hello" { // emote stripped, prefix removed
		t.Errorf("text=%q want %q", sayBodies[0]["text"], "hello")
	}
	if len(sfxBodies) != 1 {
		t.Fatalf("expected 1 /sfx, got %d", len(sfxBodies))
	}
	if sfxBodies[0]["name"] != "airhorn" {
		t.Errorf("sfx name=%q want airhorn", sfxBodies[0]["name"])
	}
	skips := 0
	for _, p := range paths {
		if p == "/skip" {
			skips++
		}
	}
	if skips != 1 {
		t.Errorf("expected exactly 1 /skip (mod only), got %d; paths=%v", skips, paths)
	}
}
