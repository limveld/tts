package main

import (
	"io"
	"log"
	"math/rand"
	"testing"
	"time"
)

type fakeTTS struct {
	says     []sayCall
	sfx      []string
	controls []string
}

type sayCall struct{ text, voice string }

func (f *fakeTTS) Say(text, voice string) error {
	f.says = append(f.says, sayCall{text, voice})
	return nil
}
func (f *fakeTTS) SFX(name string) error { f.sfx = append(f.sfx, name); return nil }
func (f *fakeTTS) Pause() error          { f.controls = append(f.controls, "pause"); return nil }
func (f *fakeTTS) Resume() error         { f.controls = append(f.controls, "resume"); return nil }
func (f *fakeTTS) Clear() error          { f.controls = append(f.controls, "clear"); return nil }
func (f *fakeTTS) Skip() error           { f.controls = append(f.controls, "skip"); return nil }

func newTestRouter(tts TTS) *Router {
	return &Router{
		cmds:     Commands{TTSPrefix: "!tts", Skip: "!skip", Pause: "!pause", Resume: "!resume", Clear: "!clear"},
		minRole:  "everyone",
		sfx:      map[string]struct{}{"!airhorn": {}, "!bruh": {}},
		voices:   &VoiceResolver{codes: defaultVoiceCodes(), rnd: rand.New(rand.NewSource(1))},
		cooldown: NewCooldown(30 * time.Second),
		sanitize: func(text string) (string, bool) { return Clean(text, []string{"banned"}, 200) },
		tts:      tts,
		logger:   log.New(io.Discard, "", 0),
	}
}

func msg(user, text string, mod bool) ChatMessage {
	return ChatMessage{User: user, Display: user, Channel: "chan", Text: text, IsMod: mod, IsBroadcaster: mod && user == "chan"}
}

func TestRouterSayRandomAndCoded(t *testing.T) {
	f := &fakeTTS{}
	r := newTestRouter(f)
	r.Handle(msg("bob", "!tts hello there", false))
	r.Handle(msg("alice", "!ttsb hi", false))
	if len(f.says) != 2 {
		t.Fatalf("says=%d want 2", len(f.says))
	}
	if f.says[0].text != "hello there" || !contains(allVoices, f.says[0].voice) {
		t.Errorf("say0=%+v", f.says[0])
	}
	wantB := defaultVoiceCodes()["b"] // "!ttsb" resolves to whatever code b maps to
	if f.says[1].voice != wantB {
		t.Errorf("say1 voice=%q want %q", f.says[1].voice, wantB)
	}
}

func TestRouterSFX(t *testing.T) {
	f := &fakeTTS{}
	r := newTestRouter(f)
	r.Handle(msg("bob", "!airhorn", false))
	r.Handle(msg("carol", "!bruh some ignored args", false)) // args ignored
	r.Handle(msg("dave", "!nope", false))                    // unknown -> ignored
	if len(f.sfx) != 2 || f.sfx[0] != "airhorn" || f.sfx[1] != "bruh" {
		t.Fatalf("sfx=%v want [airhorn bruh]", f.sfx)
	}
	if len(f.says) != 0 {
		t.Errorf("sfx should not speak; says=%v", f.says)
	}
}

func TestRouterSFXSharesTTSCooldown(t *testing.T) {
	f := &fakeTTS{}
	r := newTestRouter(f)
	r.Handle(msg("bob", "!tts hello", false)) // consumes bob's cooldown
	r.Handle(msg("bob", "!airhorn", false))   // blocked by the shared cooldown
	if len(f.says) != 1 {
		t.Fatalf("says=%d want 1", len(f.says))
	}
	if len(f.sfx) != 0 {
		t.Fatalf("sfx=%d want 0 (shared cooldown blocks it)", len(f.sfx))
	}
}

func TestRouterCooldownBlocksSecond(t *testing.T) {
	f := &fakeTTS{}
	r := newTestRouter(f)
	r.Handle(msg("bob", "!tts one", false))
	r.Handle(msg("bob", "!tts two", false)) // within cooldown
	if len(f.says) != 1 {
		t.Fatalf("says=%d want 1 (cooldown)", len(f.says))
	}
}

func TestRouterModsExemptFromCooldown(t *testing.T) {
	f := &fakeTTS{}
	r := newTestRouter(f)
	r.Handle(msg("mod1", "!tts one", true))
	r.Handle(msg("mod1", "!tts two", true))
	if len(f.says) != 2 {
		t.Fatalf("mods should be exempt; says=%d", len(f.says))
	}
}

func TestRouterControlsAreModOnly(t *testing.T) {
	f := &fakeTTS{}
	r := newTestRouter(f)
	r.Handle(msg("viewer", "!skip", false)) // ignored
	r.Handle(msg("mod1", "!skip", true))
	r.Handle(msg("mod1", "!pause please", true)) // trailing args ignored
	if len(f.controls) != 2 || f.controls[0] != "skip" || f.controls[1] != "pause" {
		t.Errorf("controls=%v", f.controls)
	}
	if len(f.says) != 0 {
		t.Errorf("controls should not speak; says=%v", f.says)
	}
}

func TestRouterDropsBlockedEmptyAndNonCommands(t *testing.T) {
	f := &fakeTTS{}
	r := newTestRouter(f)
	r.Handle(msg("bob", "!tts this is banned yo", false))
	r.Handle(msg("carol", "!tts    ", false))
	r.Handle(msg("dave", "just chatting", false))
	r.Handle(msg("erin", "!other thing", false))
	if len(f.says)+len(f.controls) != 0 {
		t.Fatalf("expected nothing; says=%v controls=%v", f.says, f.controls)
	}
}
