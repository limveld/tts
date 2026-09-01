package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"tts/internal/maze"
)

// maze.toml: the Torch Maze's rules and pacing.
//
// Payouts are deliberately not here. They live in points.toml with every other
// game's rewards, because what a win is worth belongs to the economy rather than
// to the maze — and an operator turning the economy off should not have to edit
// two files.
//
// Unlike sfx.toml or points.toml, a missing file disables nothing. Those features
// cannot function unconfigured: there are no sounds to play, no economy to run.
// The maze ships with a complete, measured ruleset and works out of the box, so
// this file is for changing the game, not for switching it on.

// LoadMazeConfig reads path over the shipping defaults. A missing file is not an
// error; anything the file does not mention keeps its default.
//
// A file that is present but wrong *is* an error, and a fatal one. The operator
// edits this and runs `mise run reload`, which restarts the bot — so a bad value
// surfaces there, in front of them, rather than as a !maze that mysteriously
// refuses to open a round halfway through a stream.
func LoadMazeConfig(path string) (mazeConfig, error) {
	cfg := defaultMazeConfig()
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return cfg, nil
	}

	// The document is pre-filled with the defaults and decoded over, rather than
	// decoded into a zero struct and merged with an orInt-style helper. Several
	// of these fields have a meaningful zero — `spikes = 0` is a board with no
	// spikes, `key_deficit = 0` is a round where nobody is locked out — and the
	// "0 means unset" idiom used elsewhere in the bot would silently ignore
	// exactly the settings an operator is most likely to have chosen on purpose.
	doc := struct {
		TickSeconds     int `toml:"tick_seconds"`
		MaxCycles       int `toml:"max_cycles"`
		MaxSeconds      int `toml:"max_seconds"`
		JoinCycles      int `toml:"join_cycles"`
		PlacementCycles int `toml:"placement_cycles"`
		MaxSeats        int `toml:"max_seats"`

		MapSize   int   `toml:"map_size"`
		LoopWalls int   `toml:"loop_walls"`
		Seed      int64 `toml:"seed"`

		KeyDeficit        int `toml:"key_deficit"`
		DeficitMinPlayers int `toml:"deficit_min_players"`
		KeysMin           int `toml:"keys_min"`
		KeyBandMin        int `toml:"key_band_min"`
		KeyBandMax        int `toml:"key_band_max"`

		Spikes         int `toml:"spikes"`
		BearTraps      int `toml:"bear_traps"`
		BearTrapCycles int `toml:"bear_trap_cycles"`

		Display string `toml:"display"`
	}{
		TickSeconds:     int(cfg.Tick / time.Second),
		MaxCycles:       cfg.Round.MaxCycles,
		MaxSeconds:      cfg.Round.MaxSeconds,
		JoinCycles:      cfg.Round.JoinCycles,
		PlacementCycles: cfg.Round.PlacementCycles,
		MaxSeats:        cfg.Round.MaxSeats,

		MapSize:   cfg.Gen.Size,
		LoopWalls: cfg.Gen.LoopWalls,
		Seed:      cfg.Seed,

		KeyDeficit:        cfg.Round.KeyDeficit,
		DeficitMinPlayers: cfg.Round.DeficitMinPlayers,
		KeysMin:           cfg.Round.KeysMin,
		KeyBandMin:        cfg.Gen.KeyBandMin,
		KeyBandMax:        cfg.Gen.KeyBandMax,

		Spikes:         cfg.Gen.Spikes,
		BearTraps:      cfg.Gen.BearTraps,
		BearTrapCycles: cfg.Round.BearTrapCycles,

		Display: cfg.Display,
	}

	md, err := toml.DecodeFile(path, &doc)
	if err != nil {
		return mazeConfig{}, err
	}
	// A misspelled key would otherwise be a setting that appears to have been
	// applied and silently was not — the worst kind of configuration bug, because
	// the file says one thing and the game does another.
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, len(undecoded))
		for i, k := range undecoded {
			keys[i] = k.String()
		}
		return mazeConfig{}, fmt.Errorf("unknown setting(s): %s", strings.Join(keys, ", "))
	}

	cfg.Tick = time.Duration(doc.TickSeconds) * time.Second
	cfg.Display = strings.ToLower(strings.TrimSpace(doc.Display))
	cfg.Seed = doc.Seed
	cfg.Round = maze.RoundConfig{
		JoinCycles:        doc.JoinCycles,
		MaxSeats:          doc.MaxSeats,
		MaxCycles:         doc.MaxCycles,
		MaxSeconds:        doc.MaxSeconds,
		PlacementCycles:   doc.PlacementCycles,
		KeyDeficit:        doc.KeyDeficit,
		DeficitMinPlayers: doc.DeficitMinPlayers,
		KeysMin:           doc.KeysMin,
		BearTrapCycles:    doc.BearTrapCycles,
	}
	cfg.Gen = maze.Config{
		Size:       doc.MapSize,
		LoopWalls:  doc.LoopWalls,
		Keys:       mazeKeySlots(doc.MaxSeats),
		KeyBandMin: doc.KeyBandMin,
		KeyBandMax: doc.KeyBandMax,
		Spikes:     doc.Spikes,
		BearTraps:  doc.BearTraps,
	}

	if err := cfg.validate(); err != nil {
		return mazeConfig{}, err
	}
	return cfg, nil
}

// mazeKeySlots is how many key positions the generator places, derived from the
// seat cap rather than configured separately.
//
// One per seat is the most any head count can call for — at two players the
// deficit does not apply and keys equal players — and the engine trims the
// surplus when seats lock, before a key is ever rendered. Placing too few would
// not error: the engine clamps the key count to the slots that exist, so the
// deficit would silently deepen. Deriving it means max_seats and the board cannot
// disagree.
func mazeKeySlots(maxSeats int) int { return maxSeats }

// validate rejects a config that cannot produce a playable round.
func (c mazeConfig) validate() error {
	switch {
	case c.Tick < time.Second:
		return fmt.Errorf("tick_seconds %v: must be at least 1", c.Tick/time.Second)
	case c.Display != "panel" && c.Display != "full":
		return fmt.Errorf("display %q: must be \"panel\" or \"full\"", c.Display)
	case c.Round.MaxSeats < 1:
		return fmt.Errorf("max_seats %d: must be at least 1", c.Round.MaxSeats)
	case c.Round.MaxCycles < 1:
		return fmt.Errorf("max_cycles %d: must be at least 1", c.Round.MaxCycles)
	case c.Round.MaxSeconds < 1:
		return fmt.Errorf("max_seconds %d: must be at least 1", c.Round.MaxSeconds)
	case c.Round.JoinCycles < 1:
		return fmt.Errorf("join_cycles %d: must be at least 1, or seats never lock", c.Round.JoinCycles)
	case c.Round.PlacementCycles < 0:
		return fmt.Errorf("placement_cycles %d: cannot be negative", c.Round.PlacementCycles)
	case c.Round.KeyDeficit < 0:
		return fmt.Errorf("key_deficit %d: cannot be negative", c.Round.KeyDeficit)
	case c.Round.KeysMin < 0:
		return fmt.Errorf("keys_min %d: cannot be negative", c.Round.KeysMin)
	case c.Round.BearTrapCycles < 0:
		return fmt.Errorf("bear_trap_cycles %d: cannot be negative", c.Round.BearTrapCycles)
	}

	// The two round limits are checked independently above, and independently they
	// both look fine — which is how a 10s tick with 60 cycles and a 320s guard
	// boots cleanly and then ends every round at cycle 30 with the overlay still
	// showing "CYCLE 30 / 60". Join ticks burn wall clock without advancing the
	// cycle counter, so cycle N lands at (join_cycles + N) x tick.
	if need := (c.Round.JoinCycles + c.Round.MaxCycles) * int(c.Tick/time.Second); c.Round.MaxSeconds < need {
		return fmt.Errorf(
			"max_seconds %d is below %d: %d cycles plus a %d-cycle join window at %.0fs each needs that long, so the wall-clock guard would end every round early",
			c.Round.MaxSeconds, need, c.Round.MaxCycles, c.Round.JoinCycles, c.Tick.Seconds())
	}

	// The board's own rules are the generator's to enforce, and the honest way to
	// ask is to build one: a config that cannot produce a board is exactly the
	// config that would fail on the first !maze. Doing it here means it fails at
	// startup instead, in front of whoever just edited the file.
	if _, err := maze.Generate(1, c.Gen); err != nil {
		return fmt.Errorf("map: %w", err)
	}
	return nil
}
