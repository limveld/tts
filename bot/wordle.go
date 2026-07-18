package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Chat-plays Wordle, bot-owned. One shared 6-row board per round: anyone opens a
// round with !wordle (when idle) and everyone submits guesses with "!guess
// <word>" — unlimited guesses per user. A correct guess or six misses ends the
// round; the solver earns marks and a win tally. The bot computes and persists
// the board and pushes it to the overlay, which renders the familiar
// tiles/keyboard. Rendering is ported from raw/wordle-chat-overlay.html; the game
// logic lives here (the prototype's per-viewer voting is dropped by design).

//go:embed wordle_answers.txt
var wordleAnswersRaw string

//go:embed wordle_valid.txt
var wordleValidRaw string

var (
	wordleAnswers = splitWords(wordleAnswersRaw) // the answer pool
	wordleValid   = wordSet(wordleValidRaw)      // accepted guesses (incl. answers)
)

const (
	wordleRows            = 6
	wordleCols            = 5
	wordleSettingKey      = "wordle_round"
	wordleResultLinger    = 12 * time.Second // how long the solved/failed board lingers before it's cleared
	wordleDefaultDuration = 3 * time.Minute  // fallback round length when unset (economy off / no points.toml)
)

// wordleRowState is one played row: the guess and its per-letter scoring
// ("correct"|"present"|"absent").
type wordleRowState struct {
	Guess  string   `json:"guess"`
	Result []string `json:"result"`
}

// wordleState is a round's full state. It's persisted as JSON (settings row) so a
// round survives a bot restart, and pushed to the overlay for rendering. Answer
// is included in the payload only after the round is Done (so a page reload
// mid-round can't leak it via devtools).
type wordleState struct {
	Answer string           `json:"-"`
	RoomID string           `json:"-"` // channel to announce into when the round auto-expires
	Rows   []wordleRowState `json:"rows"`
	Done   bool             `json:"done"`
	Won    bool             `json:"won"`
	Max    int              `json:"max"`
	EndsAt int64            `json:"endsAt,omitempty"` // unix millis; drives the overlay countdown
	Reveal string           `json:"answer,omitempty"` // set to Answer only when Done, for the overlay banner
}

// wordleDuration is the configured round length, falling back to the default
// when the economy (which carries the config) isn't loaded.
func (r *Router) wordleDuration() time.Duration {
	if r.econ.WordleDuration > 0 {
		return r.econ.WordleDuration
	}
	return wordleDefaultDuration
}

func splitWords(raw string) []string {
	var out []string
	for _, w := range strings.Fields(raw) {
		out = append(out, strings.ToUpper(w))
	}
	return out
}

func wordSet(raw string) map[string]bool {
	m := make(map[string]bool)
	for _, w := range strings.Fields(raw) {
		m[strings.ToUpper(w)] = true
	}
	return m
}

// scoreWordle scores guess against answer with correct duplicate-letter handling:
// exact-position matches ("correct") are marked first and consume that answer
// slot, then remaining letters match unused slots ("present"), else "absent".
func scoreWordle(guess, answer string) []string {
	result := make([]string, wordleCols)
	g := []byte(guess)
	a := []byte(answer)
	used := make([]bool, wordleCols)
	for i := 0; i < wordleCols; i++ {
		result[i] = "absent"
	}
	for i := 0; i < wordleCols; i++ { // greens first
		if g[i] == a[i] {
			result[i] = "correct"
			used[i] = true
			g[i] = 0
		}
	}
	for i := 0; i < wordleCols; i++ { // then yellows from remaining slots
		if g[i] == 0 {
			continue
		}
		for j := 0; j < wordleCols; j++ {
			if !used[j] && a[j] == g[i] {
				result[i] = "present"
				used[j] = true
				break
			}
		}
	}
	return result
}

func wordleSolved(result []string) bool {
	for _, r := range result {
		if r != "correct" {
			return false
		}
	}
	return true
}

// startWordle opens a new round when idle (!wordle). Anyone can start.
func (r *Router) startWordle(m ChatMessage) {
	if r.chat == nil {
		r.logger.Printf("!wordle: replies not configured — run 'mise run bot:auth'")
		return
	}
	if len(wordleAnswers) == 0 {
		return
	}
	r.wordleMu.Lock()
	if r.wordle != nil && !r.wordle.Done {
		r.wordleMu.Unlock()
		r.reply(m, "a Wordle round is already going — !guess <word> (5 letters).")
		return
	}
	dur := r.wordleDuration()
	st := &wordleState{
		Answer: wordleAnswers[r.rnd.Intn(len(wordleAnswers))],
		RoomID: m.RoomID,
		Rows:   []wordleRowState{},
		Max:    wordleRows,
		EndsAt: time.Now().Add(dur).UnixMilli(),
	}
	r.wordle = st
	r.persistWordle(st)
	r.wordleMu.Unlock()

	time.AfterFunc(dur, func() { r.expireWordle(st) })
	r.pushWordle(st)
	r.chat.Send(m.RoomID, fmt.Sprintf("🟩 Wordle! Everyone guess the 5-letter word: !guess <word> — %d tries, %s on the clock.",
		wordleRows, shortDuration(dur)))
}

// guessWordle submits a guess to the active round (!guess <word>).
func (r *Router) guessWordle(rest string, m ChatMessage) {
	if r.chat == nil {
		return
	}
	word := strings.ToUpper(strings.TrimSpace(rest))
	if f := strings.Fields(word); len(f) > 0 {
		word = f[0]
	}

	r.wordleMu.Lock()
	st := r.wordle
	if st == nil || st.Done {
		r.wordleMu.Unlock()
		r.reply(m, "no Wordle round — start one with !wordle.")
		return
	}
	if len(word) != wordleCols || !isAlpha(word) {
		r.wordleMu.Unlock()
		r.reply(m, "guesses are 5 letters, e.g. !guess crane.")
		return
	}
	if !wordleValid[word] {
		r.wordleMu.Unlock()
		r.reply(m, fmt.Sprintf("@%s '%s' isn't in the word list.", displayName(m), word))
		return
	}

	result := scoreWordle(word, st.Answer)
	st.Rows = append(st.Rows, wordleRowState{Guess: word, Result: result})
	won := wordleSolved(result)
	lost := !won && len(st.Rows) >= wordleRows
	if won || lost {
		st.Done = true
		st.Won = won
		st.Reveal = st.Answer
	}
	answer, tries := st.Answer, len(st.Rows)
	r.persistWordle(st)
	r.wordleMu.Unlock()

	r.pushWordle(st)

	switch {
	case won:
		r.awardWordle(m, tries)
		r.scheduleWordleClear(st)
	case lost:
		r.chat.Send(m.RoomID, fmt.Sprintf("💀 Out of guesses — the word was %s. !wordle to play again.", answer))
		r.scheduleWordleClear(st)
	}
}

// expireWordle ends the round when its timer fires — a loss that reveals the
// word. No-op if the round was already superseded or finished (solve / 6 misses).
func (r *Router) expireWordle(st *wordleState) {
	r.wordleMu.Lock()
	if r.wordle != st || st.Done {
		r.wordleMu.Unlock()
		return
	}
	st.Done = true
	st.Won = false
	st.Reveal = st.Answer
	answer, roomID := st.Answer, st.RoomID
	r.persistWordle(st)
	r.wordleMu.Unlock()

	r.pushWordle(st)
	if r.chat != nil {
		r.chat.Send(roomID, fmt.Sprintf("⏱ Time's up — the word was %s. !wordle to play again.", answer))
	}
	r.scheduleWordleClear(st)
}

// awardWordle grants the solver marks (when the economy is on) and bumps their
// win tally, then announces it.
func (r *Router) awardWordle(m ChatMessage, tries int) {
	wins, err := r.store.WordleAddWin(m.UserID, m.User, displayName(m))
	if err != nil {
		r.logger.Printf("wordle win tally %s: %v", m.User, err)
	}
	reward := r.econ.WordleReward
	if r.economy && reward > 0 {
		if _, err := r.store.Credit(m.UserID, reward, "wordle_win", ""); err != nil {
			r.logger.Printf("wordle reward %s: %v", m.User, err)
		}
		r.chat.Send(m.RoomID, fmt.Sprintf("🎉 @%s solved the Wordle in %d! +%s %s (win #%d).",
			displayName(m), tries, comma(reward), r.econ.CurrencyName, wins))
		return
	}
	r.chat.Send(m.RoomID, fmt.Sprintf("🎉 @%s solved the Wordle in %d! (win #%d)", displayName(m), tries, wins))
}

// scheduleWordleClear hides the finished board after a linger and drops the
// round, but only if it's still the current one (a new !wordle supersedes it).
func (r *Router) scheduleWordleClear(done *wordleState) {
	time.AfterFunc(wordleResultLinger, func() {
		r.wordleMu.Lock()
		if r.wordle != done {
			r.wordleMu.Unlock()
			return
		}
		r.wordle = nil
		r.clearWordlePersist()
		r.wordleMu.Unlock()
		r.pushWordleHidden()
	})
}

// showWordleWins replies with the top solvers (!wordlewins).
func (r *Router) showWordleWins(m ChatMessage) {
	if !(m.IsMod || m.IsBroadcaster) && !r.cooldown.Allow(m.User) {
		return
	}
	lb, err := r.store.WordleLeaderboard(10)
	if err != nil {
		r.logger.Printf("wordlewins: %v", err)
		return
	}
	if len(lb) == 0 {
		r.reply(m, "No Wordle wins yet — !wordle to start a round.")
		return
	}
	parts := make([]string, len(lb))
	for i, w := range lb {
		parts[i] = fmt.Sprintf("%d. %s %d", i+1, w.Display, w.Wins)
	}
	r.reply(m, "Top Wordle solvers: "+strings.Join(parts, "  "))
}

// --- persistence + overlay push --------------------------------------------

func (r *Router) persistWordle(st *wordleState) {
	if r.store == nil {
		return
	}
	// Persist the answer too (round survives restart), separate from the overlay
	// payload which only reveals it when Done.
	rec := struct {
		Answer string           `json:"answer"`
		RoomID string           `json:"roomID"`
		Rows   []wordleRowState `json:"rows"`
		Done   bool             `json:"done"`
		Won    bool             `json:"won"`
		EndsAt int64            `json:"endsAt"`
	}{st.Answer, st.RoomID, st.Rows, st.Done, st.Won, st.EndsAt}
	b, err := json.Marshal(rec)
	if err != nil {
		r.logger.Printf("wordle persist marshal: %v", err)
		return
	}
	if err := r.store.SetSetting(wordleSettingKey, string(b)); err != nil {
		r.logger.Printf("wordle persist: %v", err)
	}
}

func (r *Router) clearWordlePersist() {
	if r.store != nil {
		if err := r.store.SetSetting(wordleSettingKey, ""); err != nil {
			r.logger.Printf("wordle clear persist: %v", err)
		}
	}
}

func (r *Router) pushWordle(st *wordleState) {
	if r.overlay != nil {
		r.overlay.Push("wordle", st)
	}
}

func (r *Router) pushWordleHidden() {
	if r.overlay != nil {
		r.overlay.Push("wordle", map[string]any{"hidden": true})
	}
}

// loadWordle restores an in-progress round from the store on startup and re-pushes
// it to the overlay. A finished/absent round leaves the board idle.
func (r *Router) loadWordle() {
	if r.store == nil {
		return
	}
	v, ok, err := r.store.GetSetting(wordleSettingKey)
	if err != nil {
		r.logger.Printf("wordle load: %v", err)
		return
	}
	if !ok || v == "" {
		return
	}
	var rec struct {
		Answer string           `json:"answer"`
		RoomID string           `json:"roomID"`
		Rows   []wordleRowState `json:"rows"`
		Done   bool             `json:"done"`
		Won    bool             `json:"won"`
		EndsAt int64            `json:"endsAt"`
	}
	if err := json.Unmarshal([]byte(v), &rec); err != nil {
		r.logger.Printf("wordle load unmarshal: %v", err)
		return
	}
	if rec.Done { // don't resurrect a finished board
		r.clearWordlePersist()
		return
	}
	st := &wordleState{Answer: rec.Answer, RoomID: rec.RoomID, Rows: rec.Rows, Done: rec.Done, Won: rec.Won, Max: wordleRows, EndsAt: rec.EndsAt}
	r.wordle = st
	r.pushWordle(st)

	// Reschedule the round timer for whatever time is left (the AfterFunc from the
	// original start didn't survive the restart).
	if st.EndsAt > 0 {
		remaining := time.Until(time.UnixMilli(st.EndsAt))
		if remaining <= 0 {
			go r.expireWordle(st)
		} else {
			time.AfterFunc(remaining, func() { r.expireWordle(st) })
		}
	}
}

func isAlpha(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] < 'A' || s[i] > 'Z' {
			return false
		}
	}
	return len(s) > 0
}
