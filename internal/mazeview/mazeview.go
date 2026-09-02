// Package mazeview turns a maze round into what the overlay draws.
//
// It exists because two things need to draw the same game and must not disagree
// about how: the bot, pushing a live round, and cmd/maze-replay, pushing a stored
// one. Both send this payload to the same endpoint and it is rendered by the same
// code, so a replay that looks different from the game is a bug in one of them —
// which is only a useful property while there is exactly one definition of the
// payload. A second copy would drift, and the drift would show up as a replay
// quietly misrepresenting the very round you are replaying.
//
// It holds no state and knows nothing about chat, the store or the clock. What it
// cannot derive from the round — which round this is, how it is being displayed,
// where the turn timer stands — arrives in Options.
package mazeview

import (
	"fmt"

	"tts/internal/maze"
)

// seats is the seat palette: the colour a runner is drawn in, and the emoji chat
// is told to look for.
//
// Both halves live here, together, because they are one fact. The overlay used to
// own the hex and chat would have owned the emoji, which is two copies of the same
// palette that can disagree — and when they disagree the roster does not look
// wrong, it just quietly sends a player hunting for somebody else's dot. The
// renderer takes the colour from the payload, so there is nothing to keep in step.
var seats = []struct {
	Hex   string
	Emoji string
}{
	{"#e6482e", "🔴"},
	{"#4fa4ff", "🔵"},
	{"#3fbf5f", "🟢"},
	{"#e8c547", "🟡"},
	{"#b96fe0", "🟣"},
}

// Seat is seat n's colour and emoji, wrapping round if there are ever more seats
// than colours.
func Seat(n int) (hex, emoji string) {
	s := seats[((n%len(seats))+len(seats))%len(seats)]
	return s.Hex, s.Emoji
}

// Cell is one cell as the overlay sees it. Walls are sent only for revealed cells:
// an unrevealed cell's layout is the thing the fog exists to withhold, so it must
// not travel to the renderer at all rather than be sent and hidden in CSS.
type Cell struct {
	State string `json:"state"`           // unknown | frontier | revealed
	Walls uint8  `json:"walls,omitempty"` // N/E/S/W bitmask; revealed cells only
}

// Player is one runner's HUD row.
type Player struct {
	Seat     int    `json:"seat"`
	Name     string `json:"name"`
	At       string `json:"at"`
	HasKey   bool   `json:"hasKey,omitempty"`
	StuckFor int    `json:"stuckFor,omitempty"`
	Place    int    `json:"place,omitempty"`
	// Locked says a move is in; Move says which way.
	//
	// The direction was originally withheld, on the grounds that a viewer on a
	// fast connection could read a slower player's intent off the board and
	// counter it inside the same turn. Playtesting overruled it: on a stream this
	// size that interception is theoretical, while "did my command register, and
	// which way am I about to go" is a question people have every single turn.
	Locked bool   `json:"locked,omitempty"`
	Move   string `json:"move,omitempty"`
	// Color is the seat's swatch, sent rather than looked up in the renderer so
	// that it and the emoji chat was told are one palette. See seats.
	Color string `json:"color"`
}

// Trap is a sprung trap. Unsprung ones never appear here — see Build.
type Trap struct {
	At   string `json:"at"`
	Kind string `json:"kind"`
}

// Board is the render payload.
type Board struct {
	// RoundID identifies the round this board belongs to. The renderer keeps a
	// little per-round state — which sprung traps it has already faded out, which
	// finishers are mid-fade — and used to spot a new round by watching the cycle
	// counter go backwards. That is inference, and it fails in the cases that
	// matter: a bot killed mid-round never sends the hidden push that would have
	// cleared it, and two rounds are both briefly at cycle zero.
	RoundID   string `json:"roundId"`
	Display   string `json:"display"`
	Phase     string `json:"phase"`
	Cycle     int    `json:"cycle"`
	MaxCycles int    `json:"maxCycles"`
	TickMS    int64  `json:"tickMs"`
	// CycleMsLeft is how much of the current cycle is actually left. The page used
	// to count tickMs down from whenever a payload arrived, and payloads arrive on
	// every move as well as every cycle, so the bar restarted each time somebody
	// typed and never reached zero — the turn looked like it was skipping with time
	// still on it.
	//
	// Milliseconds remaining rather than an absolute instant on purpose: the server
	// and the machine running OBS need not be the same one (docs/obs-overlay.md) and
	// their clocks need not agree, whereas the flight time between them is small.
	//
	// It counts the whole cycle, beat included; the renderer caps what it *draws*
	// at one turn, because only something with a live clock can hold the bar full
	// while the beat plays.
	CycleMsLeft int64 `json:"cycleMsLeft"`
	// EndsAtCycle is the cycle the placement window closes on, 0 until somebody
	// escapes. MaxCycles stops being the deadline that binds the moment it is set:
	// the header counted toward a cap of 60 on a round that was in fact ending at
	// 26, telling everyone racing for second they had forty turns to do it in.
	EndsAtCycle int      `json:"endsAtCycle,omitempty"`
	Size        int      `json:"size"`
	Cells       []Cell   `json:"cells"` // row-major, y*Size+x
	Start       string   `json:"start"`
	Exit        string   `json:"exit"`
	Keys        []string `json:"keys"`  // unclaimed keys, drawn through the fog
	Traps       []Trap   `json:"traps"` // sprung traps only
	Players     []Player `json:"players"`
	Seats       int      `json:"seats"`
	Feed        []string `json:"feed,omitempty"`
}

// Options is everything about a board that the round itself does not know.
type Options struct {
	RoundID     string
	Display     string // "panel" | "full"
	TickMS      int64
	CycleMsLeft int64
	Feed        []string
}

// Build renders a round for the overlay.
//
// Two things are deliberately withheld. Unsprung traps never leave the bot — they
// are the one genuine surprise in the game, and a payload that carried them would
// put them one devtools panel away. Neither do the walls of cells nobody has
// walked into yet. Objectives are the exception and are always sent: the fog hides
// the maze's shape, not where you are trying to get to.
func Build(rd *maze.Round, o Options) Board {
	b := Board{
		RoundID:     o.RoundID,
		Display:     o.Display,
		Phase:       rd.Phase.String(),
		Cycle:       rd.Cycle,
		MaxCycles:   rd.Cfg.MaxCycles,
		TickMS:      o.TickMS,
		CycleMsLeft: o.CycleMsLeft,
		EndsAtCycle: rd.EndAtCycle(),
		Size:        rd.Map.Size,
		Start:       rd.Map.Start.String(),
		Exit:        rd.Map.Exit.String(),
		Seats:       rd.Cfg.MaxSeats,
		Cells:       make([]Cell, 0, rd.Map.Size*rd.Map.Size),
		Feed:        append([]string(nil), o.Feed...),
	}
	for y := 0; y < rd.Map.Size; y++ {
		for x := 0; x < rd.Map.Size; x++ {
			c := maze.Cell{X: x, Y: y}
			switch {
			case rd.Revealed(c):
				b.Cells = append(b.Cells, Cell{State: "revealed", Walls: rd.Map.Walls(c)})
			case rd.Frontier(c):
				b.Cells = append(b.Cells, Cell{State: "frontier"})
			default:
				b.Cells = append(b.Cells, Cell{State: "unknown"})
			}
		}
	}
	for _, k := range rd.KeysOnMap() {
		b.Keys = append(b.Keys, k.String())
	}
	for i, t := range rd.Map.Traps {
		if rd.TrapSprung(i) {
			b.Traps = append(b.Traps, Trap{At: t.At.String(), Kind: t.Kind.String()})
		}
	}
	for _, p := range rd.Players {
		hex, _ := Seat(p.Seat)
		row := Player{
			Seat: p.Seat, Name: p.Display, At: p.At.String(), Color: hex,
			HasKey: p.HasKey, StuckFor: p.StuckFor, Place: p.Place,
		}
		if d, ok := p.NextDir(); ok {
			row.Locked, row.Move = true, d.String()
		}
		b.Players = append(b.Players, row)
	}
	return b
}

// FeedLines is how much play-by-play the panel keeps on screen.
const FeedLines = 5

// FeedLine is the panel's one-glyph version of an event, and false for the events
// the feed does not show. Terse because the panel has room for a handful of lines,
// not sentences — the feed is there so a viewer can see *that* things are
// happening without reading chat.
//
// Pure, and taking the name rather than looking it up, so a replay can rebuild the
// same feed from stored events instead of rendering them a second, different way.
func FeedLine(e maze.Event, name string) (string, bool) {
	switch e.Kind {
	case maze.EventSeatsLocked:
		return fmt.Sprintf("▶ locked — %s", plural(e.N, "key")), true
	case maze.EventKeyTaken:
		line := "🔑 " + name
		if e.N == 0 {
			line += " — last key"
		}
		return line, true
	case maze.EventKeyDropped:
		return "🔑 dropped " + e.At.String(), true
	case maze.EventSpiked:
		return "💀 " + name + " → start", true
	case maze.EventTrapped:
		return fmt.Sprintf("🐻 %s ×%d", name, e.N), true
	case maze.EventFreed:
		return "🔓 " + name, true
	case maze.EventBounced:
		return "🚪 " + name + " — no key", true
	case maze.EventBonked:
		return "🧱 " + name, true
	case maze.EventFinished:
		return fmt.Sprintf("%s %s %s", Medal(e.N), name, Ordinal(e.N)), true
	}
	return "", false
}

// Medal is the glyph for a finishing place. Medals rather than the chequered flag
// this used to use: 🏁 renders as a small grey chequerboard at the panel's token
// size, indistinguishable from a missing glyph, whereas a medal carries its
// meaning in colour, which survives being small. Off the podium there is no medal
// and the door the escape lines already use stands in.
func Medal(place int) string {
	switch place {
	case 1:
		return "🥇"
	case 2:
		return "🥈"
	case 3:
		return "🥉"
	}
	return "🚪"
}

// Ordinal is 1 -> "1st". Lives here rather than in the bot because placements are
// a thing the board says as much as a thing chat says, and both should spell them
// the same way.
func Ordinal(n int) string {
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

// plural is a local copy on purpose. The bot's own lives in bot/info.go and is
// used by half the commands there; pulling a general string helper into a package
// about drawing a maze would point the dependency the wrong way for the sake of
// four characters.
func plural(n int, unit string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, unit)
	}
	return fmt.Sprintf("%d %ss", n, unit)
}
