// Package mazearchive is the format of a stored round and how to start it again.
//
// It sits between the bot, which writes one when a round ends, and
// cmd/maze-replay, which reads it back. Both need the same struct tags and the
// same idea of where a replay begins, and neither can import the other — the bot
// is package main. A second definition would be a second archive format, and the
// failure would be silent: fields that quietly stop round-tripping.
//
// The engine it feeds contains no randomness at all, so a board plus a ruleset
// plus the ordered submissions is a complete description of a game (ADR-0004).
package mazearchive

import (
	"fmt"
	"sort"
	"time"

	"tts/internal/maze"
)

// Submission is one player's move as it was accepted, which together with the
// board is what makes a round replayable. The parsed direction is stored, not the
// chat text: it is what the engine actually consumed, and the raw line is already
// in chat_message for anyone who wants it.
type Submission struct {
	Cycle int    `json:"cycle"`
	Seat  int    `json:"seat"`
	Dir   string `json:"dir"`
	At    int64  `json:"at"` // unix millis
}

// Replay is the document stored in maze_rounds.input.
type Replay struct {
	// Opening is the state the round began in — restore this and replay Moves.
	//
	// Absent from rounds archived before it was recorded. Reconstruct handles
	// those; see its comment for why nothing is actually lost.
	Opening *maze.RoundState `json:"opening,omitempty"`
	// Final is the state at archive time.
	//
	// Its JSON key stays "initial" because every round already in the table uses
	// it, and an archive whose old rows stop decoding is not an archive. The Go
	// name is the honest one: this field was called Initial and documented as the
	// opening state, and it was never either — it holds a finished round, so a
	// replay built on it restored a game that had already ended and played nothing
	// at all, silently, since nothing about a done round errors.
	Final maze.RoundState `json:"initial"`
	Gen   maze.Config     `json:"gen"`
	Moves []Submission    `json:"moves"`
	// Pacing and layout, so a round can be replayed as it was played rather than
	// merely re-simulated. A pointer for ResolveMS because zero is a real setting —
	// a round deliberately played with no beat — and has to stay distinguishable
	// from a document written before the field existed.
	ResolveMS *int64 `json:"resolveMs,omitempty"`
	Display   string `json:"display,omitempty"`
}

// Reconstruct returns the round as it stood before its first turn.
//
// When Opening was recorded this is just a restore. When it was not — every round
// archived before that field existed — it is rebuilt, and can be, because the half
// that matters never changed during the round: the board and the ruleset are
// immutable, and both survive in Final. A fresh round over the same board and
// rules, with the same players seated in the same order, is the same round.
//
// Seat order is load-bearing. A seat number picks a colour and a sub-slot within
// a cell, so joining in a different order produces a board with the right runners
// in the wrong places — which would look like a working replay of a different game.
func Reconstruct(r Replay) (*maze.Round, error) {
	fin, err := maze.Restore(r.Final)
	if err != nil {
		return nil, fmt.Errorf("final state: %w", err)
	}

	rd := fin
	if r.Opening != nil {
		if rd, err = maze.Restore(*r.Opening); err != nil {
			return nil, fmt.Errorf("opening state: %w", err)
		}
	} else {
		rd = maze.NewRound(fin.Map, fin.Cfg, time.UnixMilli(r.Final.StartedAt))
	}

	// Seats come from the final state either way, and have to.
	//
	// The opening state is captured before anybody has joined — that is what makes
	// it the opening — and individual joins are not events, so the roster survives
	// in exactly one place: the finished round. Restoring the opening state and
	// replaying without this produces a board with nobody on it, every submission
	// landing on a seat that does not exist, and a round that ends having gone
	// nowhere. Which is a quieter version of the bug this whole document had.
	seated := append([]*maze.Player(nil), fin.Players...)
	sort.Slice(seated, func(i, j int) bool { return seated[i].Seat < seated[j].Seat })
	for _, p := range seated {
		if _, already := rd.PlayerBy(p.UserID); already {
			continue
		}
		if _, ok := rd.Join(p.UserID, p.Login, p.Display); !ok {
			return nil, fmt.Errorf("seat %d (%s) would not join the rebuilt round", p.Seat, p.Login)
		}
	}
	return rd, nil
}

// Runner feeds a reconstructed round its recorded moves, a turn at a time.
//
// It exists so the replay tool and the test that proves replays are faithful step
// a round the same way. They did not, once, and the difference was invisible:
// both looped over turn numbers, which runs a round short by exactly the join
// window, because join ticks burn a tick without advancing the cycle counter.
type Runner struct {
	moves   []Submission
	applied []bool
	ids     map[int]string

	// now is a synthetic clock, and has to be.
	//
	// A round carries the wall-clock guard from the rules it was played under, and
	// its deadline is measured from the instant it started — which, for anything in
	// the archive, is in the past. Ticking a replay with the real clock trips
	// MaxSeconds on the very first turn and ends the round before it has moved:
	// observed on a stored 35-turn round, which replayed to turn 1 and reported
	// "all-finished" for a game nobody played. Advancing from the original start by
	// one cycle a turn reproduces the guard exactly as it behaved on the night,
	// including for a round that really did hit it.
	now    time.Time
	period time.Duration
}

// NewRunner prepares r's moves for replay, in submission order.
//
// Order within a turn matters: two runners reaching the last key on the same turn
// are separated by who submitted first, so replaying them the other way round
// hands the key to the wrong player and every turn after that diverges. Moves are
// appended in submission order as they arrive, so slice order already is that
// order; the sort makes it true rather than incidental.
// period is how long one cycle took when the round was played: the input window
// plus the resolve beat. It paces the synthetic clock, not the playback — how fast
// a replay is watched is the caller's business.
func NewRunner(r Replay, period time.Duration) *Runner {
	moves := append([]Submission(nil), r.Moves...)
	sort.SliceStable(moves, func(i, j int) bool { return moves[i].At < moves[j].At })
	start := time.UnixMilli(r.Final.StartedAt)
	if r.Opening != nil {
		start = time.UnixMilli(r.Opening.StartedAt)
	}
	if period <= 0 {
		period = time.Second
	}
	return &Runner{
		moves: moves, applied: make([]bool, len(moves)), ids: r.UserIDs(),
		now: start, period: period,
	}
}

// Step submits every move recorded for the turn the round is currently on, then
// advances it, returning what the turn produced.
//
// Driven by the round's own cycle counter rather than by a turn index, which is
// the whole point: the counter is what the moves were recorded against, and it
// does not advance during the join window.
func (x *Runner) Step(rd *maze.Round) ([]maze.Event, error) {
	for i, m := range x.moves {
		if x.applied[i] || m.Cycle != rd.Cycle {
			continue
		}
		d, ok := maze.ParseDir(m.Dir)
		if !ok {
			return nil, fmt.Errorf("turn %d: %q is not a direction", m.Cycle, m.Dir)
		}
		x.applied[i] = true
		rd.Submit(x.ids[m.Seat], d, time.UnixMilli(m.At))
	}
	x.now = x.now.Add(x.period)
	return rd.Tick(x.now), nil
}

// UserIDs maps seat number to the user id the engine keys submissions on, read
// from whichever state carries the seats.
func (r Replay) UserIDs() map[int]string {
	ids := map[int]string{}
	for _, p := range r.Final.Players {
		ids[p.Seat] = p.UserID
	}
	return ids
}
