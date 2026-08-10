package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Chat-plays Connections, bot-owned. One shared 4x4 board per round: anyone opens
// a round with !connections (when idle) and everyone submits a group of four by
// tile number with "!group 1 5 9 12" — first valid group wins it (like Wordle's
// !guess, no voting). Each tile carries a fixed number 1-16 glued to its word for
// the whole round (a mod !shuffle re-lays screen positions but never renumbers).
// A correct group locks and pays its solver; four shared mistakes or the round
// timer ends it (remaining groups revealed). The bot owns all state, persists it
// (survives a restart), and pushes a render payload to the overlay — which never
// receives the grouping of *unsolved* tiles, so it can't leak the answer.
//
// Only one board game (Wordle or Connections) runs at a time; see board.go.

const (
	connTiles           = 16
	connGroupCount      = 4
	connGroupSize       = 4
	connMaxMistakes     = 10
	connSettingKey      = "connections_round"
	connResultLinger    = 12 * time.Second
	connDefaultDuration = 3 * time.Minute
	// levelEmoji indexes 0..3 → yellow/green/blue/purple.
)

var connLevelEmoji = [4]string{"🟨", "🟩", "🟦", "🟪"}

// connLevel keeps a group's difficulty inside the color table. Levels are
// normalized when the corpus is parsed (see normalizeLevels), so this only
// catches a round restored from a store written before that normalization.
func connLevel(level int) int {
	if level < 0 || level >= len(connLevelEmoji) {
		return 0
	}
	return level
}

// connectionsState is a round's full state (internal; the overlay gets a filtered
// payload built by buildConnPayload, and persistence uses connRec — neither
// exposes the grouping of unsolved tiles).
type connectionsState struct {
	Puzzle    *connPuzzle     // the answer key
	RoomID    string          // channel to announce into when the round auto-expires
	Words     []string        // Words[n-1] = the word on tile number n (1..16); glued for the round
	Order     []int           // display order of the *unsolved* tile numbers; !shuffle permutes
	SolvedIdx []int           // Puzzle.Groups indices solved, in solve order
	Mistakes  int             // wrong guesses so far (max connMaxMistakes)
	Done      bool            // solved, busted, timed out, revealed, or skipped
	Won       bool            // all four groups solved
	EndsAt    int64           // unix millis; drives the overlay countdown
	Tried     map[string]bool // canonical keys of wrong guesses already made (repeats ignored)
}

// active reports whether this round still occupies the stage (not yet cleared).
func (st *connectionsState) active() bool { return st != nil && !st.Done }

// connectionsDuration is the configured round length, falling back to the default
// when the economy (which carries the config) isn't loaded.
func (r *Router) connectionsDuration() time.Duration {
	if r.econ.ConnectionsDuration > 0 {
		return r.econ.ConnectionsDuration
	}
	return connDefaultDuration
}

// loadConnections loads the puzzle bank (synced file → embedded seed) and restores
// an in-progress round from the store. Called once at startup.
func (r *Router) loadConnections(path string) {
	puzzles, source := loadConnectionsPuzzles(path)
	r.connPuzzles = puzzles
	r.logger.Printf("connections: %d puzzles loaded (%s)", len(puzzles), source)
	r.restoreConnections()
}

// startConnections opens a new round when idle (!connections). Anyone can start.
func (r *Router) startConnections(m ChatMessage) {
	if r.chat == nil {
		r.logger.Printf("!connections: replies not configured — run 'mise run bot:auth'")
		return
	}
	if len(r.connPuzzles) == 0 {
		r.reply(m, "no Connections puzzles loaded — run 'mise run connections:sync'.")
		return
	}
	// Claim the shared stage; refuse if the other board game (Wordle) owns it.
	if ok, live := r.claimBoard(boardConnections); !ok {
		r.reply(m, boardBusyMsg(live))
		return
	}

	r.connMu.Lock()
	if r.conn != nil && !r.conn.Done {
		r.connMu.Unlock()
		r.reply(m, "a Connections round is already going — !group <4 numbers>.")
		return
	}

	puzzle := &r.connPuzzles[r.randIntn(len(r.connPuzzles))]
	// Collect the 16 words and shuffle their assignment to tile numbers 1..16.
	words := make([]string, 0, connTiles)
	for _, g := range puzzle.Groups {
		words = append(words, g.Words...)
	}
	r.randShuffle(len(words), func(i, j int) { words[i], words[j] = words[j], words[i] })
	order := make([]int, connTiles)
	for i := range order {
		order[i] = i + 1
	}

	dur := r.connectionsDuration()
	st := &connectionsState{
		Puzzle:   puzzle,
		RoomID:   m.RoomID,
		Words:    words,
		Order:    order,
		Mistakes: 0,
		EndsAt:   time.Now().Add(dur).UnixMilli(),
		Tried:    map[string]bool{},
	}
	r.conn = st
	r.persistConnections(st)
	r.connMu.Unlock()

	time.AfterFunc(dur, func() { r.expireConnections(st) })
	r.pushConnections(st)
	r.chat.Send(m.RoomID, fmt.Sprintf("🧩 Connections! Sort the 16 tiles into 4 groups: !group 1 5 9 12 — %d mistakes allowed, %s on the clock.",
		connMaxMistakes, shortDuration(dur)))
}

// submitGroup evaluates a group submission (!group <4 tile numbers>).
func (r *Router) submitGroup(rest string, m ChatMessage) {
	if r.chat == nil {
		return
	}
	nums, ok := parseGroupNums(rest)
	if !ok {
		r.reply(m, "submit four tile numbers, e.g. !group 1 5 9 12.")
		return
	}

	r.connMu.Lock()
	st := r.conn
	if st == nil || st.Done {
		r.connMu.Unlock()
		r.reply(m, "no Connections round — start one with !connections.")
		return
	}

	// Every number must be an unsolved tile on the board.
	onBoard := make(map[int]bool, len(st.Order))
	for _, n := range st.Order {
		onBoard[n] = true
	}
	for _, n := range nums {
		if !onBoard[n] {
			r.connMu.Unlock()
			r.reply(m, fmt.Sprintf("@%s tile %d isn't on the board.", displayName(m), n))
			return
		}
	}

	words := make([]string, len(nums))
	for i, n := range nums {
		words[i] = st.Words[n-1]
	}
	gIdx, correct := st.matchGroup(words)

	if correct {
		st.SolvedIdx = append(st.SolvedIdx, gIdx)
		st.removeFromBoard(nums)
		group := st.Puzzle.Groups[gIdx]
		complete := len(st.SolvedIdx) == connGroupCount
		if complete {
			st.Done = true
			st.Won = true
		}
		r.persistConnections(st)
		r.connMu.Unlock()

		r.pushConnections(st)
		r.announceGroup(m, group, nums)
		if complete {
			r.awardConnectionsComplete(m)
			r.scheduleConnectionsClear(st)
		}
		return
	}

	// Wrong guess. Exact repeats are ignored (no life lost).
	key := groupKey(nums)
	if st.Tried[key] {
		r.connMu.Unlock()
		return
	}
	st.Tried[key] = true
	st.Mistakes++
	oneAway := st.oneAway(words)
	busted := st.Mistakes >= connMaxMistakes
	if busted {
		st.Done = true
		st.Won = false
	}
	r.persistConnections(st)
	r.connMu.Unlock()

	r.pushConnections(st)
	left := connMaxMistakes - st.Mistakes
	switch {
	case busted:
		r.chat.Send(m.RoomID, "💥 Out of mistakes — here are the groups. !connections to play again.")
		r.scheduleConnectionsClear(st)
	case oneAway:
		r.reply(m, fmt.Sprintf("❗ one away… (%s left)", plural(left, "mistake")))
	default:
		r.reply(m, fmt.Sprintf("❌ not a group (%s left)", plural(left, "mistake")))
	}
}

// matchGroup reports the puzzle-group index the four words all belong to, if they
// form a complete unsolved group. Since groups are size four and the words are
// distinct, "all four share a group" means they ARE that group.
func (st *connectionsState) matchGroup(words []string) (idx int, ok bool) {
	first := st.groupOf(words[0])
	if first < 0 {
		return 0, false
	}
	for _, w := range words[1:] {
		if st.groupOf(w) != first {
			return 0, false
		}
	}
	for _, s := range st.SolvedIdx {
		if s == first {
			return 0, false // already solved
		}
	}
	return first, true
}

// oneAway reports whether the four words are three-quarters of any single
// unsolved group (the NYT "one away…" hint).
func (st *connectionsState) oneAway(words []string) bool {
	counts := make(map[int]int)
	for _, w := range words {
		if g := st.groupOf(w); g >= 0 && !st.isSolved(g) {
			counts[g]++
		}
	}
	for _, c := range counts {
		if c == connGroupSize-1 {
			return true
		}
	}
	return false
}

// groupOf returns the puzzle-group index that owns word, or -1.
func (st *connectionsState) groupOf(word string) int {
	for i, g := range st.Puzzle.Groups {
		for _, w := range g.Words {
			if w == word {
				return i
			}
		}
	}
	return -1
}

func (st *connectionsState) isSolved(idx int) bool {
	for _, s := range st.SolvedIdx {
		if s == idx {
			return true
		}
	}
	return false
}

// removeFromBoard drops the given tile numbers from the display order.
func (st *connectionsState) removeFromBoard(nums []int) {
	drop := make(map[int]bool, len(nums))
	for _, n := range nums {
		drop[n] = true
	}
	kept := st.Order[:0]
	for _, n := range st.Order {
		if !drop[n] {
			kept = append(kept, n)
		}
	}
	st.Order = kept
}

// announceGroup posts a solved-group line and pays the solver a per-color reward.
func (r *Router) announceGroup(m ChatMessage, g connGroup, nums []int) {
	level := connLevel(g.Level)
	reward := r.econ.ConnectionsGroupReward * int64(level+1)
	emoji := connLevelEmoji[level]
	tiles := numsString(nums)
	if r.economy && reward > 0 {
		if _, err := r.store.Credit(m.UserID, reward, "connections_group", ""); err != nil {
			r.logger.Printf("connections group reward %s: %v", m.User, err)
		}
		r.chat.Send(m.RoomID, fmt.Sprintf("%s @%s got %s (%s) — +%s %s.",
			emoji, displayName(m), g.Name, tiles, comma(reward), r.econ.CurrencyName))
		return
	}
	r.chat.Send(m.RoomID, fmt.Sprintf("%s @%s got %s (%s).", emoji, displayName(m), g.Name, tiles))
}

// awardConnectionsComplete pays the completion bonus and bumps the win tally for
// whoever locked the final group.
func (r *Router) awardConnectionsComplete(m ChatMessage) {
	wins, err := r.store.ConnectionsAddWin(m.UserID, m.User, displayName(m))
	if err != nil {
		r.logger.Printf("connections win tally %s: %v", m.User, err)
	}
	bonus := r.econ.ConnectionsSolveBonus
	if r.economy && bonus > 0 {
		if _, err := r.store.Credit(m.UserID, bonus, "connections_solve", ""); err != nil {
			r.logger.Printf("connections solve bonus %s: %v", m.User, err)
		}
		r.chat.Send(m.RoomID, fmt.Sprintf("⭐ @%s completed the puzzle! +%s %s (win #%d).",
			displayName(m), comma(bonus), r.econ.CurrencyName, wins))
		return
	}
	r.chat.Send(m.RoomID, fmt.Sprintf("⭐ @%s completed the puzzle! (win #%d)", displayName(m), wins))
}

// expireConnections ends the round when its timer fires — a loss that reveals the
// remaining groups. No-op if the round was superseded or already finished.
func (r *Router) expireConnections(st *connectionsState) {
	r.connMu.Lock()
	if r.conn != st || st.Done {
		r.connMu.Unlock()
		return
	}
	st.Done = true
	st.Won = false
	roomID := st.RoomID
	r.persistConnections(st)
	r.connMu.Unlock()

	r.pushConnections(st)
	if r.chat != nil {
		r.chat.Send(roomID, "⏱ Time's up — here are the groups. !connections to play again.")
	}
	r.scheduleConnectionsClear(st)
}

// revealConnections (mod !reveal) gives up the current round, revealing all groups.
func (r *Router) revealConnections(m ChatMessage) {
	if !(m.IsMod || m.IsBroadcaster) {
		return
	}
	r.connMu.Lock()
	st := r.conn
	if st == nil || st.Done {
		r.connMu.Unlock()
		r.reply(m, "no Connections round to reveal.")
		return
	}
	st.Done = true
	st.Won = false
	roomID := st.RoomID
	r.persistConnections(st)
	r.connMu.Unlock()

	r.pushConnections(st)
	if r.chat != nil {
		r.chat.Send(roomID, "🔎 Revealed — here are the groups. !connections to play again.")
	}
	r.scheduleConnectionsClear(st)
}

// forceEndConnections clears the current round immediately (mod !skipgame) and
// frees the stage right away — no reveal, no linger — so another game can start at
// once. (The graceful "here are the groups" ending is what !reveal / a natural
// loss does.)
func (r *Router) forceEndConnections() {
	r.connMu.Lock()
	st := r.conn
	r.conn = nil
	r.clearConnectionsPersist()
	r.connMu.Unlock()

	r.releaseBoard(boardConnections)
	r.pushConnectionsHidden()
	if st != nil && r.chat != nil {
		r.chat.Send(st.RoomID, "🛑 Connections skipped. !connections to play again.")
	}
}

// shuffleConnections (mod !shuffle) re-lays the remaining tiles' screen positions.
// Numbers stay glued to their words, so in-flight submissions remain valid.
func (r *Router) shuffleConnections(m ChatMessage) {
	if !(m.IsMod || m.IsBroadcaster) {
		return
	}
	r.connMu.Lock()
	st := r.conn
	if st == nil || st.Done {
		r.connMu.Unlock()
		r.reply(m, "no Connections round to shuffle.")
		return
	}
	r.randShuffle(len(st.Order), func(i, j int) { st.Order[i], st.Order[j] = st.Order[j], st.Order[i] })
	r.persistConnections(st)
	r.connMu.Unlock()
	r.pushConnections(st)
}

// scheduleConnectionsClear hides the finished board after a linger, drops the
// round, and frees the stage — but only if it's still the current round.
func (r *Router) scheduleConnectionsClear(done *connectionsState) {
	time.AfterFunc(connResultLinger, func() {
		r.connMu.Lock()
		if r.conn != done {
			r.connMu.Unlock()
			return
		}
		r.conn = nil
		r.clearConnectionsPersist()
		r.connMu.Unlock()
		r.releaseBoard(boardConnections)
		r.pushConnectionsHidden()
	})
}

// showConnectionsWins replies with the top completers (!connectionswins).
func (r *Router) showConnectionsWins(m ChatMessage) {
	if !(m.IsMod || m.IsBroadcaster) && !r.cooldown.Allow(m.User) {
		return
	}
	lb, err := r.store.ConnectionsLeaderboard(10)
	if err != nil {
		r.logger.Printf("connectionswins: %v", err)
		return
	}
	if len(lb) == 0 {
		r.reply(m, "No Connections completions yet — !connections to start a round.")
		return
	}
	parts := make([]string, len(lb))
	for i, w := range lb {
		parts[i] = fmt.Sprintf("%d. %s %d", i+1, w.Display, w.Wins)
	}
	r.reply(m, "Top Connections solvers: "+strings.Join(parts, "  "))
}

// --- overlay payload --------------------------------------------------------

type connTilePayload struct {
	Num  int    `json:"num"`
	Word string `json:"word"`
}

type connBarPayload struct {
	Name     string   `json:"name"`
	Level    int      `json:"level"`
	Words    []string `json:"words"`
	Revealed bool     `json:"revealed,omitempty"` // true for groups exposed on a loss (not solved)
}

// connPayload is what the overlay renders. It never includes the grouping of
// unsolved tiles — only the visible words and the already-solved (or, once done,
// revealed) groups — so a page reload can't leak the answer mid-round.
type connPayload struct {
	Tiles    []connTilePayload `json:"tiles"`  // unsolved tiles, in display order
	Solved   []connBarPayload  `json:"solved"` // solved groups (+ revealed groups when done)
	Mistakes int               `json:"mistakes"`
	Max      int               `json:"max"`
	EndsAt   int64             `json:"endsAt,omitempty"`
	Done     bool              `json:"done"`
	Won      bool              `json:"won"`
}

// buildConnPayload projects the round into the safe overlay payload.
func (st *connectionsState) buildConnPayload() connPayload {
	// A finished board shows only the four category bars (below) — clear any
	// leftover tiles so a loss doesn't render the grid under/around the reveal.
	var tiles []connTilePayload
	if !st.Done {
		tiles = make([]connTilePayload, 0, len(st.Order))
		for _, n := range st.Order {
			tiles = append(tiles, connTilePayload{Num: n, Word: st.Words[n-1]})
		}
	}
	solved := make([]connBarPayload, 0, connGroupCount)
	for _, idx := range st.SolvedIdx {
		g := st.Puzzle.Groups[idx]
		solved = append(solved, connBarPayload{Name: g.Name, Level: g.Level, Words: g.Words})
	}
	if st.Done { // reveal the unsolved groups as bars too (loss reveal)
		for i, g := range st.Puzzle.Groups {
			if !st.isSolved(i) {
				solved = append(solved, connBarPayload{Name: g.Name, Level: g.Level, Words: g.Words, Revealed: true})
			}
		}
	}
	p := connPayload{
		Tiles:    tiles,
		Solved:   solved,
		Mistakes: st.Mistakes,
		Max:      connMaxMistakes,
		Done:     st.Done,
		Won:      st.Won,
	}
	if !st.Done {
		p.EndsAt = st.EndsAt
	}
	return p
}

func (r *Router) pushConnections(st *connectionsState) {
	if r.overlay != nil {
		r.overlay.Push("connections", st.buildConnPayload())
	}
}

func (r *Router) pushConnectionsHidden() {
	if r.overlay != nil {
		r.overlay.Push("connections", map[string]any{"hidden": true})
	}
}

// --- persistence ------------------------------------------------------------

// connRec is the persisted round (settings row). It carries the full puzzle so a
// restored round is self-contained even if the corpus file changes.
type connRec struct {
	Puzzle    *connPuzzle `json:"puzzle"`
	RoomID    string      `json:"roomID"`
	Words     []string    `json:"words"`
	Order     []int       `json:"order"`
	SolvedIdx []int       `json:"solvedIdx"`
	Mistakes  int         `json:"mistakes"`
	EndsAt    int64       `json:"endsAt"`
	Tried     []string    `json:"tried"`
}

func (r *Router) persistConnections(st *connectionsState) {
	if r.store == nil {
		return
	}
	tried := make([]string, 0, len(st.Tried))
	for k := range st.Tried {
		tried = append(tried, k)
	}
	rec := connRec{
		Puzzle: st.Puzzle, RoomID: st.RoomID, Words: st.Words, Order: st.Order,
		SolvedIdx: st.SolvedIdx, Mistakes: st.Mistakes, EndsAt: st.EndsAt, Tried: tried,
	}
	b, err := json.Marshal(rec)
	if err != nil {
		r.logger.Printf("connections persist marshal: %v", err)
		return
	}
	if err := r.store.SetSetting(connSettingKey, string(b)); err != nil {
		r.logger.Printf("connections persist: %v", err)
	}
}

func (r *Router) clearConnectionsPersist() {
	if r.store != nil {
		if err := r.store.SetSetting(connSettingKey, ""); err != nil {
			r.logger.Printf("connections clear persist: %v", err)
		}
	}
}

// restoreConnections restores an in-progress round on startup and re-pushes it to
// the overlay. A finished/absent round leaves the board idle.
func (r *Router) restoreConnections() {
	if r.store == nil {
		return
	}
	v, ok, err := r.store.GetSetting(connSettingKey)
	if err != nil {
		r.logger.Printf("connections load: %v", err)
		return
	}
	if !ok || v == "" {
		return
	}
	var rec connRec
	if err := json.Unmarshal([]byte(v), &rec); err != nil {
		r.logger.Printf("connections load unmarshal: %v", err)
		return
	}
	if rec.Puzzle == nil || len(rec.Words) != connTiles {
		r.clearConnectionsPersist()
		return
	}
	normalizeLevels(rec.Puzzle) // a round persisted before levels were normalized
	tried := make(map[string]bool, len(rec.Tried))
	for _, k := range rec.Tried {
		tried[k] = true
	}
	st := &connectionsState{
		Puzzle: rec.Puzzle, RoomID: rec.RoomID, Words: rec.Words, Order: rec.Order,
		SolvedIdx: rec.SolvedIdx, Mistakes: rec.Mistakes, EndsAt: rec.EndsAt, Tried: tried,
	}
	r.conn = st
	if ok, _ := r.claimBoard(boardConnections); !ok {
		// Another board somehow already holds the stage; don't double-occupy.
		r.conn = nil
		return
	}
	r.pushConnections(st)

	// Reschedule the round timer for whatever time is left.
	if st.EndsAt > 0 {
		remaining := time.Until(time.UnixMilli(st.EndsAt))
		if remaining <= 0 {
			go r.expireConnections(st)
		} else {
			time.AfterFunc(remaining, func() { r.expireConnections(st) })
		}
	}
}

// --- small helpers ----------------------------------------------------------

// parseGroupNums parses exactly four distinct tile numbers (1..16) from rest,
// accepting spaces and/or commas as separators ("1 5 9 12", "1,5,9,12").
func parseGroupNums(rest string) ([]int, bool) {
	fields := strings.FieldsFunc(rest, func(r rune) bool {
		return r == ' ' || r == ',' || r == '\t'
	})
	if len(fields) != connGroupSize {
		return nil, false
	}
	seen := make(map[int]bool, connGroupSize)
	nums := make([]int, 0, connGroupSize)
	for _, f := range fields {
		n, err := strconv.Atoi(f)
		if err != nil || n < 1 || n > connTiles || seen[n] {
			return nil, false
		}
		seen[n] = true
		nums = append(nums, n)
	}
	return nums, true
}

// groupKey is a canonical key for a set of tile numbers (sorted, comma-joined),
// so repeated wrong guesses in any order are recognized as the same.
func groupKey(nums []int) string {
	s := append([]int(nil), nums...)
	sort.Ints(s)
	return numsString(s)
}

// numsString joins tile numbers with spaces, in the given order.
func numsString(nums []int) string {
	parts := make([]string, len(nums))
	for i, n := range nums {
		parts[i] = strconv.Itoa(n)
	}
	return strings.Join(parts, " ")
}
