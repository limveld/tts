package maze

import (
	"sort"
	"time"
)

// This file is the round engine: the authoritative state of one game and the
// cycle that advances it. It holds no chat, overlay or store coupling — a round
// is driven by Join, Submit and Tick, and reports what happened as a slice of
// Events for the caller to turn into chat lines and overlay pushes.
//
// The engine is deliberately free of randomness. Everything variable about a
// round comes from the board (seeded, see maze.go) or from player input (which
// is persisted), so a round resumed after a restart replays identically. Adding
// a coin flip anywhere in here would break that quietly.

// Phase is where a round sits in its lifecycle.
type Phase uint8

const (
	// PhaseJoining is the frozen window at the top of a round: the board is up,
	// seats are filling, nothing moves. It exists because the key count depends
	// on how many people actually joined while the generator has already run, so
	// there has to be one moment where the head count is final and the surplus
	// keys come off the board — before any key has been rendered.
	PhaseJoining Phase = iota
	PhaseRacing
	PhaseDone
)

func (p Phase) String() string {
	switch p {
	case PhaseJoining:
		return "joining"
	case PhaseRacing:
		return "racing"
	default:
		return "done"
	}
}

// EndReason records why a round stopped, so the caller can word the closing
// callout without re-deriving it from the state.
type EndReason uint8

const (
	EndNotOver EndReason = iota
	// EndAbandoned: the join window closed with nobody seated.
	EndAbandoned
	// EndPlacementClosed: someone won and the placement window then ran out.
	EndPlacementClosed
	// EndAllFinished: every seated player got through the door.
	EndAllFinished
	// EndNobodyCanFinish: nobody left holds a key and none remain on the board,
	// so no further finish is possible. Keys only return to play when a
	// key-holder hits spikes, so with no key-holders left this is terminal.
	EndNobodyCanFinish
	EndCycleCap
	EndTimeCap
)

func (e EndReason) String() string {
	switch e {
	case EndAbandoned:
		return "abandoned"
	case EndPlacementClosed:
		return "placements closed"
	case EndAllFinished:
		return "everyone finished"
	case EndNobodyCanFinish:
		return "no keys left in play"
	case EndCycleCap:
		return "cycle cap"
	case EndTimeCap:
		return "time cap"
	default:
		return "not over"
	}
}

// EventKind identifies what happened during a tick.
type EventKind uint8

const (
	// EventSeatsLocked: the join window closed. N is the key count that resolved
	// from the head count.
	EventSeatsLocked EventKind = iota
	// EventBonked: walked into a wall. The move is lost and the queue is cleared,
	// because a plan that has already gone wrong is worse than no plan.
	EventBonked
	// EventBounced: tried to enter the exit without a key. At is where the player
	// still stands. Nothing is revealed — they never entered the cell.
	EventBounced
	// EventKeyTaken: N is how many keys remain on the board.
	EventKeyTaken
	// EventKeyDropped: a spiked key-holder's key landed back on the board at At.
	EventKeyDropped
	// EventSpiked: sent back to the start tile from At.
	EventSpiked
	// EventTrapped: immobilised at At for N cycles.
	EventTrapped
	// EventFreed: a bear-trap counter ran out. Per the resolution order the
	// player is free but does not move until the following cycle.
	EventFreed
	// EventFinished: through the door. N is the finishing place, 1-based.
	EventFinished
	// EventRoundEnded: Reason says why.
	EventRoundEnded
)

func (k EventKind) String() string {
	switch k {
	case EventSeatsLocked:
		return "seats-locked"
	case EventBonked:
		return "bonked"
	case EventBounced:
		return "bounced"
	case EventKeyTaken:
		return "key-taken"
	case EventKeyDropped:
		return "key-dropped"
	case EventSpiked:
		return "spiked"
	case EventTrapped:
		return "trapped"
	case EventFreed:
		return "freed"
	case EventFinished:
		return "finished"
	default:
		return "round-ended"
	}
}

// Event is one thing that happened during a tick. Seat is -1 for round-level
// events. The meaning of N depends on Kind and is documented on each constant.
type Event struct {
	Kind   EventKind
	Seat   int
	At     Cell
	N      int
	Reason EndReason
}

// RoundConfig is the rules half of maze.toml. The defaults live in
// DefaultRoundConfig; the reasoning behind the numbers is in the PRD.
//
// It is part of the persisted round: a round resumed after a restart plays by
// the rules it started under, so editing maze.toml mid-round cannot change the
// scoring of a game already in progress.
type RoundConfig struct {
	JoinCycles int `json:"joinCycles"` // frozen cycles before seats lock
	MaxSeats   int `json:"maxSeats"`
	MaxCycles  int `json:"maxCycles"`
	MaxSeconds int `json:"maxSeconds"` // wall-clock guard against pauses and desync
	// PlacementCycles is how long the round stays live after the first exit so
	// the rest can race for the places behind it.
	//
	// It costs less than it looks. The round ends the moment no unfinished player
	// holds a key and none are left on the board, so a window longer than the
	// field needs simply never runs out — it is an upper bound for stragglers,
	// not a fixed tail. Measured over played-out five-player rounds, a window of
	// 6 produced exactly one finisher every time and 12 produced four, in the
	// same number of cycles as 10. Too small a value does not shorten the round,
	// it just silently deletes second place.
	PlacementCycles int `json:"placementCycles"`

	// KeyDeficit is subtracted from the head count, but only from
	// DeficitMinPlayers upward: the deficit is a fixed absolute number and a
	// variable proportional one, and at two players it would lock out half the
	// field on what is the most common turnout for a small stream.
	KeyDeficit        int `json:"keyDeficit"`
	DeficitMinPlayers int `json:"deficitMinPlayers"`
	KeysMin           int `json:"keysMin"`

	BearTrapCycles int `json:"bearTrapCycles"`
}

// DefaultRoundConfig is the shipping ruleset. TickSeconds is not here because
// the engine never looks at wall-clock pacing except through MaxSeconds — the
// caller owns the timer.
func DefaultRoundConfig() RoundConfig {
	return RoundConfig{
		JoinCycles: 2,
		MaxSeats:   5,
		MaxCycles:  60,
		// Comfortably clear of MaxCycles at the shipping tick: cycle N lands at
		// (JoinCycles+N) x tick, so 62 x 10s = 620s of play. This is a guard
		// against pauses and desync, not a second round limit, and a value below
		// that product silently truncates every round — bot/maze_config.go
		// cross-checks the two for exactly that reason.
		MaxSeconds:        720,
		PlacementCycles:   12,
		KeyDeficit:        1,
		DeficitMinPlayers: 3,
		KeysMin:           1,
		BearTrapCycles:    2,
	}
}

// Player is one seated chatter.
type Player struct {
	Seat    int
	UserID  string
	Login   string
	Display string

	At       Cell
	HasKey   bool
	StuckFor int // bear-trap cycles remaining; 0 means free to move

	Place         int // finishing position, 1-based; 0 while still racing
	FinishedCycle int

	queue    []Dir
	queuedAt time.Time
}

// Racing reports whether p is still in contention.
func (p *Player) Racing() bool { return p.Place == 0 }

// Queued reports whether p has a move locked in for the next cycle.
func (p *Player) Queued() int { return len(p.queue) }

// NextDir is the move p has locked in, if any.
//
// This is shown on the overlay. The original design withheld it: a viewer on a
// fast connection can read a slower player's intent off the board and counter it
// within the same cycle, which is the unfairness the fixed tick exists to remove.
// Playtesting reversed that — on a stream this size the interception is
// theoretical while "did my command register, and which way am I about to go" is
// a question people actually have every cycle.
func (p *Player) NextDir() (Dir, bool) {
	if len(p.queue) == 0 {
		return North, false
	}
	return p.queue[0], true
}

// Round is one game in flight.
type Round struct {
	Map *Map
	Cfg RoundConfig

	Phase  Phase
	Cycle  int
	Reason EndReason

	Players []*Player

	startedAt  time.Time
	joinLeft   int
	finished   int
	endAtCycle int // cycle the placement window closes on; 0 until someone wins

	seats   map[string]int
	keys    []Cell // unclaimed keys on the board
	trapIdx map[int]int
	sprung  []bool

	revealed []bool
	frontier []bool
}

// NewRound opens a round on m. It starts in PhaseJoining with no keys on the
// board: the surplus cannot be removed until the head count is known, and a key
// that appeared and then vanished would be worse than one that appeared late.
func NewRound(m *Map, cfg RoundConfig, startedAt time.Time) *Round {
	n := m.Size * m.Size
	r := &Round{
		Map:       m,
		Cfg:       cfg,
		Phase:     PhaseJoining,
		joinLeft:  cfg.JoinCycles,
		startedAt: startedAt,
		seats:     make(map[string]int, cfg.MaxSeats),
		trapIdx:   make(map[int]int, len(m.Traps)),
		sprung:    make([]bool, len(m.Traps)),
		revealed:  make([]bool, n),
		frontier:  make([]bool, n),
	}
	for i, t := range m.Traps {
		r.trapIdx[m.idx(t.At)] = i
	}
	if r.joinLeft < 0 {
		r.joinLeft = 0
	}
	r.reveal(m.Start)
	return r
}

// Join seats a chatter. Re-joining is idempotent so a second !go from someone
// already seated is a move, not an error. ok is false once seats are locked or
// full — the caller should tell them they are up next round rather than say
// nothing.
func (r *Round) Join(userID, login, display string) (seat int, ok bool) {
	if s, dup := r.seats[userID]; dup {
		return s, true
	}
	if r.Phase != PhaseJoining || len(r.Players) >= r.Cfg.MaxSeats {
		return 0, false
	}
	p := &Player{Seat: len(r.Players), UserID: userID, Login: login, Display: display, At: r.Map.Start}
	r.Players = append(r.Players, p)
	r.seats[userID] = p.Seat
	return p.Seat, true
}

// Submit locks in a player's move for the next cycle, replacing whatever they had
// chosen before — so the most recent message always wins and a correction is never
// half-applied. at is when the message was sent; it is the tie-break when two
// players reach the same key on the same cycle.
//
// One message, one move. An earlier version accepted a path of several moves,
// which existed to sidestep Twitch dropping a message identical to the sender's
// previous one. It was removed because it made a player's first command — the one
// that also took their seat — bank a move they had not meant to make, and they
// then spent the round one cell away from where they believed they were.
//
// Submitting while bear-trapped is allowed. The trap already cleared the move, and
// letting someone line up their escape while stuck is friendlier than swallowing
// the input with no explanation.
func (r *Round) Submit(userID string, d Dir, at time.Time) bool {
	if r.Phase == PhaseDone {
		return false
	}
	s, ok := r.seats[userID]
	if !ok {
		return false
	}
	p := r.Players[s]
	if !p.Racing() {
		return false
	}
	p.queue = []Dir{d}
	p.queuedAt = at
	return true
}

// Tick advances the round one cycle and reports what happened. now is only
// consulted for the wall-clock guard, so tests can drive a whole round with a
// fixed time.
func (r *Round) Tick(now time.Time) []Event {
	switch r.Phase {
	case PhaseDone:
		return nil
	case PhaseJoining:
		if r.joinLeft > 0 {
			r.joinLeft--
		}
		if r.joinLeft > 0 {
			return nil
		}
		return r.lock()
	default:
		return r.race(now)
	}
}

// lock closes the join window and fixes the key count against the real head
// count. Surplus keys are dropped from the end of the generator's list, which is
// a greedy spread — so any prefix of it is itself well spread, and cutting the
// tail cannot bunch the survivors together.
func (r *Round) lock() []Event {
	if len(r.Players) == 0 {
		return r.end(EndAbandoned)
	}
	n := len(r.Players)
	k := n
	if n >= r.Cfg.DeficitMinPlayers {
		k = n - r.Cfg.KeyDeficit
	}
	if k < r.Cfg.KeysMin {
		k = r.Cfg.KeysMin
	}
	if k > len(r.Map.Keys) {
		k = len(r.Map.Keys)
	}
	if k < 0 {
		k = 0
	}
	r.keys = append([]Cell(nil), r.Map.Keys[:k]...)
	r.Phase = PhaseRacing
	return []Event{{Kind: EventSeatsLocked, Seat: -1, N: k}}
}

// race resolves one racing cycle, in the order the PRD fixes: timers, then
// moves, then destinations, then fog, then the end check.
func (r *Round) race(now time.Time) []Event {
	r.Cycle++
	var evs []Event

	// 1. Bear-trap timers. A counter reaching zero frees the player but does not
	// move them: they act from the next cycle, so a trap costs its full count.
	movers := make([]*Player, 0, len(r.Players))
	for _, p := range r.Players {
		switch {
		case !p.Racing():
		case p.StuckFor > 0:
			p.StuckFor--
			if p.StuckFor == 0 {
				evs = append(evs, Event{Kind: EventFreed, Seat: p.Seat, At: p.At})
			}
		default:
			movers = append(movers, p)
		}
	}

	// 2. One queued move each. Players pass through each other, so nothing here
	// depends on the order movers are visited in.
	type landing struct {
		p  *Player
		to Cell
	}
	var landed []landing
	for _, p := range movers {
		if len(p.queue) == 0 {
			continue
		}
		d := p.queue[0]
		p.queue = p.queue[1:]

		if !r.Map.Open(p.At, d) {
			p.queue = nil
			evs = append(evs, Event{Kind: EventBonked, Seat: p.Seat, At: p.At})
			continue
		}
		to, _ := r.Map.Neighbor(p.At, d)
		if to == r.Map.Exit && !p.HasKey {
			p.queue = nil
			evs = append(evs, Event{Kind: EventBounced, Seat: p.Seat, At: p.At})
			continue
		}
		p.At = to
		landed = append(landed, landing{p, to})
	}

	// 3. Destinations. Visited earliest-submission first, which only matters for
	// the one genuinely contested case: two players reaching the same key on the
	// same cycle. Seat order breaks an exact tie so the result stays reproducible.
	sort.SliceStable(landed, func(i, j int) bool {
		a, b := landed[i].p, landed[j].p
		if !a.queuedAt.Equal(b.queuedAt) {
			return a.queuedAt.Before(b.queuedAt)
		}
		return a.Seat < b.Seat
	})

	// A trap fires for everyone who steps on it this cycle, not just whoever the
	// tie-break happens to visit first. They all stepped on it at the same
	// instant; sparing the second player because their message arrived later
	// would be a worse outcome than springing it twice. It despawns afterwards.
	firing := make(map[int]bool)
	for _, l := range landed {
		i := r.Map.idx(l.to)
		if ti, ok := r.trapIdx[i]; ok && !r.sprung[ti] {
			firing[i] = true
		}
	}

	for _, l := range landed {
		p, cell := l.p, l.to
		i := r.Map.idx(cell)

		if cell == r.Map.Exit {
			// Reaching the exit without a key was already turned into a bounce
			// above, so arriving here means the key is spent.
			p.HasKey = false
			p.queue = nil
			r.finished++
			p.Place = r.finished
			p.FinishedCycle = r.Cycle
			if r.finished == 1 {
				r.endAtCycle = r.Cycle + r.Cfg.PlacementCycles
			}
			evs = append(evs, Event{Kind: EventFinished, Seat: p.Seat, At: cell, N: p.Place})
			continue
		}

		if firing[i] {
			switch r.Map.Traps[r.trapIdx[i]].Kind {
			case Spike:
				if p.HasKey {
					// Back onto the board rather than out of play: a key that
					// vanished with its carrier could make the round unwinnable
					// for the locked-out player through nobody's fault.
					p.HasKey = false
					r.keys = append(r.keys, cell)
					evs = append(evs, Event{Kind: EventKeyDropped, Seat: p.Seat, At: cell})
				}
				p.At = r.Map.Start
				p.queue = nil
				evs = append(evs, Event{Kind: EventSpiked, Seat: p.Seat, At: cell})
			case BearTrap:
				p.StuckFor = r.Cfg.BearTrapCycles
				p.queue = nil
				evs = append(evs, Event{Kind: EventTrapped, Seat: p.Seat, At: cell, N: p.StuckFor})
			}
			continue
		}

		if !p.HasKey {
			if j := indexOfCell(r.keys, cell); j >= 0 {
				r.keys = append(r.keys[:j], r.keys[j+1:]...)
				p.HasKey = true
				evs = append(evs, Event{Kind: EventKeyTaken, Seat: p.Seat, At: cell, N: len(r.keys)})
			}
		}
	}
	for i := range firing {
		r.sprung[r.trapIdx[i]] = true
	}

	// 4. Fog. Reveal what was entered, not where players ended up: a spiked
	// player is back at the start, but they did walk into the trap cell and the
	// board should show it.
	for _, l := range landed {
		r.reveal(l.to)
	}

	// 5. End check.
	return append(evs, r.checkEnd(now)...)
}

func (r *Round) checkEnd(now time.Time) []Event {
	switch {
	case r.allFinished():
		return r.end(EndAllFinished)
	case r.endAtCycle > 0 && r.Cycle >= r.endAtCycle:
		return r.end(EndPlacementClosed)
	case !r.finishStillPossible():
		return r.end(EndNobodyCanFinish)
	case r.Cfg.MaxCycles > 0 && r.Cycle >= r.Cfg.MaxCycles:
		return r.end(EndCycleCap)
	case r.Cfg.MaxSeconds > 0 && !now.Before(r.Deadline()):
		return r.end(EndTimeCap)
	}
	return nil
}

func (r *Round) end(reason EndReason) []Event {
	r.Phase, r.Reason = PhaseDone, reason
	return []Event{{Kind: EventRoundEnded, Seat: -1, Reason: reason}}
}

func (r *Round) allFinished() bool {
	for _, p := range r.Players {
		if p.Racing() {
			return false
		}
	}
	return true
}

// finishStillPossible reports whether anyone left can reach the door. Keys
// re-enter play only when a key-holder hits spikes, so once no racer holds one
// and none are on the board, no further finish can ever happen and the round is
// just running down a clock nobody can beat.
func (r *Round) finishStillPossible() bool {
	if len(r.keys) > 0 {
		return true
	}
	for _, p := range r.Players {
		if p.Racing() && p.HasKey {
			return true
		}
	}
	return false
}

// reveal lights c and marks every cell reachable from it as frontier: known to
// exist and known to be reachable, with its own walls still unknown. That middle
// state is what lets a player see where they *could* go without being told what
// they would find there.
func (r *Round) reveal(c Cell) {
	i := r.Map.idx(c)
	r.revealed[i] = true
	r.frontier[i] = false
	for _, d := range dirs {
		if !r.Map.Open(c, d) {
			continue
		}
		n, _ := r.Map.Neighbor(c, d)
		if ni := r.Map.idx(n); !r.revealed[ni] {
			r.frontier[ni] = true
		}
	}
}

func indexOfCell(cells []Cell, c Cell) int {
	for i, x := range cells {
		if x == c {
			return i
		}
	}
	return -1
}

// --- read-only views --------------------------------------------------------

// Revealed reports whether c's walls are known to the board.
func (r *Round) Revealed(c Cell) bool { return r.revealed[r.Map.idx(c)] }

// Frontier reports whether c is known reachable but not yet walked into.
func (r *Round) Frontier(c Cell) bool { return r.frontier[r.Map.idx(c)] }

// KeysOnMap is the unclaimed keys, in no meaningful order. It can hold the same
// cell twice: two key-holders spiked on the same cell in the same cycle both
// drop there.
func (r *Round) KeysOnMap() []Cell { return append([]Cell(nil), r.keys...) }

// TrapSprung reports whether the trap at index i of Map.Traps has fired. A
// sprung trap is inert but its cell stays revealed — the first player through
// clears it for everyone behind them.
func (r *Round) TrapSprung(i int) bool { return r.sprung[i] }

// PlayerBy returns the seated player for a chat user.
func (r *Round) PlayerBy(userID string) (*Player, bool) {
	s, ok := r.seats[userID]
	if !ok {
		return nil, false
	}
	return r.Players[s], true
}

// Deadline is when the wall-clock guard trips.
func (r *Round) Deadline() time.Time {
	return r.startedAt.Add(time.Duration(r.Cfg.MaxSeconds) * time.Second)
}

// EndAtCycle is the cycle the placement window closes on, or 0 while nobody has
// finished yet. It is not the same thing as MaxCycles: once somebody escapes,
// the round ends PlacementCycles later and the cycle cap stops being the limit
// that binds. The overlay needs it to count the scramble down rather than count
// toward a cap the round will never reach.
func (r *Round) EndAtCycle() int { return r.endAtCycle }

// Placements lists everyone who finished, in finishing order.
func (r *Round) Placements() []*Player {
	var out []*Player
	for _, p := range r.Players {
		if !p.Racing() {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Place < out[j].Place })
	return out
}

// --- input parsing ----------------------------------------------------------

// dirNames is the one place a direction's word is written. String and ParseDir
// are both built from it, so they cannot drift into disagreeing about what "left"
// means — a round-trip test pins that.
var dirNames = map[Dir]string{North: "up", East: "right", South: "down", West: "left"}

func (d Dir) String() string { return dirNames[d] }

// ParseDir reads a direction word. It is the inverse of String, and exists
// because chat has several ways to say the same move.
func ParseDir(s string) (Dir, bool) {
	for d, name := range dirNames {
		if name == s {
			return d, true
		}
	}
	return North, false
}
