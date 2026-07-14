package main

import (
	"context"
	"strings"
	"testing"
)

// fakeResolver stands in for Helix Get Users.
type fakeResolver struct {
	users map[string]struct{ id, login, display string } // keyed by login
}

func (f fakeResolver) LookupUser(_ context.Context, login string) (string, string, string, bool, error) {
	u, ok := f.users[login]
	if !ok {
		return "", "", "", false, nil
	}
	return u.id, u.login, u.display, true, nil
}

// bcast makes a broadcaster message (IsBroadcaster true).
func bcast(text string) ChatMessage {
	m := emsg("chan", text, true) // user=="chan" → IsBroadcaster in msg()
	m.UserID = "id-chan"
	return m
}

func TestGrantMintsToSeenUser(t *testing.T) {
	r, _, st, chat := econRouter(t)
	st.UpsertUser("id-bob", "bob", "Bob")

	r.Handle(bcast("!grant @bob 500"))
	if b, _ := st.Balance("id-bob"); b != 500 {
		t.Fatalf("bob=%d want 500", b)
	}
	if !strings.Contains(lastReply(chat), "@bob now has 500 marks") {
		t.Fatalf("reply=%q", lastReply(chat))
	}
}

func TestGrantRemovesClampedToZero(t *testing.T) {
	r, _, st, chat := econRouter(t)
	st.UpsertUser("id-bob", "bob", "Bob")
	st.Credit("id-bob", 120, "accrual", "")

	r.Handle(bcast("!grant @bob -100000"))
	if b, _ := st.Balance("id-bob"); b != 0 {
		t.Fatalf("bob=%d want 0 (clamped)", b)
	}
	if !strings.Contains(lastReply(chat), "removed") || !strings.Contains(lastReply(chat), "now has 0 marks") {
		t.Fatalf("reply=%q", lastReply(chat))
	}
}

func TestGrantResolvesUnseenViaResolver(t *testing.T) {
	r, _, st, _ := econRouter(t)
	r.resolver = fakeResolver{users: map[string]struct{ id, login, display string }{
		"newbie": {"id-new", "newbie", "NewBie"},
	}}

	r.Handle(bcast("!grant @newbie 300"))
	if b, _ := st.Balance("id-new"); b != 300 {
		t.Fatalf("newbie=%d want 300", b)
	}
	// Recorded, so the leaderboard shows them.
	lb, _ := st.Leaderboard(10)
	if len(lb) != 1 || lb[0].Display != "NewBie" {
		t.Fatalf("leaderboard=%+v want NewBie", lb)
	}
}

func TestGrantUnknownLogin(t *testing.T) {
	r, _, st, chat := econRouter(t)
	r.resolver = fakeResolver{} // resolves nobody

	r.Handle(bcast("!grant @ghost 300"))
	if lastReply(chat) != "no such user @ghost." {
		t.Fatalf("reply=%q want no-such-user", lastReply(chat))
	}
	if b, _ := st.Balance("id-ghost"); b != 0 {
		t.Fatalf("ghost balance=%d want 0", b)
	}
}

func TestGrantIsBroadcasterOnly(t *testing.T) {
	r, _, st, chat := econRouter(t)
	st.UpsertUser("id-bob", "bob", "Bob")

	r.Handle(emsg("mod1", "!grant @bob 500", true)) // mod, not broadcaster
	if b, _ := st.Balance("id-bob"); b != 0 {
		t.Fatalf("bob=%d want 0 (mod can't grant)", b)
	}
	if len(chat.replies) != 0 {
		t.Fatalf("expected no reply; got %v", chat.replies)
	}
}

func TestFreeModeWaivesTTSButKeepsAccrualAndGames(t *testing.T) {
	r, f, st, chat := econRouter(t)
	// bob is a mod (cooldown-exempt) so multiple actions in one test don't collide
	// on the per-user cooldown; mods still PAY, so this exercises the cost logic.
	bob := func(text string) ChatMessage { return emsg("bob", text, true) }

	// Turn free mode on (broadcaster).
	r.Handle(bcast("!free"))
	if r.charging {
		t.Fatal("charging should be off after !free")
	}
	if v, _, _ := st.GetSetting("charge_mode"); v != "free" {
		t.Fatalf("charge_mode=%q want free (persisted)", v)
	}

	// Broke bob can now !tts for free.
	r.Handle(bob("!tts hi"))
	if len(f.says) != 1 {
		t.Fatalf("says=%d want 1 (free mode)", len(f.says))
	}
	if b, _ := st.Balance("id-bob"); b != 0 {
		t.Fatalf("bob charged in free mode: balance=%d want 0", b)
	}

	// Games still use real marks: gamble with 0 balance is rejected.
	r.Handle(bob("!gamble 10"))
	if !strings.Contains(lastReply(chat), "you only have 0 marks") {
		t.Fatalf("gamble reply=%q — games should still cost", lastReply(chat))
	}

	// Back to paid: now the broke !tts is refused and not spoken.
	r.Handle(bcast("!paid"))
	if !r.charging {
		t.Fatal("charging should be on after !paid")
	}
	if v, _, _ := st.GetSetting("charge_mode"); v != "paid" {
		t.Fatalf("charge_mode=%q want paid", v)
	}
	f.says = nil
	r.Handle(bob("!tts hi"))
	if len(f.says) != 0 {
		t.Fatalf("says=%d want 0 (paid, broke)", len(f.says))
	}
}

func TestFreePaidBroadcasterOnly(t *testing.T) {
	r, _, _, _ := econRouter(t)
	r.Handle(emsg("mod1", "!free", true)) // mod, not broadcaster
	if !r.charging {
		t.Fatal("a mod should not be able to toggle free mode")
	}
}
