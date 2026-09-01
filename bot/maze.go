package main

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"tts/internal/maze"
	"tts/store"
)

// Torch Maze, bot-owned. Up to five chatters each drive a sprite through a
// fogged maze on a global cycle, scrambling for keys that are deliberately one
// short of the field and racing a locked exit. The rules and the board live in
// internal/maze, which knows nothing about chat; this file is the seam — it owns
// the channel, the tick timer, persistence, the overlay payload and the words.
//
// The engine is not safe for concurrent use, so every touch of it happens under
// mazeMu. The pattern throughout, borrowed from the other games: mutate and
// persist under the lock, snapshot what the outside world needs, unlock, then do
// the chat and overlay I/O. Nothing that can block is done holding the mutex.
//
// See .scratch/torch-maze/PRD.md for why the game is shaped the way it is.

// MazeWins is the win-tally slice of the store the maze needs (an interface so
// tests can substitute a fake). The board itself is not in here: it is persisted
// as an opaque round document. *sqlite.Store and *postgres.Store satisfy it.
type MazeWins interface {
	MazeAddWin(userID, login, display string) (wins int, err error)
	MazeLeaderboard(n int) ([]store.MazeWin, error)
}

// mazeResultLinger is how long a settled board stays up before the stage is
// freed — long enough to read the placements off the overlay.
const mazeResultLinger = 15 * time.Second

// mazeConfig is the whole game's tuning. Defaults live in defaultMazeConfig and
// are what ships; issue 09 replaces that with maze.toml, which is why this is a
// struct rather than a handful of constants.
type mazeConfig struct {
	Tick    time.Duration // cycle length; the single most important pacing knob
	Display string        // "panel" | "full" — how the overlay renders the board
	// Seed fixes the board. Zero draws a fresh one per round, which is what a
	// stream wants; setting it replays one exact maze, for a rematch or for
	// reproducing something that went wrong.
	Seed  int64
	Gen   maze.Config
	Round maze.RoundConfig
}

// defaultMazeConfig is the shipping ruleset.
//
// Tick is 8s rather than the 5s the design first assumed, because players read
// the board through the stream: Twitch adds 3-15s of video latency and IRC
// another 1-2s, so at 5s every player is permanently a cycle or two behind a
// board they cannot act on. At 8s the whole see-decide-type-receive loop fits
// inside one cycle.
func defaultMazeConfig() mazeConfig {
	round := maze.DefaultRoundConfig()
	return mazeConfig{
		Tick:    8 * time.Second,
		Display: "full",
		Gen: maze.Config{
			Size:      6,
			LoopWalls: 4,
			// Derived, never written down twice: see mazeKeySlots.
			Keys:       mazeKeySlots(round.MaxSeats),
			KeyBandMin: 4,
			KeyBandMax: 6,
			Spikes:     2,
			BearTraps:  1,
		},
		Round: round,
	}
}

// mazeConfig returns the configured tuning, falling back to the shipping
// defaults when nothing has been loaded. Router holds it so maze.toml has
// somewhere to land (issue 09) without startMaze reaching for a package
// function.
func (r *Router) mazeConfig() mazeConfig {
	if r.mazeCfg.Tick > 0 {
		return r.mazeCfg
	}
	return defaultMazeConfig()
}

// mazeRound is one live round plus what the engine deliberately does not carry:
// which channel to talk to, and the ticker driving it.
type mazeRound struct {
	round  *maze.Round
	cfg    mazeConfig
	roomID string

	stop     chan struct{}
	stopOnce sync.Once

	// feed is the rolling play-by-play shown in the overlay panel, newest last.
	// It is cosmetic and deliberately not persisted: a restart losing the log
	// costs nothing, and keeping it out of the round record keeps the stored
	// document about the game rather than about its narration.
	feed []string
	// saidBounce stops a keyless player who keeps walking into the door from
	// announcing it every cycle. The first time is a moment; the fourth is noise.
	saidBounce map[int]bool
}

// mazeFeedLines is how much play-by-play the panel keeps on screen.
const mazeFeedLines = 5

// halt stops the round's cycle ticker. It is idempotent so that force-ending a
// round can never race a second force-end into a double close, and so tests can
// stop the real clock and drive cycles by hand.
func (mr *mazeRound) halt() { mr.stopOnce.Do(func() { close(mr.stop) }) }

// --- commands ---------------------------------------------------------------

// startMaze opens a round (!maze). Anyone can start one, matching !wordle.
func (r *Router) startMaze(m ChatMessage) {
	if r.chat == nil {
		r.logger.Printf("!maze: replies not configured — run 'mise run bot:auth'")
		return
	}
	// Claim the stage before taking mazeMu: claimBoard takes its own lock and
	// must never nest inside a per-game one.
	if ok, live := r.claimBoard(boardMaze); !ok {
		r.reply(m, boardBusyMsg(live))
		return
	}

	cfg := r.mazeConfig()
	seed := cfg.Seed
	if seed == 0 {
		seed = r.randInt63()
	}
	board, err := maze.Generate(seed, cfg.Gen)
	if err != nil {
		// A board this small and this config can only fail if the config is
		// nonsense, so free the stage rather than sit on it holding a dead game.
		r.logger.Printf("maze generate: %v", err)
		r.releaseBoard(boardMaze)
		r.reply(m, "couldn't build a maze — check the log.")
		return
	}

	r.mazeMu.Lock()
	if r.maze != nil {
		r.mazeMu.Unlock()
		r.reply(m, "🧭 a maze round is already going — !go w/a/s/d to play.")
		return
	}
	mr := &mazeRound{
		round:      maze.NewRound(board, cfg.Round, time.Now()),
		cfg:        cfg,
		roomID:     m.RoomID,
		stop:       make(chan struct{}),
		saidBounce: map[int]bool{},
	}
	r.maze = mr
	r.persistMaze(mr)
	payload := r.mazePayload(mr)
	r.mazeMu.Unlock()

	r.pushMaze(payload)
	go r.runMaze(mr)

	r.chat.Send(m.RoomID, fmt.Sprintf(
		"🧭 Torch Maze! Type !go <path> to take a seat — %s to join, %d seats. Moves are w/a/s/d (up to %d at once, e.g. !go wwd).",
		shortDuration(time.Duration(cfg.Round.JoinCycles)*cfg.Tick), cfg.Round.MaxSeats, cfg.Round.QueueMax))
}

// goMaze handles !go: it seats a chatter during the join window and queues their
// path.
//
// A successful move gets no chat reply on purpose. Five players on an 8s cycle
// would be ~35 confirmations a round in a chat with fewer than ten people in it,
// burying every human conversation; the overlay shows a locked-in indicator
// instead. Only failures and seating answer back.
func (r *Router) goMaze(rest string, m ChatMessage) {
	if r.chat == nil {
		return
	}
	r.mazeMu.Lock()
	mr := r.maze
	if mr == nil || mr.round.Phase == maze.PhaseDone {
		r.mazeMu.Unlock()
		r.reply(m, "no maze round right now — !maze to start one.")
		return
	}

	path, err := maze.ParsePath(rest, mr.round.Cfg.QueueMax)
	if err != nil {
		r.mazeMu.Unlock()
		r.reply(m, "moves are w/a/s/d (or up/down/left/right) — e.g. !go w or !go wwd.")
		return
	}

	if _, seated := mr.round.PlayerBy(m.UserID); !seated {
		if _, ok := mr.round.Join(m.UserID, m.User, displayName(m)); !ok {
			full := len(mr.round.Players) >= mr.round.Cfg.MaxSeats
			r.mazeMu.Unlock()
			if full {
				r.reply(m, "🧭 all seats are taken — you're up next round.")
			} else {
				r.reply(m, "🧭 seats are locked for this round — !go early next time.")
			}
			return
		}
	}

	// The engine breaks a same-cycle race for one key by submission time. Chat
	// messages carry no timestamp we can trust here, so this is arrival order at
	// the bot — which is the IRC delivery order, since the reader is sequential,
	// and is the same order Twitch itself put them in.
	mr.round.Submit(m.UserID, path, time.Now())
	r.persistMaze(mr)
	r.mazeMu.Unlock()
}

// --- the cycle --------------------------------------------------------------

// runMaze drives one round's cycles until it ends or is superseded.
func (r *Router) runMaze(mr *mazeRound) {
	t := time.NewTicker(mr.cfg.Tick)
	defer t.Stop()
	for {
		select {
		case <-mr.stop:
			return
		case now := <-t.C:
			if !r.tickMaze(mr, now) {
				return
			}
		}
	}
}

// tickMaze advances one cycle. It reports whether the round is still running.
func (r *Router) tickMaze(mr *mazeRound, now time.Time) bool {
	r.mazeMu.Lock()
	if r.maze != mr { // superseded or force-ended
		r.mazeMu.Unlock()
		return false
	}
	evs := mr.round.Tick(now)
	done := mr.round.Phase == maze.PhaseDone
	lines, toasts := mr.announce(evs)
	r.persistMaze(mr)
	payload := r.mazePayload(mr)
	finished := mr.finishers(evs)
	r.mazeMu.Unlock()

	r.pushMaze(payload)
	for _, t := range toasts {
		if r.overlay != nil {
			r.overlay.Push("notify", t)
		}
	}
	if r.chat != nil {
		for _, line := range lines {
			r.chat.Send(mr.roomID, line)
		}
	}
	r.awardMazeFinishers(mr, finished)
	if done {
		r.scheduleMazeClear(mr)
		return false
	}
	return true
}

// mazeFinish is one player getting through the door, copied out from under the
// lock so the payout can be written without holding the round's mutex across
// database calls.
type mazeFinish struct {
	UserID  string
	Login   string
	Display string
	Place   int
	Cycle   int
}

// finishers picks the finishes out of a cycle's events. Caller holds mazeMu.
func (mr *mazeRound) finishers(evs []maze.Event) []mazeFinish {
	var out []mazeFinish
	for _, e := range evs {
		if e.Kind != maze.EventFinished {
			continue
		}
		if p := mr.player(e.Seat); p != nil {
			out = append(out, mazeFinish{
				UserID: p.UserID, Login: p.Login, Display: p.Display,
				Place: p.Place, Cycle: p.FinishedCycle,
			})
		}
	}
	return out
}

// mazePayout is what a finishing place earns: the winner takes the full reward
// and every place below halves it, floored at one mark.
//
// The floor is the point. A placement that paid nothing would make the scramble
// after the winner exits pure ceremony — everyone who lost the race to the door
// would have no reason to keep walking, and the placement window would be a
// victory lap with spectators.
func mazePayout(reward int64, place int) int64 {
	if reward <= 0 || place < 1 {
		return 0
	}
	if place > 16 { // beyond any real field; keeps the shift honest
		return 1
	}
	if v := reward >> uint(place-1); v > 0 {
		return v
	}
	return 1
}

// awardMazeFinishers pays and announces each player who got out this cycle.
// Caller must not hold mazeMu: this writes to the store.
//
// Paying at the moment of the finish rather than when the round settles is
// deliberate, and it is what wordle already does with a solve. Getting through
// the door is a finished, earned thing; if the marks waited for the round to end
// then a mod running !skipgame during the placement scramble would take a win
// that had already happened away from whoever earned it.
func (r *Router) awardMazeFinishers(mr *mazeRound, fs []mazeFinish) {
	for _, f := range fs {
		line := fmt.Sprintf("🚪 @%s escapes in %s.", f.Display, ordinal(f.Place))
		if f.Place == 1 {
			line = fmt.Sprintf("🏁 @%s is OUT of the maze in %s — first place!", f.Display, plural(f.Cycle, "cycle"))
		}

		if r.store != nil {
			if reward := r.econ.MazeReward; r.economy && reward > 0 {
				earned := mazePayout(reward, f.Place)
				if _, err := r.store.Credit(f.UserID, earned, "maze_place", ""); err != nil {
					r.logger.Printf("maze payout %s: %v", f.Login, err)
				} else {
					line += fmt.Sprintf(" +%s %s", comma(earned), r.econ.CurrencyName)
				}
			}
			// Only the winner is tallied. Placements are already recorded in the
			// ledger, and a leaderboard counting "times I came third" would answer
			// a question nobody asks.
			if f.Place == 1 {
				wins, err := r.store.MazeAddWin(f.UserID, f.Login, f.Display)
				if err != nil {
					r.logger.Printf("maze win tally %s: %v", f.Login, err)
				} else {
					line += fmt.Sprintf(" (win #%d)", wins)
				}
			}
		}
		if r.chat != nil {
			r.chat.Send(mr.roomID, line)
		}
	}
}

// showMazeWins replies with the top escapees (!mazewins).
func (r *Router) showMazeWins(m ChatMessage) {
	if r.store == nil {
		return
	}
	if !(m.IsMod || m.IsBroadcaster) && !r.cooldown.Allow(m.User) {
		return
	}
	lb, err := r.store.MazeLeaderboard(10)
	if err != nil {
		r.logger.Printf("mazewins: %v", err)
		return
	}
	if len(lb) == 0 {
		r.reply(m, "Nobody has escaped the maze yet — !maze to start a round.")
		return
	}
	parts := make([]string, len(lb))
	for i, w := range lb {
		parts[i] = fmt.Sprintf("%d. %s %d", i+1, w.Display, w.Wins)
	}
	r.reply(m, "Top maze escapees: "+strings.Join(parts, "  "))
}

// announce turns a cycle's events into what chat hears, what the panel logs and
// what gets a toast. Caller holds mazeMu.
//
// The split is by *actionability*, not by importance, because the two sinks reach
// a player at different times: chat arrives 1-2s after the event, the overlay
// 5-10s after it, through the stream. So anything that changes what you should do
// next goes to chat, and colour goes to the panel.
//
// Chat gets at most one line of texture per cycle no matter how much happened.
// Five players on an 8s cycle can easily produce four events at once, and a bot
// that says four things every eight seconds for three minutes buries every human
// conversation in a chat with under ten people in it.
func (mr *mazeRound) announce(evs []maze.Event) (lines []string, toasts []notifyData) {
	// A spiked key-holder emits KeyDropped and Spiked in the same cycle. They are
	// one event to a reader, so the drop is folded into the spike sentence rather
	// than said twice.
	dropped := map[int]maze.Cell{}
	for _, e := range evs {
		if e.Kind == maze.EventKeyDropped {
			dropped[e.Seat] = e.At
		}
	}

	var texture []string
	var bookends []string
	for _, e := range evs {
		mr.logFeed(e)
		switch e.Kind {
		case maze.EventSeatsLocked:
			bookends = append(bookends, mr.seatsLockedLine(e.N))
		case maze.EventFinished:
			// The chat line for a finish is written by awardMazeFinishers, which
			// runs outside the lock and knows what was actually paid.
			if p := mr.player(e.Seat); p != nil {
				toasts = append(toasts, notifyData{
					Kind:  "maze",
					Line1: "🏁 " + p.Display + " is OUT",
					Line2: mazeOrdinalWord(e.N) + " out of the maze",
				})
			}
		case maze.EventRoundEnded:
			if l := mr.endLine(e.Reason); l != "" {
				bookends = append(bookends, l)
			}
		case maze.EventSpiked:
			if p := mr.player(e.Seat); p != nil {
				line := "💀 @" + p.Display + " hit spikes at " + e.At.String() + " — back to the start"
				toast := notifyData{Kind: "maze", Line1: "💀 " + p.Display + " hit spikes", Line2: "back to the start"}
				if at, ok := dropped[e.Seat]; ok {
					line += " and dropped their key there"
					toast.Line2 = "dropped their key at " + at.String()
				}
				texture = append(texture, line)
				toasts = append(toasts, toast)
			}
		case maze.EventTrapped:
			if p := mr.player(e.Seat); p != nil {
				texture = append(texture, fmt.Sprintf("🐻 @%s is caught for %s", p.Display, plural(e.N, "cycle")))
			}
		case maze.EventFreed:
			if p := mr.player(e.Seat); p != nil {
				texture = append(texture, "🔓 @"+p.Display+" is loose again")
			}
		case maze.EventKeyTaken:
			if p := mr.player(e.Seat); p != nil {
				if e.N == 0 {
					texture = append(texture, "🔑 @"+p.Display+" took the LAST key")
				} else {
					texture = append(texture, fmt.Sprintf("🔑 @%s has a key (%s left)", p.Display, plural(e.N, "key")))
				}
			}
		case maze.EventBounced:
			// Worth saying once — it tells the whole channel someone has found the
			// door and cannot open it, which is the shape of the round in one line.
			if p := mr.player(e.Seat); p != nil && !mr.saidBounce[e.Seat] {
				mr.saidBounce[e.Seat] = true
				texture = append(texture, "🚪 @"+p.Display+" is at the exit with no key!")
			}
		}
		// Bonks are deliberately not in chat. They are by far the most frequent
		// event, they say only that someone guessed a wall, and the board already
		// shows the move was lost. They stay in the panel feed.
	}

	if len(texture) > 0 {
		lines = append(lines, strings.Join(texture, " · "))
	}
	return append(lines, bookends...), toasts
}

// player is the seat's player, or nil for a round-level event.
func (mr *mazeRound) player(seat int) *maze.Player {
	if seat < 0 || seat >= len(mr.round.Players) {
		return nil
	}
	return mr.round.Players[seat]
}

// logFeed appends the panel's one-glyph version of an event. These are terse
// because the panel has room for a handful of lines, not sentences — the feed is
// there so a viewer can see *that* things are happening without reading chat.
func (mr *mazeRound) logFeed(e maze.Event) {
	name := ""
	if p := mr.player(e.Seat); p != nil {
		name = p.Display
	}
	var line string
	switch e.Kind {
	case maze.EventSeatsLocked:
		line = fmt.Sprintf("▶ locked — %s", plural(e.N, "key"))
	case maze.EventKeyTaken:
		line = "🔑 " + name
		if e.N == 0 {
			line += " — last key"
		}
	case maze.EventKeyDropped:
		line = "🔑 dropped " + e.At.String()
	case maze.EventSpiked:
		line = "💀 " + name + " → start"
	case maze.EventTrapped:
		line = fmt.Sprintf("🐻 %s ×%d", name, e.N)
	case maze.EventFreed:
		line = "🔓 " + name
	case maze.EventBounced:
		line = "🚪 " + name + " no key"
	case maze.EventBonked:
		line = "🧱 " + name
	case maze.EventFinished:
		line = fmt.Sprintf("🏁 %s %s", name, ordinal(e.N))
	default:
		return
	}
	mr.feed = append(mr.feed, line)
	if len(mr.feed) > mazeFeedLines {
		mr.feed = mr.feed[len(mr.feed)-mazeFeedLines:]
	}
}

// mazeOrdinalWord reads better than "1st" in a toast's second line.
func mazeOrdinalWord(place int) string {
	switch place {
	case 1:
		return "first"
	case 2:
		return "second"
	case 3:
		return "third"
	default:
		return ordinal(place)
	}
}

func (mr *mazeRound) seatsLockedLine(keys int) string {
	names := make([]string, 0, len(mr.round.Players))
	for _, p := range mr.round.Players {
		names = append(names, "@"+p.Display)
	}
	line := fmt.Sprintf("🧭 Seats locked: %s — %s on the board.",
		strings.Join(names, " "), plural(keys, "key"))
	switch short := len(mr.round.Players) - keys; {
	case short == 1:
		line += " One short — somebody isn't getting out. Don't be last to a key."
	case short > 1:
		line += fmt.Sprintf(" %s short — don't be last to one.", plural(short, "key"))
	}
	return line
}

func (mr *mazeRound) endLine(reason maze.EndReason) string {
	if reason == maze.EndAbandoned {
		return "🧭 Nobody joined — maze cancelled. !maze to try again."
	}
	places := mr.round.Placements()
	if len(places) == 0 {
		var stranded []string
		for _, p := range mr.round.Players {
			stranded = append(stranded, "@"+p.Display)
		}
		return fmt.Sprintf("⏱ Nobody made it out. %s — the maze wins. !maze to go again.",
			strings.Join(stranded, " "))
	}
	parts := make([]string, len(places))
	for i, p := range places {
		parts[i] = fmt.Sprintf("%s %s", ordinal(p.Place), p.Display)
	}
	line := "🏁 Round over — " + strings.Join(parts, ", ") + "."
	if left := len(mr.round.Players) - len(places); left > 0 {
		line += fmt.Sprintf(" %d left behind in the dark.", left)
	}
	return line + " !maze to go again."
}

// --- ending -----------------------------------------------------------------

// scheduleMazeClear leaves the settled board up for a moment so the placements
// can be read off it, then frees the stage.
func (r *Router) scheduleMazeClear(done *mazeRound) {
	time.AfterFunc(mazeResultLinger, func() { r.clearMaze(done) })
}

// clearMaze drops the round and frees the stage for another game.
//
// It no-ops unless done is still the current round: a new !maze may have started
// during the linger, and an older round's timer must not tear it down. Split out
// of the timer closure so a test can exercise it without waiting out the linger —
// a stage that is never released is a bot that can never start another game, and
// that failure would otherwise only show up on stream.
func (r *Router) clearMaze(done *mazeRound) {
	r.mazeMu.Lock()
	if r.maze != done {
		r.mazeMu.Unlock()
		return
	}
	r.maze = nil
	r.clearRound(mazeGame)
	r.mazeMu.Unlock()

	done.halt()
	r.releaseBoard(boardMaze)
	r.pushMazeHidden()
}

// forceEndMaze drops the round immediately (mod !skipgame), freeing the stage at
// once with no linger.
func (r *Router) forceEndMaze() {
	r.mazeMu.Lock()
	mr := r.maze
	r.maze = nil
	r.clearRound(mazeGame)
	r.mazeMu.Unlock()

	if mr != nil {
		mr.halt()
	}
	r.releaseBoard(boardMaze)
	r.pushMazeHidden()
	if mr != nil && r.chat != nil {
		r.chat.Send(mr.roomID, "🛑 Maze skipped. !maze to play again.")
	}
}

// --- persistence ------------------------------------------------------------

// mazeRec is the persisted round: the engine's own state document plus the two
// things it has no business knowing — the channel and how the board is drawn.
type mazeRec struct {
	RoomID  string          `json:"roomID"`
	TickMS  int64           `json:"tickMs"`
	Display string          `json:"display"`
	Round   maze.RoundState `json:"round"`
}

// persistMaze mirrors the round to the store. Caller holds mazeMu.
func (r *Router) persistMaze(mr *mazeRound) {
	rec := mazeRec{
		RoomID:  mr.roomID,
		TickMS:  mr.cfg.Tick.Milliseconds(),
		Display: mr.cfg.Display,
		Round:   mr.round.State(),
	}
	r.saveRound(mazeGame, mr.roomID, mr.round.Deadline().UnixMilli(), rec)
}

// loadMaze restores an in-flight round at startup and starts its clock again.
//
// The ticker's phase is not preserved, so the cycle in progress when the bot went
// down gets a full fresh cycle — which is the friendly way round, since everyone
// has just lost their view of the board. The wall-clock guard is not reset,
// though: a round that was live before a long outage hits its deadline on the
// first tick and ends, which is exactly what that guard is for.
func (r *Router) loadMaze() {
	var rec mazeRec
	if !r.loadRoundInto(mazeGame, &rec) {
		return
	}
	round, err := maze.Restore(rec.Round)
	if err != nil {
		// A round we cannot read is dropped, never guessed at: there is no safe
		// way to resume a game whose state is untrustworthy and then place people
		// on it.
		r.logger.Printf("maze restore: %v", err)
		r.clearRound(mazeGame)
		return
	}
	if round.Phase == maze.PhaseDone {
		r.clearRound(mazeGame)
		return
	}
	if ok, live := r.claimBoard(boardMaze); !ok {
		r.logger.Printf("maze restore: stage held by %s, dropping the round", live)
		r.clearRound(mazeGame)
		return
	}

	cfg := r.mazeConfig()
	if rec.TickMS > 0 {
		cfg.Tick = time.Duration(rec.TickMS) * time.Millisecond
	}
	if rec.Display != "" {
		cfg.Display = rec.Display
	}
	cfg.Round = round.Cfg // the round plays by the rules it started under

	mr := &mazeRound{round: round, cfg: cfg, roomID: rec.RoomID, stop: make(chan struct{}), saidBounce: map[int]bool{}}
	// Assigning r.maze without mazeMu is safe only because loadMaze runs at
	// startup, before the IRC loop and before any game timer exists. Don't move
	// this call later without taking the lock.
	r.maze = mr
	r.pushMaze(r.mazePayload(mr))
	go r.runMaze(mr)
}

// --- overlay ----------------------------------------------------------------

// mazeCell is one cell as the overlay sees it. Walls are sent only for revealed
// cells: an unrevealed cell's layout is the thing the fog exists to withhold, so
// it must not travel to the renderer at all rather than be sent and hidden in CSS.
type mazeCell struct {
	State string `json:"state"`           // unknown | frontier | revealed
	Walls uint8  `json:"walls,omitempty"` // N/E/S/W bitmask; revealed cells only
}

// mazePlayer is one runner's HUD row.
type mazePlayer struct {
	Seat     int    `json:"seat"`
	Name     string `json:"name"`
	At       string `json:"at"`
	HasKey   bool   `json:"hasKey,omitempty"`
	StuckFor int    `json:"stuckFor,omitempty"`
	Place    int    `json:"place,omitempty"`
	// Locked says a move is buffered. It is deliberately not *which* move: seeing
	// intent would let a player on a fast connection intercept one on a slow
	// connection every cycle, which is the unfairness the whole tick model exists
	// to remove.
	Locked bool `json:"locked,omitempty"`
}

// mazeBoard is the render payload.
type mazeBoard struct {
	Display   string       `json:"display"`
	Phase     string       `json:"phase"`
	Cycle     int          `json:"cycle"`
	MaxCycles int          `json:"maxCycles"`
	TickMS    int64        `json:"tickMs"`
	Size      int          `json:"size"`
	Cells     []mazeCell   `json:"cells"` // row-major, y*Size+x
	Start     string       `json:"start"`
	Exit      string       `json:"exit"`
	Keys      []string     `json:"keys"`  // unclaimed keys, drawn through the fog
	Traps     []mazeTrap   `json:"traps"` // sprung traps only
	Players   []mazePlayer `json:"players"`
	Seats     int          `json:"seats"`
	Feed      []string     `json:"feed,omitempty"`
}

type mazeTrap struct {
	At   string `json:"at"`
	Kind string `json:"kind"`
}

// mazePayload builds the render state. Caller holds mazeMu.
//
// Two things are deliberately withheld. Unsprung traps never leave the bot — they
// are the one genuine surprise in the game, and a payload that carried them would
// put them one devtools panel away. Neither do the walls of cells nobody has
// walked into yet. Objectives are the exception and are always sent: the fog
// hides the maze's shape, not where you are trying to get to.
func (r *Router) mazePayload(mr *mazeRound) mazeBoard {
	rd := mr.round
	b := mazeBoard{
		Display:   mr.cfg.Display,
		Phase:     rd.Phase.String(),
		Cycle:     rd.Cycle,
		MaxCycles: rd.Cfg.MaxCycles,
		TickMS:    mr.cfg.Tick.Milliseconds(),
		Size:      rd.Map.Size,
		Start:     rd.Map.Start.String(),
		Exit:      rd.Map.Exit.String(),
		Seats:     rd.Cfg.MaxSeats,
		Cells:     make([]mazeCell, 0, rd.Map.Size*rd.Map.Size),
		Feed:      append([]string(nil), mr.feed...),
	}
	for y := 0; y < rd.Map.Size; y++ {
		for x := 0; x < rd.Map.Size; x++ {
			c := maze.Cell{X: x, Y: y}
			switch {
			case rd.Revealed(c):
				b.Cells = append(b.Cells, mazeCell{State: "revealed", Walls: rd.Map.Walls(c)})
			case rd.Frontier(c):
				b.Cells = append(b.Cells, mazeCell{State: "frontier"})
			default:
				b.Cells = append(b.Cells, mazeCell{State: "unknown"})
			}
		}
	}
	for _, k := range rd.KeysOnMap() {
		b.Keys = append(b.Keys, k.String())
	}
	for i, t := range rd.Map.Traps {
		if rd.TrapSprung(i) {
			b.Traps = append(b.Traps, mazeTrap{At: t.At.String(), Kind: t.Kind.String()})
		}
	}
	for _, p := range rd.Players {
		b.Players = append(b.Players, mazePlayer{
			Seat: p.Seat, Name: p.Display, At: p.At.String(),
			HasKey: p.HasKey, StuckFor: p.StuckFor, Place: p.Place,
			Locked: p.Queued() > 0,
		})
	}
	return b
}

func (r *Router) pushMaze(b mazeBoard) {
	if r.overlay != nil {
		r.overlay.Push("maze", b)
	}
}

func (r *Router) pushMazeHidden() {
	if r.overlay != nil {
		r.overlay.Push("maze", map[string]any{"hidden": true})
	}
}

// --- small helpers ----------------------------------------------------------

func ordinal(n int) string {
	suffix := "th"
	if n%100 < 11 || n%100 > 13 {
		switch n % 10 {
		case 1:
			suffix = "st"
		case 2:
			suffix = "nd"
		case 3:
			suffix = "rd"
		}
	}
	return fmt.Sprintf("%d%s", n, suffix)
}
