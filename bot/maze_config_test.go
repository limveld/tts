package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func writeMazeToml(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "maze.toml")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestMazeConfigDefaultsWhenAbsent(t *testing.T) {
	got, err := LoadMazeConfig(filepath.Join(t.TempDir(), "nope.toml"))
	if err != nil {
		t.Fatalf("a missing file must not be an error: %v", err)
	}
	if want := defaultMazeConfig(); !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v want the built-in defaults %+v", got, want)
	}
}

// TestShippedMazeTomlIsAllDefaults keeps the file in the repo honest. It ships
// with every setting commented out on purpose: a file that restated the defaults
// would pin an install to today's values, so the next time one is improved —
// placement_cycles has already been wrong once — that install would silently keep
// the old one.
func TestShippedMazeTomlIsAllDefaults(t *testing.T) {
	got, err := LoadMazeConfig("../maze.toml")
	if err != nil {
		t.Fatalf("the shipped maze.toml does not load: %v", err)
	}
	if want := defaultMazeConfig(); !reflect.DeepEqual(got, want) {
		t.Errorf("the shipped maze.toml changes a default; it should only document them:\n got %+v\nwant %+v", got, want)
	}
}

func TestMazeConfigOverridesOnlyWhatItSets(t *testing.T) {
	p := writeMazeToml(t, `
tick_seconds = 5
display      = "panel"
map_size     = 7
`)
	got, err := LoadMazeConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if got.Tick != 5*time.Second {
		t.Errorf("Tick=%v want 5s", got.Tick)
	}
	if got.Display != "panel" {
		t.Errorf("Display=%q want panel", got.Display)
	}
	if got.Gen.Size != 7 {
		t.Errorf("map size=%d want 7", got.Gen.Size)
	}
	// Everything untouched keeps its default.
	def := defaultMazeConfig()
	if got.Round.MaxCycles != def.Round.MaxCycles || got.Round.PlacementCycles != def.Round.PlacementCycles {
		t.Errorf("unset fields drifted: %+v", got.Round)
	}
	if got.Gen.Spikes != def.Gen.Spikes {
		t.Errorf("spikes=%d want the default %d", got.Gen.Spikes, def.Gen.Spikes)
	}
}

// TestMazeConfigHonoursMeaningfulZeros is why this loader decodes over a
// pre-filled struct instead of using the bot's usual "0 means unset" helper.
// Several of these settings have a real zero — a board with no spikes, a round
// where nobody is locked out — and the usual idiom would silently ignore exactly
// the values an operator chose most deliberately.
func TestMazeConfigHonoursMeaningfulZeros(t *testing.T) {
	p := writeMazeToml(t, `
spikes           = 0
bear_traps       = 0
key_deficit      = 0
loop_walls       = 0
placement_cycles = 0
seed             = 0
`)
	got, err := LoadMazeConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		got  int
	}{
		{"spikes", got.Gen.Spikes},
		{"bear_traps", got.Gen.BearTraps},
		{"key_deficit", got.Round.KeyDeficit},
		{"loop_walls", got.Gen.LoopWalls},
		{"placement_cycles", got.Round.PlacementCycles},
	} {
		if c.got != 0 {
			t.Errorf("%s=%d — an explicit zero was ignored", c.name, c.got)
		}
	}
}

// TestMazeConfigDerivesKeySlotsFromSeats: writing the slot count separately would
// let it drift below the seat cap, and the engine clamps rather than complains —
// so the deficit would quietly deepen instead of erroring.
func TestMazeConfigDerivesKeySlotsFromSeats(t *testing.T) {
	p := writeMazeToml(t, "max_seats = 3\n")
	got, err := LoadMazeConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if got.Gen.Keys != 3 {
		t.Errorf("key slots=%d want one per seat (3)", got.Gen.Keys)
	}
}

// TestMazeConfigRejectsUnknownKeys: a misspelled setting that silently does
// nothing is the worst kind of config bug, because the file says one thing and
// the game does another.
func TestMazeConfigRejectsUnknownKeys(t *testing.T) {
	p := writeMazeToml(t, "tick_second = 5\n") // missing the s
	_, err := LoadMazeConfig(p)
	if err == nil {
		t.Fatal("a misspelled key was accepted")
	}
	if !strings.Contains(err.Error(), "tick_second") {
		t.Errorf("error %q should name the offending key", err)
	}
}

func TestMazeConfigRejectsBadSyntax(t *testing.T) {
	if _, err := LoadMazeConfig(writeMazeToml(t, "tick_seconds = = 5\n")); err == nil {
		t.Error("malformed TOML was accepted")
	}
}

// TestMazeConfigRejectsUnplayableValues. Each of these is fatal at startup rather
// than a !maze that refuses to open a round mid-stream.
func TestMazeConfigRejectsUnplayableValues(t *testing.T) {
	cases := []struct{ name, body string }{
		{"zero tick", "tick_seconds = 0"},
		{"unknown display", `display = "hud"`},
		{"no seats", "max_seats = 0"},
		{"no cycles", "max_cycles = 0"},
		{"no wall clock", "max_seconds = 0"},
		{"seats never lock", "join_cycles = 0"},
		{"negative placement", "placement_cycles = -1"},
		{"nobody can move", "queue_max = 0"},
		{"negative deficit", "key_deficit = -1"},
		{"negative keys_min", "keys_min = -1"},
		{"negative bear trap", "bear_trap_cycles = -1"},
		// Caught by actually building a board rather than by mirroring the
		// generator's rules here.
		{"board too small", "map_size = 2"},
		{"inverted key band", "key_band_min = 6\nkey_band_max = 3"},
		{"board too crowded", "map_size = 3\nmax_seats = 5\nspikes = 4\nbear_traps = 4"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := LoadMazeConfig(writeMazeToml(t, c.body+"\n")); err == nil {
				t.Fatal("want an error, got nil")
			}
		})
	}
}

// TestMazeConfigSeedFixesTheBoard covers the rematch case end to end: the same
// seed must deal the same maze.
func TestMazeConfigSeedFixesTheBoard(t *testing.T) {
	p := writeMazeToml(t, "seed = 424242\n")
	cfg, err := LoadMazeConfig(p)
	if err != nil {
		t.Fatal(err)
	}

	var first string
	for i := 0; i < 3; i++ {
		r, _, _, _ := econRouter(t)
		r.overlay = &fakeOverlay{}
		r.mazeCfg = cfg
		r.Handle(emsg("alice", "!maze", false))
		mr := r.maze
		if mr == nil {
			t.Fatal("round did not start")
		}
		mr.halt()
		got := mr.round.Map.Start.String() + "/" + mr.round.Map.Exit.String()
		for _, k := range mr.round.Map.Keys {
			got += "/" + k.String()
		}
		if i == 0 {
			first = got
			continue
		}
		if got != first {
			t.Fatalf("round %d dealt %s, want the same board as %s", i+1, got, first)
		}
	}

	// And with no seed set, boards differ. This reuses one router on purpose: a
	// fresh one from econRouter starts from a fixed RNG seed, so every first draw
	// would be identical and the test would fail on the harness rather than on the
	// code. Production seeds from the clock.
	cfg.Seed = 0
	r, _, _, _ := econRouter(t)
	r.overlay = &fakeOverlay{}
	r.mazeCfg = cfg
	seen := map[string]bool{}
	for i := 0; i < 6; i++ {
		r.Handle(emsg("alice", "!maze", false))
		if r.maze == nil {
			t.Fatalf("round %d did not start", i+1)
		}
		seen[r.maze.round.Map.Start.String()+"/"+r.maze.round.Map.Exit.String()] = true
		r.forceEndMaze()
	}
	if len(seen) == 1 {
		t.Error("every unseeded round dealt the same board")
	}
}
