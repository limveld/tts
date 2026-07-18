package main

import (
	"io"
	"log"
	"math/rand"
	"path/filepath"
	"testing"

	"tts/store"
)

func newCmdRouter(t *testing.T, tts TTS, chat Chat) (*Router, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	r := &Router{
		cmds:     Commands{TTSPrefix: "!tts", Skip: "!skip", Pause: "!pause", Resume: "!resume", Clear: "!clear", SFX: "!sfx"},
		minRole:  "everyone",
		sfx:      map[string]struct{}{"!airhorn": {}},
		cooldown: NewCooldown(0), // no per-user cooldown in tests
		sanitize: func(text string) (string, bool) { return Clean(text, nil, 200) },
		tts:      tts,
		chat:     chat,
		store:    st,
		rnd:      rand.New(rand.NewSource(1)),
		logger:   log.New(io.Discard, "", 0),
	}
	return r, st
}

func lastReply(c *fakeChat) string {
	if len(c.replies) == 0 {
		return ""
	}
	return c.replies[len(c.replies)-1].text
}

func TestAddUseEditDelCommand(t *testing.T) {
	chat := &fakeChat{}
	r, st := newCmdRouter(t, &fakeTTS{}, chat)

	// add (broadcaster)
	r.Handle(msg("chan", "!addcom !discord join $user at discord.gg/x", true))
	if c, ok, _ := st.Get("discord"); !ok || c.Response != "join $user at discord.gg/x" {
		t.Fatalf("discord not stored: %+v ok=%v", c, ok)
	}
	// non-mod can't add
	r.Handle(msg("bob", "!addcom !evil hi", false))
	if _, ok, _ := st.Get("evil"); ok {
		t.Error("non-mod added a command")
	}
	// use it (substitution + count)
	r.Handle(msg("bob", "!discord", false))
	if lastReply(chat) != "join bob at discord.gg/x" {
		t.Errorf("custom reply=%q", lastReply(chat))
	}
	if c, _, _ := st.Get("discord"); c.Count != 1 {
		t.Errorf("count=%d want 1", c.Count)
	}
	// edit
	r.Handle(msg("chan", "!editcom !discord new text", true))
	if c, _, _ := st.Get("discord"); c.Response != "new text" {
		t.Errorf("edit didn't take: %q", c.Response)
	}
	// can't shadow a built-in
	r.Handle(msg("chan", "!addcom !tts nope", true))
	if _, ok, _ := st.Get("tts"); ok {
		t.Error("shadowed built-in !tts")
	}
	// delete
	r.Handle(msg("chan", "!delcom !discord", true))
	if _, ok, _ := st.Get("discord"); ok {
		t.Error("discord not deleted")
	}
}

func TestListCommandsAndVoices(t *testing.T) {
	chat := &fakeChat{}
	f := &fakeTTS{voices: []VoiceInfo{{Code: "n", Voice: "af_nicole"}, {Code: "k", Voice: "Kevin"}}}
	r, st := newCmdRouter(t, f, chat)
	st.Add(store.Command{Name: "discord", Response: "x"})
	st.Add(store.Command{Name: "socials", Response: "y"})

	r.Handle(msg("bob", "!commands", false))
	if lastReply(chat) != "Commands: !tts, !sfx, !voices, !wordle, !guess, !wordlewins, !discord, !socials" {
		t.Errorf("!commands reply=%q", lastReply(chat))
	}
	r.Handle(msg("bob", "!voices", false))
	if lastReply(chat) != "Voices (!tts<code>): n=af_nicole, k=Kevin" {
		t.Errorf("!voices reply=%q", lastReply(chat))
	}
}

func TestCustomCommandCooldownAndRole(t *testing.T) {
	chat := &fakeChat{}
	r, st := newCmdRouter(t, &fakeTTS{}, chat)
	st.Add(store.Command{Name: "hi", Response: "hello", Cooldown: 3600})

	r.Handle(msg("bob", "!hi", false))
	r.Handle(msg("bob", "!hi", false)) // within cooldown → blocked
	if n := len(chat.replies); n != 1 {
		t.Errorf("replies=%d want 1 (second blocked by cooldown)", n)
	}
	r.Handle(msg("chan", "!hi", true)) // mods are exempt from cooldown
	if n := len(chat.replies); n != 2 {
		t.Errorf("replies=%d want 2 (mod exempt)", n)
	}

	// A mod-only command ignores a regular user.
	st.Add(store.Command{Name: "secret", Response: "shh", MinRole: "mod"})
	r.Handle(msg("bob", "!secret", false))
	if lastReply(chat) == "shh" {
		t.Error("regular user ran a mod-only command")
	}
	r.Handle(msg("chan", "!secret", true))
	if lastReply(chat) != "shh" {
		t.Errorf("mod couldn't run mod-only command: %q", lastReply(chat))
	}
}
