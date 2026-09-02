package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"tts/internal/maze"
	"tts/internal/mazearchive"
	"tts/internal/mazeview"
	"tts/store"
)

// GET OUT!!!, bot-owned. Up to five chatters each drive a sprite through a
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

// MazeLog is the replay-archive slice of the store (an interface so tests can
// substitute a fake). *sqlite.Store and *postgres.Store satisfy it.
type MazeLog interface {
	MazeLogRound(r store.MazeRound, evs []store.MazeEvent) error
}

// id names this round, for the archive and for the overlay.
//
// Rounds cannot overlap — there is one board stage — so the start instant is
// already unique; the seed rides along because it can be pinned for a rematch,
// and a colliding id would silently drop the second round from the log.
//
// The same string goes to both, which is what lets a round being watched be
// matched up with its archived record afterwards, and what lets the renderer tell
// one round from the next without having to guess from the cycle counter.
func (mr *mazeRound) id() string {
	return fmt.Sprintf("%d-%x", mr.round.State().StartedAt, mr.round.Map.Seed)
}

// mazeResultLinger is how long a settled board stays up before the stage is
// freed — long enough to read the placements off the overlay.
const mazeResultLinger = 15 * time.Second

// mazeConfig is the whole game's tuning. Defaults live in defaultMazeConfig and
// are what ships; issue 09 replaces that with maze.toml, which is why this is a
// struct rather than a handful of constants.
type mazeConfig struct {
	Tick    time.Duration // the input window: how long players have to choose
	Display string        // "panel" | "full" — how the overlay renders the board
	// ResolveBuffer is dead time at the *start* of every cycle, immediately after
	// the tick that resolved the previous one. The board is already showing the
	// new positions; this is the beat in which the runners visibly move into them.
	//
	// It sits at the start rather than the end deliberately. By the time anyone can
	// type, the resolution they are reacting to has happened, so input during the
	// buffer belongs to the next tick and nothing can be banked against a turn that
	// is about to resolve — which is the bug that made joining also move you.
	ResolveBuffer time.Duration
	// Seed fixes the board. Zero draws a fresh one per round, which is what a
	// stream wants; setting it replays one exact maze, for a rematch or for
	// reproducing something that went wrong.
	Seed  int64
	Gen   maze.Config
	Round maze.RoundConfig
}

// defaultMazeConfig is the shipping ruleset.
//
// Tick is 10s, not the 5s the design first assumed, because players read the
// board through the stream: Twitch adds 3-15s of video latency and IRC another
// 1-2s, so at 5s every player is permanently a cycle or two behind a board they
// cannot act on. Ten seconds puts the whole see-decide-type-receive loop inside a
// single cycle with room to spare.
//
// Display defaults to the corner panel. Covering the stage for several minutes is
// the channel owner's call, made per round with "!maze full".
func defaultMazeConfig() mazeConfig {
	round := maze.DefaultRoundConfig()
	return mazeConfig{
		Tick:          10 * time.Second,
		ResolveBuffer: time.Second,
		Display:       "panel",
		Gen: maze.Config{
			Size:      6,
			LoopWalls: 4,
			// Derived, never written down twice: see mazeKeySlots.
			Keys:       mazeKeySlots(round.MaxSeats),
			KeyBandMin: 4,
			KeyBandMax: 6,
			// Eight traps on a 36-cell board, and the mix is the comeback dial.
			//
			// This shipped as 2 spikes and 1 bear trap, reasoned about before anyone
			// had played it: spikes are the only way a key returns to the board, so
			// spike density is the rate at which a locked-out player gets back into
			// contention, and a bear trap was treated as the mild one to be used
			// sparingly. Play disagreed. A single bear trap on a board this size is
			// one most rounds never meet, so the mechanic barely existed, and two
			// spikes over a ~25-turn round left the shortfall resolving early and
			// then standing.
			//
			// Traps despawn on firing, so density is a budget for the round rather
			// than a standing hazard: the first player through clears each one for
			// everyone behind. Eight is roughly one trap per three turns of a typical
			// round, spread over the ~12-15 cells traffic actually crosses.
			Spikes:    3,
			BearTraps: 5,
		},
		Round: round,
	}
}

// cyclePeriod is how often a cycle actually resolves: the input window plus the
// beat after it in which the movement is drawn. Everything that schedules or
// measures a cycle uses this; Tick alone is only ever the countdown players read.
func (c mazeConfig) cyclePeriod() time.Duration { return c.Tick + c.ResolveBuffer }

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

	// cycleEndsAt is when the current cycle resolves. It exists so the overlay can
	// be told how much of the turn is actually left, rather than inferring it from
	// the tick length at the moment a payload happens to arrive: the bot pushes on
	// every move as well as every cycle, and the page restarted its countdown on
	// each one, so the bar refilled every time somebody typed and never reached
	// zero. The turn looked like it was being skipped with time still on it.
	cycleEndsAt time.Time

	// joined buffers the seats taken since the last cycle so they go out as one
	// line. Answering each !up on its own would be five bot messages inside the
	// join window in a chat with fewer than ten people in it; saying nothing at
	// all, which is what it used to do, leaves a first-timer with no evidence for
	// twenty seconds that they are in the round.
	joined []int
	// saidRepeatHint and saidLastKey each gate a line that is worth saying once
	// and grating said twice, the way saidBounce does.
	saidRepeatHint bool
	saidLastKey    bool

	// opening is the round exactly as it began, kept for the archive.
	//
	// It has to be captured rather than derived, because the state the round is in
	// when it ends is not reversible: fog has been lifted, traps are spent, keys are
	// gone and everybody is placed. The archive stored that end state under the name
	// "initial" for a while, so a replay built on it restored a finished round and
	// played no game at all — silently, since nothing about a done round errors.
	opening maze.RoundState

	// log and moves are the permanent record: what happened, and the input that
	// caused it. Unlike feed, these *are* persisted with the round — they ride in
	// the document persistMaze already writes every cycle, so a bot that restarts
	// mid-round still has the first half of the game when it comes to archive it.
	log   []store.MazeEvent
	moves []mazearchive.Submission
	// logged guards against a second archive write. Several paths end a round and
	// more than one can reach the same round; the store insert is idempotent too,
	// but not making the call at all is cheaper than relying on that.
	logged bool
}

// halt stops the round's cycle ticker. It is idempotent so that force-ending a
// round can never race a second force-end into a double close, and so tests can
// stop the real clock and drive cycles by hand.
func (mr *mazeRound) halt() { mr.stopOnce.Do(func() { close(mr.stop) }) }

// --- commands ---------------------------------------------------------------

// startMaze opens a round (!maze). Anyone can start one, matching !wordle.
// startMaze opens a round (!maze). Anyone can start one, matching !wordle.
//
// "!maze full" puts the board on the whole stage instead of the corner panel, and
// is the channel owner's call alone: it covers whatever else is on screen for
// several minutes, which is not a decision to hand to chat. Asking for it without
// being the owner still gets you a round — refusing the whole thing over a
// modifier would be the harsher reading — just a panel one.
func (r *Router) startMaze(rest string, m ChatMessage) {
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
	deniedFull := false
	switch strings.ToLower(strings.TrimSpace(rest)) {
	case "full":
		if m.IsBroadcaster {
			cfg.Display = "full"
		} else {
			deniedFull = true
		}
	case "panel":
		cfg.Display = "panel"
	}

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
		r.reply(m, "🧭 a GET OUT!!! round is already going — !up !down !left !right to play.")
		return
	}
	mr := &mazeRound{
		round:      maze.NewRound(board, cfg.Round, time.Now()),
		cfg:        cfg,
		roomID:     m.RoomID,
		stop:       make(chan struct{}),
		saidBounce: map[int]bool{},
		// runMaze's ticker starts a beat after this, which is close enough: the
		// alternative is a zero value that reads as a cycle already over.
		cycleEndsAt: time.Now().Add(cfg.cyclePeriod()),
	}
	// Before anyone joins and before the first tick: this is the only moment the
	// opening state exists to be taken.
	mr.opening = mr.round.State()
	r.maze = mr
	r.persistMaze(mr)
	payload := r.mazePayload(mr)
	r.mazeMu.Unlock()

	r.pushMaze(payload)
	go r.runMaze(mr)

	// Seconds, spelled out here rather than through shortDuration: that rounds to
	// the nearest minute for uptime and follow-age, so every join window this game
	// will ever have came out as "0m".
	joinWindow := time.Duration(cfg.Round.JoinCycles) * cfg.cyclePeriod()
	// One thing to do, and where to read the rest. This line used to carry the
	// seat count, every movement spelling, the cycle length and the duplicate-move
	// workaround in one 155-character breath, which wraps to four lines in chat and
	// buries the only sentence that needs acting on. The cycle length is on screen
	// as a countdown, the workaround now rides the join line — where it reaches
	// people who are about to move — and !mazehelp has held the full rules all
	// along without anything ever pointing at it.
	r.sendMaze(m.RoomID, fmt.Sprintf(
		"🧭 GET OUT!!! Type !up to grab one of %d seats — %.0fs. !mazehelp for how it works.",
		cfg.Round.MaxSeats, joinWindow.Seconds()))
	if deniedFull {
		r.reply(m, "full-screen is the channel owner's call — started in the corner instead.")
	}
}

// goMaze handles "!go <direction>", the wordier spelling of the same move.
//
// Three ways to say each move — !up, !go up, !gou — is not indecision. Twitch
// drops a message identical to the sender's previous one within thirty seconds,
// and this game asks people to walk straight corridors, so a viewer going north
// four times in a row would have three of those swallowed with no feedback
// explaining why. Alternating spellings sidesteps it entirely. Moderators and the
// broadcaster appear to be exempt, which is exactly why it took a report from
// someone else to notice.
func (r *Router) goMaze(rest string, m ChatMessage) {
	if r.chat == nil {
		return
	}
	d, ok := maze.ParseDir(strings.ToLower(strings.TrimSpace(rest)))
	if !ok {
		r.reply(m, "!go up / !go down / !go left / !go right — or just !up, or !gou.")
		return
	}
	r.moveMaze(d, m)
}

// moveMaze handles !up / !down / !left / !right: it seats a chatter during the
// join window, and once the race is on, locks in their move for the next cycle.
//
// **Joining does not also move you.** A command sent during the join window takes
// the seat and nothing else. When it did both, a player's very first message —
// the one they sent to join — banked a move they had not meant to make, so their
// sprite advanced two cells on the opening cycles and they spent the rest of the
// round one step away from where they believed they were. Every downstream
// confusion followed from that.
//
// A successful move gets no chat reply on purpose. Five players on a 10s cycle
// would be dozens of confirmations a round in a chat with fewer than ten people in
// it, burying every human conversation; the overlay shows the locked-in move
// instead. Only failures and seat refusals answer back.
func (r *Router) moveMaze(d maze.Dir, m ChatMessage) {
	if r.chat == nil {
		return
	}
	r.mazeMu.Lock()
	mr := r.maze
	if mr == nil || mr.round.Phase == maze.PhaseDone {
		r.mazeMu.Unlock()
		r.reply(m, "no round right now — !getout to start one.")
		return
	}

	seatOf, seated := mr.round.PlayerBy(m.UserID)
	if !seated {
		seat, joinedNow := mr.round.Join(m.UserID, m.User, displayName(m))
		if !joinedNow {
			full := len(mr.round.Players) >= mr.round.Cfg.MaxSeats
			r.mazeMu.Unlock()
			if full {
				r.reply(m, "🧭 all seats are taken — you're up next round.")
			} else {
				r.reply(m, "🧭 seats are locked for this round — get in early next time.")
			}
			return
		}
		// Seated by this command, and that is all it does.
		mr.joined = append(mr.joined, seat)
		r.persistMaze(mr)
		payload := r.mazePayload(mr)
		r.mazeMu.Unlock()
		r.pushMaze(payload)
		return
	}

	// The engine breaks a same-cycle race for one key by submission time. Chat
	// messages carry no timestamp we can trust here, so this is arrival order at
	// the bot — which is the IRC delivery order, since the reader is sequential,
	// and is the same order Twitch itself put them in.
	at := time.Now()
	mr.round.Submit(m.UserID, d, at)
	mr.moves = append(mr.moves, mazearchive.Submission{
		Cycle: mr.round.Cycle, Seat: seatOf.Seat, Dir: d.String(), At: at.UnixMilli(),
	})
	r.persistMaze(mr)
	payload := r.mazePayload(mr)
	r.mazeMu.Unlock()

	// Push on the move, not just on the tick. The board is only redrawn once a
	// cycle otherwise — and by then the move has been spent, so the direction a
	// player just chose would never actually appear on screen.
	r.pushMaze(payload)
}

// --- the cycle --------------------------------------------------------------

// runMaze drives one round's cycles until it ends or is superseded.
func (r *Router) runMaze(mr *mazeRound) {
	t := time.NewTicker(mr.cfg.cyclePeriod())
	defer t.Stop()
	for {
		select {
		case <-mr.stop:
			return
		case now := <-t.C:
			if !r.tickMaze(mr, now) {
				return
			}
			// Discard a tick that fired while this cycle was resolving. Go buffers
			// one, so an overrunning cycle used to deliver the next tick the
			// instant it returned — two cycles back to back, and a sprite that
			// moved twice with nobody touching it.
			//
			// Belt and braces, and honestly so: queuing the chat sends removed the
			// only thing that currently makes a cycle slow, so the suite cannot
			// isolate this — an attempt to test it needed a fake slow enough to
			// distort the very timings it was measuring. It is here for the next
			// thing that gets added to a cycle, not for anything in one today.
			select {
			case <-t.C:
			default:
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
	mr.cycleEndsAt = now.Add(mr.cfg.cyclePeriod())
	done := mr.round.Phase == maze.PhaseDone
	mr.record(evs)
	lines, endLine, toasts := mr.announce(evs)
	// Ahead of everything announce produced, so that on the cycle the window
	// closes the roll-call is still the last thing said.
	if joinLine, ok := mr.flushJoins(); ok {
		lines = append([]string{joinLine}, lines...)
	}
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
	// Order matters: what happened this cycle, then who got out, then the closing
	// summary that names them.
	for _, line := range lines {
		r.sendMaze(mr.roomID, line)
	}
	r.awardMazeFinishers(mr, finished)
	if endLine != "" {
		r.sendMaze(mr.roomID, endLine)
	}
	if done {
		r.archiveMaze(mr, "")
		r.scheduleMazeClear(mr)
		return false
	}
	return true
}

// archiveMaze writes the permanent record of a finished round. Caller must not
// hold mazeMu: this writes to the store.
//
// reason overrides the engine's own, and exists for one case: a moderator's
// !skipgame ends a round without the engine ever reaching PhaseDone, so there is
// no end reason to read. "skipped" is a documented extra value rather than a new
// EndReason, because adding one would ripple through the wire maps and change how
// every already-stored round decodes.
//
// It runs where the round ends rather than where it clears, so the archive is
// written before anything is thrown away. By then no further cycles are due, so a
// slow write delays nothing.
func (r *Router) archiveMaze(mr *mazeRound, reason string) {
	if r.store == nil {
		return
	}
	r.mazeMu.Lock()
	if mr.logged {
		r.mazeMu.Unlock()
		return
	}
	mr.logged = true
	rec, evs := mr.archive(reason)
	r.mazeMu.Unlock()

	if err := r.store.MazeLogRound(rec, evs); err != nil {
		r.logger.Printf("maze archive %s: %v", rec.ID, err)
	}
}

// archive flattens the round for the log. Caller holds mazeMu.
func (mr *mazeRound) archive(reason string) (store.MazeRound, []store.MazeEvent) {
	st := mr.round.State()
	if reason == "" {
		reason = st.Reason
	}
	rec := store.MazeRound{
		ID:        mr.id(),
		RoomID:    mr.roomID,
		Seed:      mr.round.Map.Seed,
		StartedAt: st.StartedAt / 1000,
		EndedAt:   time.Now().Unix(),
		TickMS:    mr.cfg.Tick.Milliseconds(),
		Cycles:    mr.round.Cycle,
		Reason:    reason,
		Players:   len(mr.round.Players),
	}
	for _, p := range mr.round.Placements() {
		rec.Finishers++
		if p.Place == 1 {
			rec.WinnerID, rec.WinnerLogin, rec.WinnerDisplay = p.UserID, p.Login, p.Display
		}
	}
	resolveMS := mr.cfg.ResolveBuffer.Milliseconds()
	replay := mazearchive.Replay{
		Final: st, Gen: mr.cfg.Gen, Moves: mr.moves,
		ResolveMS: &resolveMS, Display: mr.cfg.Display,
	}
	// Zero when a round predates the capture and was resumed across the upgrade.
	if mr.opening.StartedAt != 0 {
		opening := mr.opening
		replay.Opening = &opening
	}
	if doc, err := json.Marshal(replay); err == nil {
		rec.Input = doc
	} else {
		rec.Input = []byte("{}")
	}
	// The round id is only known here — it is derived from the start instant and
	// the seed — so the events are stamped with it now rather than at the point
	// they were recorded.
	evs := make([]store.MazeEvent, len(mr.log))
	for i, e := range mr.log {
		e.RoundID = rec.ID
		evs[i] = e
	}
	return rec, evs
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
		line := fmt.Sprintf("🚪 @%s escapes in %s.", f.Display, mazeview.Ordinal(f.Place))
		if f.Place == 1 {
			line = fmt.Sprintf("🏁 @%s is OUT of the maze in %s — first place!", f.Display, plural(f.Cycle, "turn"))
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
		r.sendMaze(mr.roomID, line)
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
		r.reply(m, "Nobody has escaped yet — !getout to start a round.")
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
func (mr *mazeRound) announce(evs []maze.Event) (lines []string, end string, toasts []notifyData) {
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
				// The medal here for the same reason as in the panel: this toast is
				// drawn by the overlay, where 🏁 came out as a grey chequerboard.
				toasts = append(toasts, notifyData{
					Kind:  "maze",
					Line1: mazeview.Medal(e.N) + " " + p.Display + " is OUT",
					Line2: mazeOrdinalWord(e.N) + " out of the maze",
				})
			}
		case maze.EventRoundEnded:
			// Handed back separately, not appended: the closing summary names the
			// placements, so it has to be said after the finishes that produced
			// them rather than before.
			end = mr.endLine(e.Reason)
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
				texture = append(texture, fmt.Sprintf("🐻 @%s is caught for %s", p.Display, plural(e.N, "turn")))
			}
		case maze.EventFreed:
			if p := mr.player(e.Seat); p != nil {
				texture = append(texture, "🔓 @"+p.Display+" is loose again")
			}
		case maze.EventKeyTaken:
			if p := mr.player(e.Seat); p != nil {
				if e.N == 0 {
					line := "🔑 @" + p.Display + " took the LAST key"
					if out := mr.lockedOutLine(); out != "" {
						line += " · " + out
					}
					texture = append(texture, line)
				} else {
					texture = append(texture, fmt.Sprintf("🔑 @%s has a key (%s left)", p.Display, plural(e.N, "key")))
				}
			}
		case maze.EventBounced:
			// Worth saying once — it tells the whole channel someone has found the
			// door and cannot open it, which is the shape of the round in one line.
			if p := mr.player(e.Seat); p != nil && !mr.saidBounce[e.Seat] {
				mr.saidBounce[e.Seat] = true
				texture = append(texture, "🚪 @"+p.Display+" tried the door — no key!")
			}
		case maze.EventBonked:
			// A bonk costs a whole cycle, and from the seat it is indistinguishable
			// from a message Twitch swallowed as a duplicate: you typed, you did not
			// move. The two want opposite responses — turn, versus say it again
			// differently — and the bot never sees a dropped duplicate at all, so
			// naming the bonk is the only side of the ambiguity it can speak to.
			//
			// This was feed-only, on the grounds that bonks are the most frequent
			// event. That was written when a !go path banked several moves into
			// fogged cells; with one deliberate move a turn, and a cell's own walls
			// revealed the moment you stand on it, an attentive player cannot bonk
			// by surprise. TestMazeChatVolumeStaysWithinBudget holds the claim.
			if p := mr.player(e.Seat); p != nil {
				texture = append(texture, "🧱 @"+p.Display+" walked into a wall — that turn is gone")
			}
		}
	}

	if len(texture) > 0 {
		lines = append(lines, strings.Join(texture, " · "))
	}
	return append(lines, bookends...), end, toasts
}

// flushJoins turns the seats taken since the last cycle into one line and empties
// the buffer. Coalescing is the whole point — see mazeRound.joined — so this is
// deliberately neither one message per player nor the silence it replaces.
func (mr *mazeRound) flushJoins() (string, bool) {
	if len(mr.joined) == 0 {
		return "", false
	}
	names := make([]string, 0, len(mr.joined))
	for _, seat := range mr.joined {
		if p := mr.player(seat); p != nil {
			_, emoji := mazeview.Seat(seat)
			names = append(names, emoji+" @"+p.Display)
		}
	}
	mr.joined = mr.joined[:0]
	if len(names) == 0 {
		return "", false
	}
	line := strings.Join(names, " ")
	if len(names) == 1 {
		line += " took a seat"
	} else {
		line += " took seats"
	}
	if left := mr.round.Cfg.MaxSeats - len(mr.round.Players); left > 0 {
		line += fmt.Sprintf(" — %s left", plural(left, "seat"))
	}
	line += "."
	// The workaround lands here rather than in the opening line because you cannot
	// repeat a move you have not made yet: these are the people about to make their
	// first one. Once — twice inside one join window would be nagging.
	if !mr.saidRepeatHint {
		mr.saidRepeatHint = true
		line += " Repeating a move? Say it differently: !gou."
	}
	return line, true
}

// lockedOutLine names whoever is still racing without a key at the instant the
// last one leaves the board, and tells them the one thing that can change it.
//
// Decision 13 guarantees somebody is in this position at three players or more,
// and the round never used to say a word to them — every other callout is about a
// player who just did something, so the person the scarcity is aimed at plays two
// minutes in silence. Said once, and phrased as a route back in rather than as
// the bot pointing at whoever is losing.
func (mr *mazeRound) lockedOutLine() string {
	if mr.saidLastKey {
		return ""
	}
	var names []string
	for _, p := range mr.round.Players {
		if p.Racing() && !p.HasKey {
			names = append(names, "@"+p.Display)
		}
	}
	if len(names) == 0 {
		return ""
	}
	mr.saidLastKey = true
	return strings.Join(names, " ") + ": spikes are your way back in — they make someone drop one."
}

// record appends a cycle's events to the permanent log. Caller holds mazeMu.
//
// Kept separate from announce, which is named for narration and already does
// enough: this is the archive, and the two want different things from the same
// events. It denormalizes the player's name because the archive should carry the
// name as it was at the time — people rename — and because users is written only
// by the economy and is empty when that is switched off.
func (mr *mazeRound) record(evs []maze.Event) {
	for _, e := range evs {
		kind, at, reason := e.Wire()
		row := store.MazeEvent{
			Seq: len(mr.log), Cycle: mr.round.Cycle,
			Kind: kind, Seat: e.Seat, At: at, N: e.N, Reason: reason,
		}
		if p := mr.player(e.Seat); p != nil {
			row.UserID, row.Login, row.Display = p.UserID, p.Login, p.Display
		}
		mr.log = append(mr.log, row)
	}
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
	line, ok := mazeview.FeedLine(e, name)
	if !ok {
		return
	}
	mr.feed = append(mr.feed, line)
	if len(mr.feed) > mazeview.FeedLines {
		mr.feed = mr.feed[len(mr.feed)-mazeview.FeedLines:]
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
		return mazeview.Ordinal(place)
	}
}

func (mr *mazeRound) seatsLockedLine(keys int) string {
	// Each name carries its colour, so a player can find their own dot on a board
	// where everyone is a small square. This line is the only seat confirmation
	// chat gets — joins are coalesced rather than answered one by one — so it is
	// where the colour has to be said.
	names := make([]string, 0, len(mr.round.Players))
	for _, p := range mr.round.Players {
		_, emoji := mazeview.Seat(p.Seat)
		names = append(names, emoji+" @"+p.Display)
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
		return "🧭 Nobody joined — round cancelled. !getout to try again."
	}
	places := mr.round.Placements()
	if len(places) == 0 {
		var stranded []string
		for _, p := range mr.round.Players {
			stranded = append(stranded, "@"+p.Display)
		}
		return fmt.Sprintf("⏱ Nobody made it out. %s — the maze wins. !getout to go again.",
			strings.Join(stranded, " "))
	}
	parts := make([]string, len(places))
	for i, p := range places {
		parts[i] = fmt.Sprintf("%s %s", mazeview.Ordinal(p.Place), p.Display)
	}
	line := "🏁 Round over — " + strings.Join(parts, ", ") + "."
	if left := len(mr.round.Players) - len(places); left > 0 {
		line += fmt.Sprintf(" %d left behind in the dark.", left)
	}
	return line + " !getout to go again."
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
	r.retireMaze()
}

// retireMaze frees the stage and clears the board, unless a new round has already
// begun.
//
// The stage token and the overlay cannot be touched while holding mazeMu —
// claimBoard takes its own lock and must never nest inside a per-game one — so
// there is a gap between dropping the finished round and tearing its board down.
// A !maze landing in that gap would otherwise have its brand-new round hidden and
// un-staged by the old one's teardown. Re-checking narrows that to nothing worth
// worrying about; the new round owns the stage it just claimed.
func (r *Router) retireMaze() {
	r.mazeMu.Lock()
	superseded := r.maze != nil
	r.mazeMu.Unlock()
	if superseded {
		return
	}
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
		// A skipped round still happened, so it is still archived — with its own
		// reason, because the engine never reached PhaseDone and has none to give.
		r.archiveMaze(mr, "skipped")
	}
	r.retireMaze()
	if mr != nil {
		r.sendMaze(mr.roomID, "🛑 Maze skipped. !maze to play again.")
	}
}

// --- persistence ------------------------------------------------------------

// mazeRec is the persisted round: the engine's own state document plus the two
// things it has no business knowing — the channel and how the board is drawn.
type mazeRec struct {
	RoomID string `json:"roomID"`
	TickMS int64  `json:"tickMs"`
	// ResolveMS is the beat between turns, carried for the same reason TickMS is: a
	// round resumed after a restart plays by the rules it started under, and editing
	// maze.toml mid-round must not re-pace a game already in progress.
	//
	// A pointer because zero is a meaningful setting — a round deliberately played
	// with no beat — and must stay distinguishable from a document written before
	// this field existed, which should fall back to the configured default. This is
	// the same trap LoadMazeConfig calls out for spikes and key_deficit.
	ResolveMS *int64          `json:"resolveMs,omitempty"`
	Display   string          `json:"display"`
	Round     maze.RoundState `json:"round"`
	// Opening is the round as it began, carried here for the same reason the
	// accumulating log is: a bot that restarts mid-round has to be able to archive
	// a replayable document afterwards, and by then the only moment the opening
	// state existed is long past.
	Opening *maze.RoundState `json:"opening,omitempty"`

	// The accumulating archive. Carried here rather than buffered in memory so a
	// bot that restarts mid-round still has the first half of the game to log when
	// it ends. Worst case this adds a few tens of KB to a document that is
	// rewritten every cycle, which is nothing for either backend but is the reason
	// the feed is deliberately *not* here — that one is narration, this one is the
	// record.
	Log   []store.MazeEvent        `json:"log,omitempty"`
	Moves []mazearchive.Submission `json:"moves,omitempty"`
}

// openingRec is the stored opening state, or a zero one when the document predates
// it. Zero is the signal archive uses to leave Opening out rather than write a
// state that was never real.
func openingRec(rec mazeRec) maze.RoundState {
	if rec.Opening == nil {
		return maze.RoundState{}
	}
	return *rec.Opening
}

// openingOf is mr.opening as a pointer, or nil when the round has none — which
// only happens for a round that was already in flight when this was added.
func openingOf(mr *mazeRound) *maze.RoundState {
	if mr.opening.StartedAt == 0 {
		return nil
	}
	opening := mr.opening
	return &opening
}

// persistMaze mirrors the round to the store. Caller holds mazeMu.
func (r *Router) persistMaze(mr *mazeRound) {
	resolveMS := mr.cfg.ResolveBuffer.Milliseconds()
	rec := mazeRec{
		RoomID:    mr.roomID,
		TickMS:    mr.cfg.Tick.Milliseconds(),
		ResolveMS: &resolveMS,
		Opening:   openingOf(mr),
		Display:   mr.cfg.Display,
		Round:     mr.round.State(),
		Log:       mr.log,
		Moves:     mr.moves,
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
		// A round that finished in the instant the bot died. Its events are sitting
		// in the stored document and would be thrown away here, so archive first —
		// this is the easiest of the end paths to forget, because nothing about it
		// looks like a round ending.
		done := &mazeRound{
			round: round, cfg: r.mazeConfig(), roomID: rec.RoomID,
			stop: make(chan struct{}), saidBounce: map[int]bool{},
			log: rec.Log, moves: rec.Moves,
		}
		if rec.Opening != nil {
			done.opening = *rec.Opening
		}
		if rec.TickMS > 0 {
			done.cfg.Tick = time.Duration(rec.TickMS) * time.Millisecond
		}
		if rec.ResolveMS != nil {
			done.cfg.ResolveBuffer = time.Duration(*rec.ResolveMS) * time.Millisecond
		}
		r.archiveMaze(done, "")
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
	if rec.ResolveMS != nil {
		cfg.ResolveBuffer = time.Duration(*rec.ResolveMS) * time.Millisecond
	}
	if rec.Display != "" {
		cfg.Display = rec.Display
	}
	cfg.Round = round.Cfg // the round plays by the rules it started under

	mr := &mazeRound{
		round: round, cfg: cfg, roomID: rec.RoomID,
		stop: make(chan struct{}), saidBounce: map[int]bool{},
		opening: openingRec(rec),
		log:     rec.Log, moves: rec.Moves,
	}
	// Assigning r.maze without mazeMu is safe only because loadMaze runs at
	// startup, before the IRC loop and before any game timer exists. Don't move
	// this call later without taking the lock.
	r.maze = mr
	r.pushMaze(r.mazePayload(mr))
	go r.runMaze(mr)
}

// --- overlay ----------------------------------------------------------------

// mazePayload builds the render state for this round. Caller holds mazeMu.
//
// Everything the board can be derived from lives on the round itself; what it
// cannot know — which round this is, how it is being displayed, where the turn
// timer stands — is what this supplies. cmd/maze-replay calls the same builder
// with the same three answers taken from a stored round.
func (r *Router) mazePayload(mr *mazeRound) mazeview.Board {
	return mazeview.Build(mr.round, mazeview.Options{
		RoundID:     mr.id(),
		Display:     mr.cfg.Display,
		TickMS:      mr.cfg.Tick.Milliseconds(),
		CycleMsLeft: mr.cycleMsLeft(),
		Feed:        mr.feed,
	})
}

// cycleMsLeft is how long until the current cycle resolves. Clamped at zero so an
// overrunning cycle cannot hand the page a negative bar, and falling back to a
// whole tick before anything has ticked — a zero there would read as a turn that
// is already over.
// The whole remainder is reported, beat included, and the renderer is what caps it
// at a turn. Capping here instead looks equivalent and is not: the page samples
// this once per push and then runs its own clock, so a pre-capped value simply
// starts an 8-second countdown one second early. Only something with a live clock
// can hold the bar full while the beat plays, and that is the page.
func (mr *mazeRound) cycleMsLeft() int64 {
	if mr.cycleEndsAt.IsZero() {
		return mr.cfg.cyclePeriod().Milliseconds()
	}
	if ms := time.Until(mr.cycleEndsAt).Milliseconds(); ms > 0 {
		return ms
	}
	return 0
}

func (r *Router) pushMaze(b mazeview.Board) {
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
