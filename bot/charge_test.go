package main

import (
	"math/rand"
	"path/filepath"
	"testing"
	"time"

	"tts/store"
)

// econRouter builds a test router with the marks economy enabled (tts=10, sfx=5)
// backed by a temp store, plus a fake chat for refusal replies.
func econRouter(t *testing.T) (*Router, *fakeTTS, *store.Store, *fakeChat) {
	t.Helper()
	f := &fakeTTS{}
	r := newTestRouter(f)
	st, err := store.Open(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	chat := &fakeChat{}
	r.store = st
	r.chat = chat
	r.economy = true
	r.charging = true // paid mode by default in tests
	r.rnd = rand.New(rand.NewSource(1))
	r.econ = EconomyConfig{
		CurrencyName: "marks", TTSCost: 10, SFXCost: 5,
		GambleMinBet: 10, GambleDuration: 60 * time.Second,
	}
	return r, f, st, chat
}

// emsg is like msg but sets UserID (the economy's key).
func emsg(user, text string, mod bool) ChatMessage {
	m := msg(user, text, mod)
	m.UserID = "id-" + user
	m.RoomID, m.ID = "room1", "msg1"
	return m
}

func TestChargeTTSWhenAffordable(t *testing.T) {
	r, f, st, chat := econRouter(t)
	st.Credit("id-bob", 25, "accrual", "")

	r.Handle(emsg("bob", "!tts hello", false))
	if len(f.says) != 1 {
		t.Fatalf("says=%d want 1 (affordable)", len(f.says))
	}
	if b, _ := st.Balance("id-bob"); b != 15 {
		t.Fatalf("balance=%d want 15 (charged 10)", b)
	}
	if len(chat.replies) != 0 {
		t.Errorf("no refusal expected; replies=%v", chat.replies)
	}
}

func TestBrokeUserRejectedAndNotSpoken(t *testing.T) {
	r, f, st, chat := econRouter(t)
	st.Credit("id-bob", 3, "accrual", "") // < 10

	r.Handle(emsg("bob", "!tts hello", false))
	if len(f.says) != 0 {
		t.Fatalf("says=%d want 0 (too poor)", len(f.says))
	}
	if b, _ := st.Balance("id-bob"); b != 3 {
		t.Fatalf("balance=%d want 3 (unchanged)", b)
	}
	if len(chat.replies) != 1 {
		t.Fatalf("expected a refusal reply; got %v", chat.replies)
	}
}

func TestFailedEffectIsNotCharged(t *testing.T) {
	r, _, st, _ := econRouter(t)
	r.tts = &errTTS{} // Say/SFX return errors
	if _, err := st.Credit("id-bob", 100, "accrual", ""); err != nil {
		t.Fatal(err)
	}

	r.Handle(emsg("bob", "!tts hello", false))
	if b, _ := st.Balance("id-bob"); b != 100 {
		t.Fatalf("balance=%d want 100 (failed effect => no charge)", b)
	}
}

func TestSFXCharged(t *testing.T) {
	r, f, st, _ := econRouter(t)
	st.Credit("id-bob", 20, "accrual", "")

	r.Handle(emsg("bob", "!airhorn", false))
	if len(f.sfx) != 1 {
		t.Fatalf("sfx=%d want 1", len(f.sfx))
	}
	if b, _ := st.Balance("id-bob"); b != 15 {
		t.Fatalf("balance=%d want 15 (charged 5)", b)
	}
}

func TestEconomyDisabledMeansFree(t *testing.T) {
	r, f, st, _ := econRouter(t)
	r.economy = false // disabled: no charge, no balance needed
	// bob has 0 marks.

	r.Handle(emsg("bob", "!tts hello", false))
	if len(f.says) != 1 {
		t.Fatalf("says=%d want 1 (free when economy disabled)", len(f.says))
	}
	if b, _ := st.Balance("id-bob"); b != 0 {
		t.Fatalf("balance=%d want 0 (never charged)", b)
	}
}

func TestModsPayToo(t *testing.T) {
	r, f, _, chat := econRouter(t)
	// mod with no marks: cost still applies (everyone pays).
	r.Handle(emsg("mod1", "!tts hello", true))
	if len(f.says) != 0 {
		t.Fatalf("says=%d want 0 (mod can't afford)", len(f.says))
	}
	if len(chat.replies) != 1 {
		t.Fatalf("mod should get a refusal; replies=%v", chat.replies)
	}
}

func TestMarksBalanceCommand(t *testing.T) {
	r, _, st, chat := econRouter(t)
	st.UpsertUser("id-bob", "bob", "Bob")
	st.Credit("id-bob", 1024, "accrual", "")

	// Caller is a mod so the per-user cooldown doesn't block these back-to-back.
	r.Handle(emsg("bob", "!marks", true))
	if len(chat.replies) != 1 || chat.replies[0].text != "@bob you have 1,024 marks." {
		t.Fatalf("reply=%v want '@bob you have 1,024 marks.'", chat.replies)
	}

	// !m alias, looking up another user.
	st.UpsertUser("id-amy", "amy", "Amy")
	st.Credit("id-amy", 50, "accrual", "")
	r.Handle(emsg("bob", "!m @amy", true))
	last := chat.replies[len(chat.replies)-1].text
	if last != "@amy has 50 marks." {
		t.Fatalf("reply=%q want '@amy has 50 marks.'", last)
	}

	// Unknown user.
	r.Handle(emsg("bob", "!marks @ghost", true))
	last = chat.replies[len(chat.replies)-1].text
	if last != "haven't seen @ghost yet." {
		t.Fatalf("reply=%q want unseen message", last)
	}
}

func TestLeaderboardCommand(t *testing.T) {
	r, _, st, chat := econRouter(t)
	st.UpsertUser("id-bob", "bob", "Bob")
	st.UpsertUser("id-amy", "amy", "Amy")
	st.Credit("id-bob", 100, "accrual", "")
	st.Credit("id-amy", 900, "accrual", "")

	r.Handle(emsg("bob", "!leaderboard", false))
	if len(chat.replies) != 1 || chat.replies[0].text != "Top marks: 1. Amy 900  2. Bob 100" {
		t.Fatalf("reply=%v", chat.replies)
	}
}

// errTTS fails every effect, to test that a failed effect isn't charged.
type errTTS struct{ fakeTTS }

func (e *errTTS) Say(string, string) error { return errSay }
func (e *errTTS) SFX(string) error         { return errSay }

var errSay = &effectError{}

type effectError struct{}

func (*effectError) Error() string { return "effect failed" }
