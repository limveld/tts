# 09 — maze.toml config surface

Status: resolved
Blocked by: —

New `maze.toml` for game rules. Payouts stay in `points.toml`.

It does **not** follow the "file present = feature enabled" pattern this issue
originally called for; see the comments for why.

Full surface and defaults are in the PRD. The three that carry real reasoning and
should be commented in the file itself:

- `tick_seconds = 8` — sized so the stream-delay round trip fits inside one
  cycle. Measure the channel's actual OBS-to-viewer delay and tune.
- `loop_walls = 4` — *extra* walls removed beyond the 2-connectivity guarantee.
  It is not a dead-end percentage; see PRD decision 5 for why that dial was
  dropped.
- `deficit_min_players = 3` — below this, keys = N and nobody is locked out.

Should be reloadable via the existing `mise run reload` path, not restart-only.

## Comments

`bot/maze_config.go` holds `LoadMazeConfig`, wired through `Config.Maze` and onto
`Router.mazeCfg` in main. `maze.toml` ships in the repo. `mise run reload`
restarts the bot, so the file is read at startup and nothing needed hot-reloading.

### A missing file means defaults, not "off"

This issue said to follow sfx.toml/points.toml, where an absent file disables the
feature. That is wrong here. Those features cannot function unconfigured — there
are no sounds to play, no economy to run — whereas the maze has a complete,
measured ruleset and works out of the box. Requiring a file to enable it would
mean a fresh checkout has a dead `!maze`.

So the loader returns defaults for a missing file. A file that is present but
*wrong* is fatal, which is the other half of the same idea: the operator edits
this and runs `mise run reload`, so a bad value stops the bot in front of them
instead of surfacing as a `!maze` that mysteriously will not open a round
mid-stream.

### The shipped file has every setting commented out

Deliberate, and the opposite of points.toml. A file that restated every default
would pin that install to today's values — and the next time a default is improved
they would silently not get it. `placement_cycles` has already been wrong once in
exactly that way. The shipped file is documentation with the defaults shown;
uncomment only what you are changing. `TestShippedMazeTomlIsAllDefaults` keeps it
that way.

### Three deviations from the bot's usual config idiom, each earning its keep

- **Decoded over a pre-filled struct**, not merged with the `orInt64(v, def)`
  helper used elsewhere. Several settings have a meaningful zero — `spikes = 0` is
  a board with no spikes, `key_deficit = 0` is a round where nobody is locked out
  — and "0 means unset" would silently ignore exactly the values an operator chose
  most deliberately. `TestMazeConfigHonoursMeaningfulZeros` covers it.
- **Unknown keys are rejected.** `tick_second = 5` would otherwise be a setting
  that looks applied and is not, which is the worst kind of config bug because the
  file and the game disagree with nobody to notice.
- **Validation builds an actual board** rather than re-stating the generator's
  rules. A config that cannot produce a maze is precisely the config that would
  fail on the first `!maze`; asking the generator is both shorter and honest.

### Two things the surface changed

- **Key slots are derived from `max_seats`**, not configured. Writing them
  separately let them drift below the seat cap, and the engine *clamps* rather
  than complains — so the deficit would quietly deepen instead of erroring. One
  per seat is the most any head count can call for, and the engine trims the
  surplus at lock.
- **`seed` is an integer, 0 meaning random**, rather than the PRD's empty string.
  Same behaviour, better typed.

### Verified

Six mutations — unknown-key rejection, derived slots, validation, the
build-a-board check, the missing-file path, and the shipped file pinning an old
default — all caught. Full suite under `mise run test:all`.
