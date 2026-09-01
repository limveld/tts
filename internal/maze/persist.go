package maze

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// This file is the on-disk form of a round, so the bot can restore an in-flight
// game after a restart instead of stranding whoever was mid-race.
//
// Two rules shape it.
//
// The board is stored, not regenerated from its seed. Seeded regeneration would
// be smaller, but it reproduces the original board only if the generator config
// *and* the generator code are both byte-identical to when the round started —
// and neither is guaranteed, since maze.toml gets edited between restarts and a
// redeploy can change the carve. When it went wrong it would go wrong silently,
// resuming players inside walls with the keys somewhere else, and it would do so
// exactly when someone was deploying a fix mid-stream. The whole board is a few
// hundred bytes; that is not a saving worth a failure mode. The seed rides along
// for display and rematches.
//
// Enums cross the wire as strings, never as their iota values. Inserting a new
// Phase or EndReason constant in the middle would otherwise silently reinterpret
// every already-stored round, which is the kind of bug that only shows up in
// production and cannot be reasoned about from the record itself. Cells travel
// as their chat coordinates ("C4") for the same reason the store's schema keeps
// room_id and ends_at as real columns: a round should be legible in psql without
// decoding anything.

// RoundState is a Round flattened for storage. It is JSON-serialisable and
// carries everything needed to resume: the board, the ruleset in force, and all
// mutable state.
type RoundState struct {
	Board BoardState  `json:"board"`
	Cfg   RoundConfig `json:"cfg"`

	Phase  string `json:"phase"`
	Reason string `json:"reason,omitempty"`
	Cycle  int    `json:"cycle"`

	StartedAt  int64 `json:"startedAt"` // unix millis
	JoinLeft   int   `json:"joinLeft,omitempty"`
	Finished   int   `json:"finished,omitempty"`
	EndAtCycle int   `json:"endAtCycle,omitempty"` // cycle the placement window closes on

	Players  []PlayerState `json:"players"`
	Keys     []string      `json:"keys"`             // unclaimed keys still on the board
	Sprung   []int         `json:"sprung,omitempty"` // indices into Board.Traps
	Revealed []string      `json:"revealed"`
	Frontier []string      `json:"frontier"`
}

// BoardState is a Map flattened for storage.
type BoardState struct {
	Size  int         `json:"size"`
	Seed  int64       `json:"seed"`
	Walls []uint8     `json:"walls"` // one mask per cell, indexed y*Size+x
	Start string      `json:"start"`
	Exit  string      `json:"exit"`
	Keys  []string    `json:"keys"` // every slot the generator placed, pre-deficit
	Traps []TrapState `json:"traps"`
	Door  string      `json:"door,omitempty"`
}

// TrapState is one hazard flattened for storage.
type TrapState struct {
	At   string `json:"at"`
	Kind string `json:"kind"` // "spike" | "bear"
}

// PlayerState is one seated chatter flattened for storage.
type PlayerState struct {
	Seat    int    `json:"seat"`
	UserID  string `json:"userID"`
	Login   string `json:"login"`
	Display string `json:"display"`

	At            string `json:"at"`
	HasKey        bool   `json:"hasKey,omitempty"`
	StuckFor      int    `json:"stuckFor,omitempty"`
	Place         int    `json:"place,omitempty"`
	FinishedCycle int    `json:"finishedCycle,omitempty"`

	Queue    []string `json:"queue,omitempty"`
	QueuedAt int64    `json:"queuedAt,omitempty"` // unix millis
}

// --- wire vocabularies ------------------------------------------------------
//
// Kept separate from the String methods on purpose: those are display text and
// may be reworded, while these are a storage format and may not.

var phaseWire = map[Phase]string{PhaseJoining: "joining", PhaseRacing: "racing", PhaseDone: "done"}

var endReasonWire = map[EndReason]string{
	EndNotOver:         "",
	EndAbandoned:       "abandoned",
	EndPlacementClosed: "placements-closed",
	EndAllFinished:     "all-finished",
	EndNobodyCanFinish: "no-keys-left",
	EndCycleCap:        "cycle-cap",
	EndTimeCap:         "time-cap",
}

var trapKindWire = map[TrapKind]string{Spike: "spike", BearTrap: "bear"}

var dirWire = map[Dir]string{North: "up", East: "right", South: "down", West: "left"}

// invert builds a wire-to-value lookup, so each vocabulary is written once and
// cannot drift between the two directions.
func invert[K comparable, V comparable](m map[K]V) map[V]K {
	out := make(map[V]K, len(m))
	for k, v := range m {
		out[v] = k
	}
	return out
}

var (
	phaseFromWire     = invert(phaseWire)
	endReasonFromWire = invert(endReasonWire)
	trapKindFromWire  = invert(trapKindWire)
	dirFromWire       = invert(dirWire)
)

// ParseCell reads a chat coordinate ("C4") back into a Cell. It is the inverse
// of Cell.String.
func ParseCell(s string) (Cell, error) {
	if len(s) < 2 {
		return Cell{}, fmt.Errorf("cell %q: too short", s)
	}
	col := strings.ToUpper(s[:1])[0]
	if col < 'A' || col > 'Z' {
		return Cell{}, fmt.Errorf("cell %q: column must be a letter", s)
	}
	row, err := strconv.Atoi(s[1:])
	if err != nil || row < 1 {
		return Cell{}, fmt.Errorf("cell %q: row must be a positive number", s)
	}
	return Cell{X: int(col - 'A'), Y: row - 1}, nil
}

// --- save -------------------------------------------------------------------

// State flattens r for storage. It is a pure read; calling it never disturbs a
// live round.
func (r *Round) State() RoundState {
	s := RoundState{
		Board:      boardState(r.Map),
		Cfg:        r.Cfg,
		Phase:      phaseWire[r.Phase],
		Reason:     endReasonWire[r.Reason],
		Cycle:      r.Cycle,
		StartedAt:  r.startedAt.UnixMilli(),
		JoinLeft:   r.joinLeft,
		Finished:   r.finished,
		EndAtCycle: r.endAtCycle,
		Keys:       cellStrings(r.keys),
		Revealed:   markedCells(r.Map, r.revealed),
		Frontier:   markedCells(r.Map, r.frontier),
	}
	for i, sprung := range r.sprung {
		if sprung {
			s.Sprung = append(s.Sprung, i)
		}
	}
	for _, p := range r.Players {
		ps := PlayerState{
			Seat: p.Seat, UserID: p.UserID, Login: p.Login, Display: p.Display,
			At: p.At.String(), HasKey: p.HasKey, StuckFor: p.StuckFor,
			Place: p.Place, FinishedCycle: p.FinishedCycle,
		}
		for _, d := range p.queue {
			ps.Queue = append(ps.Queue, dirWire[d])
		}
		if !p.queuedAt.IsZero() {
			ps.QueuedAt = p.queuedAt.UnixMilli()
		}
		s.Players = append(s.Players, ps)
	}
	return s
}

func boardState(m *Map) BoardState {
	b := BoardState{
		Size:  m.Size,
		Seed:  m.Seed,
		Walls: append([]uint8(nil), m.walls...),
		Start: m.Start.String(),
		Exit:  m.Exit.String(),
		Keys:  cellStrings(m.Keys),
	}
	for _, t := range m.Traps {
		b.Traps = append(b.Traps, TrapState{At: t.At.String(), Kind: trapKindWire[t.Kind]})
	}
	if m.Door != nil {
		b.Door = m.Door.String()
	}
	return b
}

func cellStrings(cells []Cell) []string {
	out := make([]string, 0, len(cells))
	for _, c := range cells {
		out = append(out, c.String())
	}
	return out
}

func markedCells(m *Map, flags []bool) []string {
	var out []string
	for i, on := range flags {
		if on {
			out = append(out, m.cellAt(i).String())
		}
	}
	return out
}

// --- restore ----------------------------------------------------------------

// Restore rebuilds a Round from stored state, validating as it goes.
//
// It is strict on purpose. The caller's contract, shared with the other games in
// this bot, is that an unreadable round is dropped rather than resurrected —
// there is no safe way to settle a game whose state cannot be trusted. So every
// error here is a decision to lose one round, which is much cheaper than
// resuming a corrupt one and paying out on it.
func Restore(s RoundState) (*Round, error) {
	m, err := restoreBoard(s.Board)
	if err != nil {
		return nil, err
	}
	phase, ok := phaseFromWire[s.Phase]
	if !ok {
		return nil, fmt.Errorf("phase %q: unknown", s.Phase)
	}
	reason, ok := endReasonFromWire[s.Reason]
	if !ok {
		return nil, fmt.Errorf("end reason %q: unknown", s.Reason)
	}
	if s.Cycle < 0 {
		return nil, fmt.Errorf("cycle %d: negative", s.Cycle)
	}

	n := m.Size * m.Size
	r := &Round{
		Map:        m,
		Cfg:        s.Cfg,
		Phase:      phase,
		Reason:     reason,
		Cycle:      s.Cycle,
		startedAt:  time.UnixMilli(s.StartedAt),
		joinLeft:   s.JoinLeft,
		finished:   s.Finished,
		endAtCycle: s.EndAtCycle,
		seats:      make(map[string]int, len(s.Players)),
		trapIdx:    make(map[int]int, len(m.Traps)),
		sprung:     make([]bool, len(m.Traps)),
		revealed:   make([]bool, n),
		frontier:   make([]bool, n),
	}
	for i, t := range m.Traps {
		r.trapIdx[m.idx(t.At)] = i
	}
	for _, i := range s.Sprung {
		if i < 0 || i >= len(m.Traps) {
			return nil, fmt.Errorf("sprung trap %d: out of range (%d traps)", i, len(m.Traps))
		}
		r.sprung[i] = true
	}

	if r.keys, err = parseCells(m, s.Keys, "key"); err != nil {
		return nil, err
	}
	if err := setFlags(m, r.revealed, s.Revealed, "revealed"); err != nil {
		return nil, err
	}
	if err := setFlags(m, r.frontier, s.Frontier, "frontier"); err != nil {
		return nil, err
	}

	for i, ps := range s.Players {
		p, err := restorePlayer(m, i, ps)
		if err != nil {
			return nil, err
		}
		if _, dup := r.seats[p.UserID]; dup {
			return nil, fmt.Errorf("player %q is seated twice", p.UserID)
		}
		r.seats[p.UserID] = p.Seat
		r.Players = append(r.Players, p)
	}
	if s.Finished > len(r.Players) {
		return nil, fmt.Errorf("%d finishers but %d players", s.Finished, len(r.Players))
	}
	return r, nil
}

func restoreBoard(b BoardState) (*Map, error) {
	if b.Size < 3 {
		return nil, fmt.Errorf("board size %d: too small", b.Size)
	}
	if len(b.Walls) != b.Size*b.Size {
		return nil, fmt.Errorf("board has %d wall masks, want %d for a %dx%d board",
			len(b.Walls), b.Size*b.Size, b.Size, b.Size)
	}
	m := &Map{Size: b.Size, Seed: b.Seed, walls: append([]uint8(nil), b.Walls...)}

	// Wall state is stored once per side of each wall. If the two copies ever
	// disagree, a player can step one way through a wall and not back, which
	// would be a maddening thing to debug from a bug report — so it is checked
	// here, where it is one pass and a clear error.
	for i := range m.walls {
		c := m.cellAt(i)
		for _, d := range dirs {
			n, ok := m.Neighbor(c, d)
			if !ok {
				if m.walls[i]&d.bit() == 0 {
					return nil, fmt.Errorf("cell %v is open through the board edge", c)
				}
				continue
			}
			if m.Open(c, d) != m.Open(n, d.opposite()) {
				return nil, fmt.Errorf("wall between %v and %v disagrees with itself", c, n)
			}
		}
	}

	var err error
	if m.Start, err = cellIn(m, b.Start, "start"); err != nil {
		return nil, err
	}
	if m.Exit, err = cellIn(m, b.Exit, "exit"); err != nil {
		return nil, err
	}
	if m.Keys, err = parseCells(m, b.Keys, "board key"); err != nil {
		return nil, err
	}
	for _, ts := range b.Traps {
		at, err := cellIn(m, ts.At, "trap")
		if err != nil {
			return nil, err
		}
		kind, ok := trapKindFromWire[ts.Kind]
		if !ok {
			return nil, fmt.Errorf("trap kind %q: unknown", ts.Kind)
		}
		m.Traps = append(m.Traps, Trap{At: at, Kind: kind})
	}
	if b.Door != "" {
		door, err := cellIn(m, b.Door, "door")
		if err != nil {
			return nil, err
		}
		m.Door = &door
	}
	return m, nil
}

func restorePlayer(m *Map, i int, ps PlayerState) (*Player, error) {
	if ps.Seat != i {
		return nil, fmt.Errorf("player %d has seat %d; seats must match their order", i, ps.Seat)
	}
	if ps.UserID == "" {
		return nil, fmt.Errorf("player %d has no user id", i)
	}
	at, err := cellIn(m, ps.At, fmt.Sprintf("player %d position", i))
	if err != nil {
		return nil, err
	}
	p := &Player{
		Seat: ps.Seat, UserID: ps.UserID, Login: ps.Login, Display: ps.Display,
		At: at, HasKey: ps.HasKey, StuckFor: ps.StuckFor,
		Place: ps.Place, FinishedCycle: ps.FinishedCycle,
	}
	for _, w := range ps.Queue {
		d, ok := dirFromWire[w]
		if !ok {
			return nil, fmt.Errorf("player %d has %q queued: not a direction", i, w)
		}
		p.queue = append(p.queue, d)
	}
	if ps.QueuedAt != 0 {
		p.queuedAt = time.UnixMilli(ps.QueuedAt)
	}
	return p, nil
}

func cellIn(m *Map, s, what string) (Cell, error) {
	c, err := ParseCell(s)
	if err != nil {
		return Cell{}, fmt.Errorf("%s: %w", what, err)
	}
	if !m.InBounds(c) {
		return Cell{}, fmt.Errorf("%s %v: off a %dx%d board", what, c, m.Size, m.Size)
	}
	return c, nil
}

func parseCells(m *Map, ss []string, what string) ([]Cell, error) {
	var out []Cell
	for _, s := range ss {
		c, err := cellIn(m, s, what)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

func setFlags(m *Map, flags []bool, ss []string, what string) error {
	for _, s := range ss {
		c, err := cellIn(m, s, what)
		if err != nil {
			return err
		}
		flags[m.idx(c)] = true
	}
	return nil
}
