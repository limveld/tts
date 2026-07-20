package main

import (
	"strings"
	"testing"
)

// soRouter wires a router with the shoutout seam: a fake chat, a fake overlay,
// and a fakeInfo returning the given game/avatar.
func soRouter(t *testing.T, game, avatar string) (*Router, *fakeChat, *fakeOverlay) {
	t.Helper()
	r := newTestRouter(&fakeTTS{})
	chat := &fakeChat{}
	ov := &fakeOverlay{}
	r.chat = chat
	r.overlay = ov
	r.info = fakeInfo{game: game, avatar: avatar}
	r.shouted = make(map[string]bool)
	return r, chat, ov
}

func TestSoCommandWithGame(t *testing.T) {
	r, chat, ov := soRouter(t, "Elden Ring", "https://cdn/x.png")
	// resolveTarget has no store here, so it resolves via the fake resolver.
	r.resolver = fakeResolver{users: map[string]struct{ id, login, display string }{
		"cool": {"id-cool", "cool", "CoolStreamer"},
	}}

	r.Handle(emsg("mod1", "!so @cool", true))

	last := lastSend(chat)
	if !strings.Contains(last, "Go show @CoolStreamer some love") ||
		!strings.Contains(last, "last streaming Elden Ring") ||
		!strings.Contains(last, "twitch.tv/cool") {
		t.Fatalf("chat=%q", last)
	}
	p, ok := ov.last("notify")
	if !ok {
		t.Fatal("no overlay notify")
	}
	d := p.data.(notifyData)
	if d.Kind != "shoutout" || d.Line1 != "Show @CoolStreamer some love" ||
		d.Line2 != "Last streaming Elden Ring" || d.Avatar != "https://cdn/x.png" {
		t.Fatalf("notify=%+v", d)
	}
}

func TestSoCommandNoGameDropsClause(t *testing.T) {
	r, chat, ov := soRouter(t, "", "")
	r.resolver = fakeResolver{users: map[string]struct{ id, login, display string }{
		"pal": {"id-pal", "pal", "Pal"},
	}}

	r.Handle(emsg("mod1", "!so pal", true))

	last := lastSend(chat)
	if strings.Contains(last, "last streaming") {
		t.Fatalf("game clause should be dropped: %q", last)
	}
	if !strings.Contains(last, "Go show @Pal some love") || !strings.Contains(last, "twitch.tv/pal") {
		t.Fatalf("chat=%q", last)
	}
	d, _ := ov.last("notify")
	if nd := d.data.(notifyData); nd.Line2 != "" {
		t.Fatalf("line2 should be empty without a game; got %q", nd.Line2)
	}
}

func TestSoCommandRequiresMod(t *testing.T) {
	r, chat, _ := soRouter(t, "", "")
	r.resolver = fakeResolver{users: map[string]struct{ id, login, display string }{
		"pal": {"id-pal", "pal", "Pal"},
	}}
	r.Handle(emsg("rando", "!so @pal", false)) // not a mod
	if len(chat.sends) != 0 {
		t.Fatalf("non-mod should not shout; sends=%v", chat.sends)
	}
}

func TestAutoShoutoutOncePerSession(t *testing.T) {
	r, chat, _ := soRouter(t, "Halo", "")
	r.shoutAllow = map[string]bool{"friend": true}
	r.sessionLive.Store(true)

	r.Handle(emsg("friend", "hello chat", false))
	r.Handle(emsg("friend", "another message", false)) // no repeat
	if n := len(chat.sends); n != 1 {
		t.Fatalf("auto-shoutout sent %d times, want 1 per session", n)
	}

	// New session → eligible again.
	r.resetShoutSession()
	r.Handle(emsg("friend", "back again", false))
	if n := len(chat.sends); n != 2 {
		t.Fatalf("after ResetSession sends=%d, want 2", n)
	}
}

func TestAutoShoutoutOnlyWhenLive(t *testing.T) {
	r, chat, _ := soRouter(t, "", "")
	r.shoutAllow = map[string]bool{"friend": true}
	// sessionLive defaults false.
	r.Handle(emsg("friend", "hi", false))
	if len(chat.sends) != 0 {
		t.Fatalf("should not shout while offline; sends=%v", chat.sends)
	}
}

func TestAutoShoutoutIgnoresNonListed(t *testing.T) {
	r, chat, _ := soRouter(t, "", "")
	r.shoutAllow = map[string]bool{"friend": true}
	r.sessionLive.Store(true)
	r.Handle(emsg("stranger", "hi", false))
	if len(chat.sends) != 0 {
		t.Fatalf("only allow-listed users get auto-shouted; sends=%v", chat.sends)
	}
}
