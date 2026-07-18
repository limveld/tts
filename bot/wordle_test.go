package main

import (
	"strings"
	"testing"
)

func TestScoreWordle(t *testing.T) {
	cases := []struct {
		guess, answer string
		want          string // space-joined per-letter states, c/p/a shorthand
	}{
		{"CRANE", "CRANE", "c c c c c"},
		{"SLATE", "CRANE", "a a c a c"}, // A in pos2 correct, E correct
		{"OZONE", "ROBOT", "p a p a a"}, // two O's: both present (answer has two O's)
		{"OOOOO", "ROBOT", "a c a c a"}, // only two O's in answer -> two greens, rest absent
		{"LEVEL", "EAGER", "a p a c a"}, // pos3 E is a green; the other E is present
	}
	short := map[string]string{"correct": "c", "present": "p", "absent": "a"}
	for _, c := range cases {
		got := scoreWordle(c.guess, c.answer)
		parts := make([]string, len(got))
		for i, s := range got {
			parts[i] = short[s]
		}
		if strings.Join(parts, " ") != c.want {
			t.Errorf("scoreWordle(%q,%q) = %v, want %q", c.guess, c.answer, got, c.want)
		}
	}
}

func TestWordleRoundLifecycle(t *testing.T) {
	r, _, st, chat := econRouter(t)
	ov := &fakeOverlay{}
	r.overlay = ov

	// start
	r.Handle(emsg("alice", "!wordle", false))
	if r.wordle == nil || r.wordle.Done || len(r.wordle.Rows) != 0 {
		t.Fatalf("round not started cleanly: %+v", r.wordle)
	}
	r.wordle.Answer = "ABOUT" // pin the answer (an answer-pool word, in the valid set)
	if _, ok := ov.last("wordle"); !ok {
		t.Fatal("no wordle push on start")
	}

	// a wrong (but valid) guess appends a row, stays active
	r.Handle(emsg("bob", "!guess OTHER", false))
	if len(r.wordle.Rows) != 1 || r.wordle.Done {
		t.Fatalf("after wrong guess: rows=%d done=%v", len(r.wordle.Rows), r.wordle.Done)
	}

	// solving ends the round, credits the reward, tallies the win
	r.Handle(emsg("bob", "!guess ABOUT", false))
	if r.wordle == nil || !r.wordle.Done || !r.wordle.Won {
		t.Fatalf("after solve: %+v", r.wordle)
	}
	if bal, _ := st.Balance("id-bob"); bal != r.econ.WordleReward {
		t.Fatalf("winner balance=%d want reward %d", bal, r.econ.WordleReward)
	}
	wins, _ := st.WordleLeaderboard(10)
	if len(wins) != 1 || wins[0].Wins != 1 {
		t.Fatalf("leaderboard=%+v want one winner with 1 win", wins)
	}
	if !strings.Contains(lastSend(chat), "solved the Wordle") {
		t.Fatalf("no solve announcement; last=%q", lastSend(chat))
	}
	// the solving push carries the reveal + done
	p, _ := ov.last("wordle")
	ws := p.data.(*wordleState)
	if !ws.Done || ws.Reveal != "ABOUT" {
		t.Fatalf("solve push=%+v want done + reveal ABOUT", ws)
	}
}

func TestWordleGuessWithoutRound(t *testing.T) {
	r, _, _, chat := econRouter(t)
	r.Handle(emsg("bob", "!guess crane", false))
	if !strings.Contains(lastReply(chat), "no Wordle round") {
		t.Fatalf("reply=%q want 'no Wordle round'", lastReply(chat))
	}
}

func TestWordleRejectsInvalidGuess(t *testing.T) {
	r, _, _, chat := econRouter(t)
	r.Handle(emsg("alice", "!wordle", false))

	r.Handle(emsg("bob", "!guess abcd", false)) // 4 letters
	if !strings.Contains(lastReply(chat), "5 letters") {
		t.Fatalf("short-word reply=%q", lastReply(chat))
	}
	r.Handle(emsg("bob", "!guess zzzzz", false)) // not in dict
	if !strings.Contains(lastReply(chat), "isn't in the word list") {
		t.Fatalf("bad-word reply=%q", lastReply(chat))
	}
	if len(r.wordle.Rows) != 0 {
		t.Fatalf("invalid guesses should not append rows; rows=%d", len(r.wordle.Rows))
	}
}

func TestWordleSixMissesLoses(t *testing.T) {
	r, _, _, chat := econRouter(t)
	r.Handle(emsg("alice", "!wordle", false))
	r.wordle.Answer = "ABOUT"

	// six valid, non-answer guesses
	misses := []string{"OTHER", "WHICH", "THEIR", "THERE", "FIRST", "WOULD"}
	for _, g := range misses {
		r.Handle(emsg("bob", "!guess "+g, false))
	}
	if r.wordle == nil || !r.wordle.Done || r.wordle.Won {
		t.Fatalf("after 6 misses: %+v want done+not won", r.wordle)
	}
	if !strings.Contains(lastSend(chat), "the word was ABOUT") {
		t.Fatalf("loss announcement=%q", lastSend(chat))
	}
}

func TestWordleTimerExpiresAsLoss(t *testing.T) {
	r, _, _, chat := econRouter(t)
	ov := &fakeOverlay{}
	r.overlay = ov
	r.Handle(emsg("alice", "!wordle", false))
	r.wordle.Answer = "ABOUT"
	st := r.wordle

	r.expireWordle(st) // fire the timer directly (deterministic)

	if r.wordle == nil || !r.wordle.Done || r.wordle.Won {
		t.Fatalf("after expiry: %+v want done+not won", r.wordle)
	}
	if !strings.Contains(lastSend(chat), "Time's up") || !strings.Contains(lastSend(chat), "ABOUT") {
		t.Fatalf("expiry announcement=%q", lastSend(chat))
	}
	p, _ := ov.last("wordle")
	if ws := p.data.(*wordleState); !ws.Done || ws.Reveal != "ABOUT" {
		t.Fatalf("expiry push=%+v want done + reveal ABOUT", ws)
	}
}

func TestWordleExpireNoOpAfterSolve(t *testing.T) {
	r, _, _, _ := econRouter(t)
	r.Handle(emsg("alice", "!wordle", false))
	r.wordle.Answer = "ABOUT"
	st := r.wordle
	r.Handle(emsg("bob", "!guess ABOUT", false)) // solved

	if !st.Won {
		t.Fatal("expected a solved round")
	}
	// The still-pending timer must not overwrite the win.
	r.expireWordle(st)
	if !r.wordle.Won {
		t.Fatalf("expireWordle clobbered a solved round: %+v", r.wordle)
	}
}

func TestWordleStartSetsDeadline(t *testing.T) {
	r, _, _, _ := econRouter(t)
	r.Handle(emsg("alice", "!wordle", false))
	if r.wordle.EndsAt <= 0 {
		t.Fatalf("EndsAt=%d want a future deadline", r.wordle.EndsAt)
	}
}

func TestWordleAlreadyRunning(t *testing.T) {
	r, _, _, chat := econRouter(t)
	r.Handle(emsg("alice", "!wordle", false))
	r.Handle(emsg("bob", "!wordle", false)) // second start rejected
	if !strings.Contains(lastReply(chat), "already going") {
		t.Fatalf("reply=%q want 'already going'", lastReply(chat))
	}
}
