# 06 — Overlay renderer

Status: resolved
Blocked by: 02

Bot pushes `Push("maze", ...)`; add `"maze"` to `stateKinds` in
`server/overlay.go:48` so the state is accepted **and cached for replay** — an
OBS scene switch or browser-source reconnect mid-round must restore the board,
not show an empty page.

## Rendering

Expand the Go-side cell grid (4-bit wall masks) to a `(2n+1)` block tile grid in
JS at draw time. `image-rendering: pixelated`, scoped under `#maze` so it does
not leak into the shared stylesheet. A pixel font is a separate decision — the
page currently loads JetBrains Mono and Cinzel only.

## Cell states

- **unknown** — black
- **frontier** — an opening leads here so it is known reachable; dim, walls unknown
- **revealed** — full walls and contents

Objectives (exit, keys) are drawn from cycle 0 regardless of fog. Unsprung traps
are never drawn; a sprung trap's tile stays revealed and inert.

## HUD

One row per player: seat colour swatch, username, live status token (moving /
🔑 has key / 🐻 stuck ×N / 🏁 2nd). Big cycle countdown and cycle/round counter.
Flash the row of whoever just did something.

**Locked-move indicator only** — a ready state and an "n/5 locked" counter, never
the chosen direction (PRD decision 7).

## Stacking

All players spawn on the same cell. Render stacked sprites in **fixed sub-slots
by seat index** so a player's dot sits in a consistent relative position and they
can track themselves.

## Display mode

`display: "panel" | "full"` rides in the pushed payload (the overlay holds no
state of its own), so a mod command flips it mid-round without a restart. Panel
mode sits with the other games; full mode claims the stage. Default `full` —
panel mode is hard to read on a phone at 6x6 with five sprites.

## Also

A1–F6 coordinate labels on the board, so chat callouts can name locations and
viewers can talk about the map.

## Comments

Four changes: `"maze"` added to `stateKinds` in `server/overlay.go`; a `#maze`
container in `server/web/overlay/index.html`; the styles and `renderMaze` appended
to `overlay.css` / `overlay.js`; and `Press Start 2P` added to the existing Google
Fonts request.

The pixel face is used for the maze's chrome only — title, cycle counter,
countdown, status tokens. Usernames stay in JetBrains Mono, because Press Start 2P
is very wide and a long name would break the HUD row.

Both display modes work. `full` centres on the stage at a 34px tile; `panel`
tucks into the same top-right corner the other games use at 17px. `#maze` sits
outside `#top-right` rather than in the stack, which is safe because the board
arbiter guarantees no other game is up at the same time.

### Verified in a browser, not just in tests

The server was built and run locally, a real payload generated through the actual
`mazePayload` code path, pushed over the real SSE transport, and the result
screenshotted. That caught two things no unit test would have:

- **The palette was unreadable and I nearly shipped it twice.** Explored floor
  (`#0f1420`) sat within a few points of unexplored fog (`#05060a`), so the one
  thing the whole fog mechanic exists to show off — where people have actually
  been — was invisible. It is now a deliberate brightness ramp: black for
  unexplored, dark stone for walls, and the lit corridor as the brightest thing on
  the board, which is also the metaphor the game is named for.
- **The stylesheet is `go:embed`-ed, so CSS edits do nothing until the server is
  rebuilt.** Both palette fixes appeared to change nothing because the running
  binary still held the original file. Anyone iterating on this must rebuild
  between edits; a browser reload is not enough. Reading the computed styles back
  out of the page is what exposed it — the screenshots alone looked plausible.

### What the renderer withholds

It can only draw what the bot sends, and the bot deliberately withholds unsprung
traps, the walls of unwalked cells, and the direction of a queued move (see issue
04). Nothing here should try to reconstruct any of them.

Objectives are the deliberate exception and are drawn straight through the fog:
without that, nobody would know there was a scarce key to race for, and the
mechanic the game is built around could never fire.

### Not covered by tests

`TestOverlayMazeStateIsCachedForReplay` pins the Go side — that `"maze"` is
accepted and, more importantly, cached and replayed, since a five-minute round
must survive an OBS scene switch. It was mutation-checked by removing the
whitelist entry.

The JS itself has no test harness in this repo and none was added for it. It was
verified by driving the real transport and reading the resulting DOM back
(13×13 grid, 5 markers, 4 runner dots, 12 axis labels, wall/floor/fog/frontier
counts summing correctly).
