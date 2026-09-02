// Full-screen stream overlay. Renders purely from the server's SSE stream
// (/overlay/events); it holds no game state of its own and never connects to
// Twitch. Events: play/stop (audio) + gamble/depth/wordle/connections.

// Carry any ?token= from this page's URL onto the API calls (a Browser Source
// can't set headers, so the server accepts the token as a query param).
const token = new URLSearchParams(location.search).get('token');
const q = token ? ('?token=' + encodeURIComponent(token)) : '';

// --- audio (TTS/SFX) --------------------------------------------------------
let current = null;

function playClip(d) {
  const audio = new Audio(d.url + q);
  current = audio;
  // volume: 0-100 percent -> 0-1 (reduce-only; <audio> caps at 1.0).
  if (typeof d.volume === 'number') audio.volume = Math.max(0, Math.min(1, d.volume / 100));
  let acked = false;
  const done = () => {
    if (acked) return; acked = true;
    fetch('/overlay/done' + q, {
      method: 'POST',
      headers: {'Content-Type': 'application/json'},
      body: JSON.stringify({id: d.id}),
      keepalive: true
    }).catch(() => {});
  };
  // trim end: stop and ack once we reach d.end (natural 'ended' also acks).
  if (d.end > 0) {
    audio.addEventListener('timeupdate', () => {
      if (audio.currentTime >= d.end) { audio.pause(); done(); }
    });
  }
  audio.addEventListener('ended', done);
  audio.addEventListener('error', done);
  const play = () => audio.play().catch(e => { console.error('tts play blocked:', e); done(); });
  // trim start: seek before playing so there's no blip from 0 (needs metadata).
  if (d.start > 0) {
    audio.addEventListener('loadedmetadata', () => {
      try { audio.currentTime = d.start; } catch (e) {}
      play();
    }, {once: true});
    audio.load();
  } else {
    play();
  }
}

function stopClip() {
  if (current) { current.pause(); current = null; }
}

// --- gamble panel -----------------------------------------------------------
// Renders {phase:"open"|"result"|"hidden", buyIn, players, pot, endsAt, winner,
// cancelled}. During an open round a local countdown ticks toward endsAt; the
// result flashes the winner (or "cancelled") then fades; hidden clears it.
const gambleEl = document.getElementById('gamble');
let gambleCountdown = null;

function fmtCountdown(ms) {
  if (ms < 0) ms = 0;
  const s = Math.round(ms / 1000);
  return Math.floor(s / 60) + ':' + String(s % 60).padStart(2, '0');
}

function renderGamble(d) {
  if (gambleCountdown) { clearInterval(gambleCountdown); gambleCountdown = null; }

  if (!d || d.phase === 'hidden') {
    gambleEl.hidden = true;
    gambleEl.innerHTML = '';
    return;
  }

  gambleEl.hidden = false;
  gambleEl.classList.remove('fading');

  const sep = '<span class="g-sep">·</span>';
  const tag = '<span class="g-tag">🎲</span>';

  if (d.phase === 'result') {
    const winner = d.cancelled
      ? '<span class="g-winner cancelled">cancelled</span>'
      : '<span class="g-winner">🎉 ' + (d.winner || '') + '</span>';
    gambleEl.innerHTML =
      tag +
      '<span class="g-pot">' + (d.pot || 0) + '</span>' + sep + winner;
    // fade out shortly before the server clears the cached state.
    setTimeout(() => gambleEl.classList.add('fading'), 6000);
    return;
  }

  // phase === 'open' — one line: 🎲 <pot> · <players>🤡 · <m:ss>
  gambleEl.innerHTML =
    tag +
    '<span class="g-pot">' + (d.pot || 0) + '</span>' + sep +
    '<span class="g-players">' + (d.players || 0) + '🤡</span>' + sep +
    '<span class="g-countdown" id="g-countdown"></span>';

  const cd = document.getElementById('g-countdown');
  const tick = () => { cd.textContent = fmtCountdown((d.endsAt || 0) - Date.now()); };
  tick();
  if (d.endsAt) gambleCountdown = setInterval(tick, 250);
}

// --- depth rating -----------------------------------------------------------
// Renders {points, tier, pb} as [depth-tier.png] <points> · PB <pb> in the
// bottom-right. Tier is derived from the same thresholds the bot uses, so the
// payload's tier is only a fallback. Display caps at 9999 to match the game.
const depthEl = document.getElementById('depth');
const DEPTH_THRESHOLDS = [
  {tier: 5, min: 6000},
  {tier: 4, min: 4000},
  {tier: 3, min: 2000},
  {tier: 2, min: 1000},
  {tier: 1, min: 0},
];

function depthTier(points) {
  for (const t of DEPTH_THRESHOLDS) if (points >= t.min) return t.tier;
  return 1;
}

function depthFmt(n) {
  return Math.min(n, 9999); // 9999 display cap
}

function renderDepth(d) {
  if (!d || typeof d.points !== 'number') { depthEl.hidden = true; return; }
  const tier = depthTier(d.points);
  const pb = (typeof d.pb === 'number' && d.pb > 0)
    ? '<span class="d-pb">· PB ' + depthFmt(d.pb) + '</span>' : '';
  depthEl.hidden = false;
  depthEl.innerHTML =
    '<img src="/overlay/images/depth-' + tier + '.png" alt="Depth ' + tier + '">' +
    '<span class="d-points">' + depthFmt(d.points) + '</span>' + pb;
}

// --- wordle -----------------------------------------------------------------
// Renders {rows:[{guess,result}], done, won, max, answer?} as a shared 6x5 board
// plus a colored keyboard. Ported from raw/wordle-chat-overlay.html. {hidden:true}
// clears it.
const wordleEl = document.getElementById('wordle');
const WORDLE_KB = ['QWERTYUIOP', 'ASDFGHJKL', 'ZXCVBNM'];
const WORDLE_RANK = {unused: 0, absent: 1, present: 2, correct: 3};
let wordleCountdown = null;
let wordlePrevRows = 0; // rows shown last render, to detect a freshly-added guess

function renderWordle(d) {
  if (wordleCountdown) { clearInterval(wordleCountdown); wordleCountdown = null; }

  if (!d || d.hidden) {
    wordleEl.hidden = true; wordleEl.innerHTML = ''; wordlePrevRows = 0; return;
  }
  const rows = d.rows || [];
  const max = d.max || 6;
  // Animate only the just-added row (rows grew by exactly one since last render),
  // so replays/resets don't spuriously flash the whole board.
  const newIdx = rows.length === wordlePrevRows + 1 ? rows.length - 1 : -1;
  wordlePrevRows = rows.length;

  // countdown (live rounds only)
  let head = '';
  if (!d.done && d.endsAt) head = '<div class="w-countdown" id="w-countdown"></div>';

  // board
  let board = '<div class="w-board">';
  for (let r = 0; r < max; r++) {
    board += '<div class="w-row' + (r === newIdx ? ' w-row-new' : '') + '">';
    const row = rows[r];
    for (let c = 0; c < 5; c++) {
      if (row) {
        board += '<div class="w-tile ' + row.result[c] + '">' + row.guess[c] + '</div>';
      } else {
        board += '<div class="w-tile"></div>';
      }
    }
    board += '</div>';
  }
  board += '</div>';

  // keyboard letter states (best rank seen across all rows)
  const states = {};
  for (const row of rows) {
    for (let i = 0; i < 5; i++) {
      const L = row.guess[i], s = row.result[i];
      if (WORDLE_RANK[s] > WORDLE_RANK[states[L] || 'unused']) states[L] = s;
    }
  }
  let kb = '<div class="w-keyboard">';
  for (const rowStr of WORDLE_KB) {
    kb += '<div class="w-krow">';
    for (const L of rowStr) kb += '<div class="w-key ' + (states[L] || '') + '">' + L + '</div>';
    kb += '</div>';
  }
  kb += '</div>';

  let banner = '';
  if (d.done) {
    banner = d.won
      ? '<div class="w-banner">SOLVED — ' + (d.answer || '') + '</div>'
      : '<div class="w-banner lost">THE WORD WAS ' + (d.answer || '') + '</div>';
  }

  wordleEl.hidden = false;
  wordleEl.innerHTML = head + board + kb + banner;

  // drive the countdown by updating only the text (no re-render / re-animate).
  if (!d.done && d.endsAt) {
    const cd = document.getElementById('w-countdown');
    const tick = () => { cd.textContent = '⏱ ' + fmtCountdown(d.endsAt - Date.now()); };
    tick();
    wordleCountdown = setInterval(tick, 250);
  }
}

// --- connections ------------------------------------------------------------
// Renders {tiles:[{num,word}], solved:[{name,level,words,revealed}], mistakes,
// max, done, won, endsAt?} as an NYT-style board: solved groups collapse into
// colored category bars above a shrinking 4x4 numbered grid. {hidden:true} clears
// it. The payload never carries the grouping of unsolved tiles, so the answer
// can't leak mid-round.
const connEl = document.getElementById('connections');
const CONN_COLORS = ['c-yellow', 'c-green', 'c-blue', 'c-purple']; // level 0..3
let connCountdown = null;
let connPrevSolved = 0; // solved bars shown last render, to animate a fresh one

function escHtml(s) {
  return String(s).replace(/[&<>"']/g, c => (
    {'&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;'}[c]
  ));
}

function renderConnections(d) {
  if (connCountdown) { clearInterval(connCountdown); connCountdown = null; }

  if (!d || d.hidden) {
    connEl.hidden = true; connEl.innerHTML = ''; connPrevSolved = 0; return;
  }
  const solved = d.solved || [];
  const tiles = d.tiles || [];
  const max = d.max || 4;
  const mistakes = d.mistakes || 0;
  // Animate only a just-solved group (bars grew by exactly one since last render).
  const newBar = solved.length === connPrevSolved + 1 ? solved.length - 1 : -1;
  connPrevSolved = solved.length;

  // header: countdown (live only) + mistake dots
  let dots = '';
  for (let i = 0; i < max; i++) dots += '<span class="cn-dot' + (i < max - mistakes ? '' : ' used') + '"></span>';
  let head = '<div class="cn-head">';
  if (!d.done && d.endsAt) head += '<span class="cn-countdown" id="cn-countdown"></span>';
  head += '<span class="cn-dots">' + dots + '</span></div>';

  // solved (and, when done, revealed) groups as colored bars
  let bars = '';
  for (let i = 0; i < solved.length; i++) {
    const g = solved[i];
    const cls = CONN_COLORS[g.level % 4] + (i === newBar ? ' cn-bar-new' : '') + (g.revealed ? ' cn-revealed' : '');
    bars += '<div class="cn-bar ' + cls + '">' +
      '<span class="cn-name">' + escHtml(g.name) + '</span>' +
      '<span class="cn-words">' + (g.words || []).map(escHtml).join(' · ') + '</span></div>';
  }

  // remaining tiles as a numbered grid, in display order
  let grid = '<div class="cn-grid">';
  for (const t of tiles) {
    grid += '<div class="cn-tile"><span class="cn-num">' + t.num + '</span>' +
      '<span class="cn-word">' + escHtml(t.word) + '</span></div>';
  }
  grid += '</div>';

  let banner = '';
  if (d.done) {
    banner = d.won
      ? '<div class="cn-banner">SOLVED</div>'
      : '<div class="cn-banner lost">BUSTED</div>';
  }

  connEl.hidden = false;
  connEl.innerHTML = head + bars + grid + banner;

  // drive the countdown by updating only the text (no re-render / re-animate).
  if (!d.done && d.endsAt) {
    const cd = document.getElementById('cn-countdown');
    const tick = () => { cd.textContent = '⏱ ' + fmtCountdown(d.endsAt - Date.now()); };
    tick();
    connCountdown = setInterval(tick, 250);
  }
}

// --- notification toasts (bottom-left) --------------------------------------
// Transient {kind:"shoutout"|"ad", line1, line2?, avatar?}. Shown one at a time
// (slide-in, hold 5s, fade out); queued so bursts don't overlap. Built via DOM
// APIs so names/game text can't inject markup.
const notifyEl = document.getElementById('notify');
const notifyQueue = [];
let notifyShowing = false;

function enqueueNotify(d) {
  notifyQueue.push(d);
  if (!notifyShowing) showNextNotify();
}

function showNextNotify() {
  const d = notifyQueue.shift();
  if (!d) { notifyShowing = false; return; }
  notifyShowing = true;

  const toast = document.createElement('div');
  toast.className = 'toast enter' + (d.kind === 'ad' ? ' ad' : '');

  if (d.kind === 'shoutout' && d.avatar) {
    const img = document.createElement('img');
    img.className = 't-avatar';
    img.src = d.avatar;
    img.alt = '';
    toast.appendChild(img);
  } else if (d.kind === 'shoutout') {
    const icon = document.createElement('div');
    icon.className = 't-icon';
    icon.textContent = '📢';
    toast.appendChild(icon);
  }
  // ad: line1 already carries its 📺, so no separate media element.

  const body = document.createElement('div');
  body.className = 't-body';
  const l1 = document.createElement('div');
  l1.className = 't-line1';
  l1.textContent = d.line1 || '';
  body.appendChild(l1);
  if (d.line2) {
    const l2 = document.createElement('div');
    l2.className = 't-line2';
    l2.textContent = d.line2;
    body.appendChild(l2);
  }
  toast.appendChild(body);
  notifyEl.appendChild(toast);

  setTimeout(() => {
    toast.classList.add('leaving');
    setTimeout(() => { toast.remove(); showNextNotify(); }, 500);
  }, 5000);
}

// --- torch maze -------------------------------------------------------------
// The bot sends a grid of cells carrying wall bitmasks; this expands it to a
// (2n+1) square of blocks, which is what makes it read as an arcade maze rather
// than a diagram. Cells the bot withheld simply have nothing to draw: unsprung
// traps and the walls of unwalked cells never arrive, so the fog cannot be
// undone by reading the page.
const mazeEl = document.getElementById('maze');
let mazeTimer = null;
let mazePrev = null; // previous payload, for change flashes

// Seat colours come from the payload, not from here. Chat is told which colour a
// player is when they take a seat, and a second copy of the palette in this file
// would be free to drift from that one — sending people to look for the wrong dot
// without anything appearing broken. See mazeSeats in bot/maze.go.
const MAZE_FALLBACK_SEAT = '#8d97b5';

// How long a sprung trap stays on the board before fading out. Only sprung traps
// are ever sent — an armed one would give the game away — so an icon here means
// the hazard is already spent, and leaving it up forever tells people a safe cell
// is dangerous. Roughly half a cycle at the shipping tick: long enough to be seen
// through the stream delay, gone before the next move matters.
const MAZE_TRAP_FADE = 6000;

// When each sprung trap was first seen, so the fade survives a re-render. The
// board is redrawn on every move as well as every cycle, and a CSS animation on a
// fresh element restarts from the top — without this the icon would flash back to
// full opacity each time somebody typed.
let mazeTrapSeen = {};
// Rotations, not four separate glyphs. The thin Unicode arrows this replaced were
// only really legible pointing up: ↓ ← → are near-identical strokes at the panel's
// eleven pixels, and the one with a vertical stem was the only one anybody could
// read at a glance. A filled triangle keeps its shape and weight at any size, and
// turning a single glyph guarantees all four directions read identically rather
// than depending on how one font happens to draw each arrow.
const MAZE_ARROWS = {up: 0, right: 90, down: 180, left: 270};

// mazeArrow is the locked-in direction as a rotated triangle, or nothing when no
// move is in. Compared against undefined rather than tested for truth, because up
// is zero degrees.
function mazeArrow(move) {
  const deg = MAZE_ARROWS[move];
  if (deg === undefined) return '';
  return '<i class="m-arrow" style="transform:rotate(' + deg + 'deg)">\u25B2</i>';
}
// Placements are medals rather than a chequered flag. At the token's size the flag
// resolves to a small grey chequerboard, which reads as a missing glyph rather than
// as a finish; a medal carries its meaning in colour, which survives being small.
// Off the podium there is no medal and the ordinal stands on its own.
const MAZE_MEDALS = {1: '🥇', 2: '🥈', 3: '🥉'};

// How long a runner takes to travel one cell. It has to fit inside the resolve
// beat the bot leaves at the top of each cycle (resolve_seconds), so the movement
// has landed before anyone is being asked to choose their next one.
const MAZE_STEP_MS = 380;
// How long a finisher stays on the board, sliding into the door and fading, rather
// than blinking out at the payoff moment of the round.
const MAZE_EXIT_FADE = 900;
// Below this, a measured difference is rounding rather than movement. Rects come
// back fractional, and a dot mid-warp is being scaled, so its centre survives the
// arithmetic a hair off — enough to make `dx !== 0` true and slide a runner that
// had not moved at all. A real step is 64px in the corner panel and 128px on the
// stage, so half a pixel is nowhere near anything the game can produce.
const MAZE_MIN_SLIDE = 0.5;
// When each finisher was first seen, so the fade survives the re-render on every
// move — same reason as mazeTrapSeen, and cleared with it.
let mazeExitSeen = {};

function mazeCell(s) {
  if (!s || s.length < 2) return null;
  const x = s.charCodeAt(0) - 65;
  const y = parseInt(s.slice(1), 10) - 1;
  return (x >= 0 && y >= 0) ? {x: x, y: y} : null;
}

function mazeOrdinal(n) {
  const t = n % 100;
  if (t > 10 && t < 14) return n + 'th';
  return n + (['th', 'st', 'nd', 'rd'][n % 10] || 'th');
}

// mazeAt positions an element over the grid in tile units, so several runners can
// share a cell without the grid reflowing.
function mazeAt(col, row, cls, body) {
  return '<span class="' + cls + '" style="left:calc(var(--m-tile)*' + col.toFixed(3) +
         ');top:calc(var(--m-tile)*' + row.toFixed(3) + ')">' + body + '</span>';
}

// mazeBlocks turns cells into the block grid. Wall bits are N=1 E=2 S=4 W=8.
function mazeBlocks(d, prev) {
  const n = d.size, span = 2 * n + 1;
  const t = new Array(span * span).fill('unknown');
  const at = (col, row) => row * span + col;

  // The outer wall is drawn from the first cycle. It gives the arena a shape to
  // read before anything is explored and gives away nothing — the boundary is
  // closed on every board there is.
  for (let i = 0; i < span; i++) {
    t[at(i, 0)] = 'wall'; t[at(i, span - 1)] = 'wall';
    t[at(0, i)] = 'wall'; t[at(span - 1, i)] = 'wall';
  }

  const fresh = {};
  for (let y = 0; y < n; y++) {
    for (let x = 0; x < n; x++) {
      const c = d.cells[y * n + x] || {};
      const col = 2 * x + 1, row = 2 * y + 1;

      if (c.state === 'revealed') {
        t[at(col, row)] = 'floor';
        const w = c.walls || 0;
        const edge = (i, walled) => {
          // A wall known from either side stays a wall; the two cells sharing it
          // always agree, so this only guards against ordering.
          if (walled) t[i] = 'wall';
          else if (t[i] !== 'wall') t[i] = 'floor';
        };
        edge(at(col, row - 1), w & 1);
        edge(at(col + 1, row), w & 2);
        edge(at(col, row + 1), w & 4);
        edge(at(col - 1, row), w & 8);
        // Corner posts next to a known cell are structural, never passable.
        [[-1, -1], [1, -1], [-1, 1], [1, 1]].forEach(o => {
          const i = at(col + o[0], row + o[1]);
          if (t[i] === 'unknown') t[i] = 'wall';
        });
        const was = prev && prev.cells && prev.cells[y * n + x];
        if (was && was.state !== 'revealed') fresh[at(col, row)] = true;
      } else if (c.state === 'frontier' && t[at(col, row)] === 'unknown') {
        t[at(col, row)] = 'frontier';
      }
    }
  }
  return {span: span, tiles: t, fresh: fresh};
}

// mazeRows is the seat list: swatch, name, what the player is carrying, and the
// direction they have locked in. Shared by both layouts rather than copied into
// each, because two copies of this would drift and the panel's is the one nobody
// would be looking at when it did.
function mazeRows(d, players, prev) {
  let rows = '<div class="m-rows">';
  players.forEach(p => {
    const wasP = prev && (prev.players || []).find(q => q.seat === p.seat);
    // Flash a row whose state actually moved. The payload carries no event list,
    // so the change itself is the signal.
    const moved = wasP && (wasP.at !== p.at || wasP.hasKey !== p.hasKey ||
                           wasP.stuckFor !== p.stuckFor || wasP.place !== p.place);
    let token = '·';
    if (p.place) token = (MAZE_MEDALS[p.place] || '') + mazeOrdinal(p.place);
    else if (p.stuckFor) token = '🐻×' + p.stuckFor;
    else if (p.hasKey) token = '🔑';
    rows += '<div class="m-row' + (moved ? ' m-changed' : '') + (p.place ? ' m-done' : '') + '">' +
            '<span class="m-swatch" style="background:' + (p.color || MAZE_FALLBACK_SEAT) + '"></span>' +
            '<span class="m-name">' + escHtml(p.name || '') + '</span>' +
            '<span class="m-token">' + token + '</span>' +
            // The chosen direction, not just that one is chosen. It is the answer
            // to "did my command register, and which way am I about to go", which
            // is the question players have every cycle — and in panel mode it is
            // also the only way to tell a registered move from one Twitch ate.
            '<span class="m-lock' + (p.locked ? ' m-on' : '') +
            (p.place ? ' m-empty' : '') + '">' +
            mazeArrow(p.move) + '</span>' +
            '</div>';
  });
  for (let i = players.length; i < (d.seats || 5); i++) {
    rows += '<div class="m-row m-done"><span class="m-swatch"></span><span class="m-name">—</span></div>';
  }
  return rows + '</div>';
}

// mazeDotPositions records where every runner is on screen right now.
//
// This is the F in FLIP: measure, rebuild, invert, play. renderMaze replaces the
// board wholesale, so a dot is a brand-new element on every render and a CSS
// transition has nothing to transition *from*. Measuring first and inverting on
// the new element gives the movement back without persisting anything across the
// rebuild — and because it measures what is on screen rather than what the last
// payload said, a push arriving mid-slide re-animates from wherever the dot
// currently looks like it is, instead of snapping.
function mazeDotPositions() {
  const at = {};
  mazeEl.querySelectorAll('.m-dot[data-seat]').forEach(el => {
    // The centre, not the corner. A warping dot is mid-scale, and a scale about the
    // element's own centre moves every edge of its box while leaving the centre
    // exactly where it is — so measuring a corner made a dot that had just
    // teleported appear to have drifted, and the next ordinary redraw slid it back
    // from a position it had never been in.
    const r = el.getBoundingClientRect();
    at[el.dataset.seat] = {x: r.left + r.width / 2, y: r.top + r.height / 2};
  });
  return at;
}

// mazeCellSteps is how many cells apart two board coordinates are, or -1 if either
// is unreadable. Cells rather than pixels on purpose: it is the same number in
// panel and full mode, where a pixel threshold would have to know --m-tile.
function mazeCellSteps(fromStr, toStr) {
  const a = mazeCell(fromStr), b = mazeCell(toStr);
  if (!a || !b) return -1;
  return Math.abs(a.x - b.x) + Math.abs(a.y - b.y);
}

// mazeAnimateDots plays the movement for a board that has just been rebuilt.
function mazeAnimateDots(before, d, prev) {
  // Nothing to animate *from*: a first paint, a new round, a reconnect replaying
  // cached state, or a layout switch. All of them should land instantly rather
  // than fly in from wherever the last board happened to have things.
  if (!prev || d.roundId !== prev.roundId || d.display !== prev.display) return;

  const wasAt = {};
  (prev.players || []).forEach(p => { wasAt[p.seat] = p.at; });

  mazeEl.querySelectorAll('.m-dot[data-seat]').forEach(el => {
    const from = before[el.dataset.seat];
    if (!from) return; // newly seated this render; nowhere to come from
    const seat = Number(el.dataset.seat);
    const steps = mazeCellSteps(wasAt[seat], (d.players || []).reduce(
      (acc, q) => q.seat === seat ? q.at : acc, null));

    // A spike throws a runner back to the start from anywhere on the board.
    // Sliding that would draw a line straight through walls, so it jumps — and
    // flashes, because a dot that silently relocates across the board is easy to
    // lose track of, and this is the most consequential thing that happens.
    if (steps > 1) { el.classList.add('m-warp'); return; }

    const r = el.getBoundingClientRect();
    const dx = from.x - (r.left + r.width / 2), dy = from.y - (r.top + r.height / 2);
    // Settled, and nothing moved — by far the commonest render, since the board is
    // redrawn on every !up as well as every cycle.
    if (Math.abs(dx) < MAZE_MIN_SLIDE && Math.abs(dy) < MAZE_MIN_SLIDE) return;

    // The -50%,-50% centring has to survive both halves of this, or every dot
    // jumps half its own width on the way past.
    el.style.transition = 'none';
    el.style.transform = 'translate(-50%,-50%) translate(' + dx + 'px,' + dy + 'px)';
    // Commit the inverted position with a forced reflow rather than waiting for an
    // animation frame. A Browser Source is meant to be left running while not
    // visible (see docs/obs-overlay.md), and a hidden document does not fire
    // requestAnimationFrame — which would leave the dot stranded a whole cell from
    // where it belongs until the next payload happened to redraw it. Reading a
    // layout property is synchronous and cannot be skipped, and with at most five
    // runners the cost is nothing.
    void el.offsetWidth;
    el.style.transition = 'transform ' + MAZE_STEP_MS + 'ms ease-out';
    el.style.transform = 'translate(-50%,-50%)';
  });
}

function renderMaze(d) {
  if (mazeTimer) { clearInterval(mazeTimer); mazeTimer = null; }

  if (!d || d.hidden) {
    mazeEl.hidden = true; mazeEl.innerHTML = '';
    mazePrev = null; mazeTrapSeen = {}; mazeExitSeen = {}; return;
  }
  mazeEl.hidden = false;
  // Captured before the class switch, because changing layout changes --m-tile and
  // would move every dot before it could be measured.
  const before = mazeDotPositions();
  const panel = d.display === 'panel';
  mazeEl.className = panel ? 'm-panel' : 'm-full';

  const prev = mazePrev;
  // A new round reuses cells, so the fade clock starts again with it. Keyed on the
  // round's own id rather than inferred from the cycle counter going backwards:
  // that inference missed a round whose bot was killed before it could send the
  // hidden push, leaving the previous round's spent traps tracked into the next
  // one — where they would never draw at all, on cells that might really be
  // trapped this time.
  if (!prev || d.roundId !== prev.roundId) { mazeTrapSeen = {}; mazeExitSeen = {}; }
  const g = mazeBlocks(d, prev);
  const n = d.size;

  // The start tile is marked on the floor rather than with a glyph at the cell
  // centre. Every runner spawns here, so a centred mark spends the whole join
  // window and the opening cycles buried under a pile of dots — and it is the one
  // landmark that keeps mattering, because spikes throw you back to it.
  const startCell = mazeCell(d.start);
  const startTile = startCell ? (2 * startCell.y + 1) * g.span + (2 * startCell.x + 1) : -1;

  let board = '<div class="m-grid" style="--m-span:' + g.span + '">';
  for (let i = 0; i < g.tiles.length; i++) {
    board += '<i class="m-t m-' + g.tiles[i] + (g.fresh[i] ? ' m-new' : '') +
             (i === startTile ? ' m-startcell' : '') + '"></i>';
  }

  // Coordinate rules on the wall frame, so a callout naming C4 can be followed.
  for (let x = 0; x < n; x++) board += mazeAt(2 * x + 1.5, 0.5, 'm-axis', String.fromCharCode(65 + x));
  for (let y = 0; y < n; y++) board += mazeAt(0.5, 2 * y + 1.5, 'm-axis', String(y + 1));

  // Objectives are drawn through the fog on purpose: it hides the route, not the
  // destination. Without that the scramble for a scarce key could never happen —
  // nobody would know there was anything to race for.
  const mark = (cellStr, cls, glyph) => {
    const c = mazeCell(cellStr);
    return c ? mazeAt(2 * c.x + 1.5, 2 * c.y + 1.5, 'm-mark ' + cls, glyph) : '';
  };
  board += mark(d.exit, 'm-exit', '🚪');
  // Keys are counted per cell before being drawn. Two can genuinely share one —
  // KeysOnMap says as much — and two identical glyphs at one point drew as one, so
  // the board understated how many keys were still in play.
  const keyCount = {};
  (d.keys || []).forEach(k => { keyCount[k] = (keyCount[k] || 0) + 1; });
  Object.keys(keyCount).forEach(k => {
    const c = mazeCell(k);
    if (!c) return;
    const badge = keyCount[k] > 1 ? '<b class="m-keyn">×' + keyCount[k] + '</b>' : '';
    board += mazeAt(2 * c.x + 1.5, 2 * c.y + 1.5, 'm-mark m-key', '🔑' + badge);
  });
  // Only sprung traps are ever sent, so each one marks a hazard that has already
  // been spent by whoever hit it. Show it briefly and let it fade: a permanent
  // icon reads as "danger here" on a cell that is now the safest on the board.
  //
  // The negative animation-delay resumes the fade where it left off rather than
  // restarting it, which is what makes this survive the re-render on every move.
  const nowMs = Date.now();
  (d.traps || []).forEach(tr => {
    if (mazeTrapSeen[tr.at] === undefined) mazeTrapSeen[tr.at] = nowMs;
    // A spiked key-holder drops their key on the trap cell, so this collides on
    // every drop — and a drop is the only way a key ever returns to the board. The
    // key wins: it is live and decides the round, the trap is spent and inert, and
    // chat still narrates the spiking. The fade is stamped above either way, so the
    // icon cannot reappear at full strength if the key is picked up later.
    if (keyCount[tr.at]) return;
    const age = nowMs - mazeTrapSeen[tr.at];
    if (age >= MAZE_TRAP_FADE) return; // spent and faded; the cell is just floor now
    const c = mazeCell(tr.at);
    if (!c) return;
    board += '<span class="m-mark m-trap" style="left:calc(var(--m-tile)*' + (2 * c.x + 1.5).toFixed(3) +
             ');top:calc(var(--m-tile)*' + (2 * c.y + 1.5).toFixed(3) +
             ');animation-delay:' + (-age) + 'ms">' + (tr.kind === 'spike' ? '💀' : '🐻') + '</span>';
  });

  const players = d.players || [];
  players.forEach(p => {
    const c = mazeCell(p.at);
    if (!c) return;
    // A finisher is drawn a little longer rather than vanishing the instant they
    // are through: their last step is into the door, and it is the one moment of
    // the round worth watching. Stamped so the fade resumes across the re-render
    // on every move instead of restarting, exactly as a sprung trap does.
    let exiting = '', age = 0;
    if (p.place) {
      if (mazeExitSeen[p.seat] === undefined) mazeExitSeen[p.seat] = nowMs;
      age = nowMs - mazeExitSeen[p.seat];
      if (age >= MAZE_EXIT_FADE) return; // through the door and gone
      exiting = ' m-out';
    }
    // Fixed sub-slot per seat, so a player's dot sits in the same relative spot
    // whichever cell they are in and they can follow themselves in a pile-up.
    const sx = (p.seat % 3 + 0.5) / 3, sy = (Math.floor(p.seat / 3) + 0.5) / 2;
    const cls = 'm-dot' + (p.hasKey ? ' m-carrying' : '') + (p.stuckFor ? ' m-stuck' : '') + exiting;
    let style = 'color:' + (p.color || MAZE_FALLBACK_SEAT) +
                ';left:calc(var(--m-tile)*' + (2 * c.x + 1 + sx).toFixed(3) +
                ');top:calc(var(--m-tile)*' + (2 * c.y + 1 + sy).toFixed(3) + ')';
    if (exiting) style += ';animation-delay:' + (-age) + 'ms';
    board += '<span class="' + cls + '" data-seat="' + p.seat + '" style="' + style + '"></span>';
  });
  board += '</div>';

  // --- side panel ---
  //
  // Panel mode used to be the board and a timer strip and nothing else, on the
  // grounds that a corner widget earns its space by showing the game rather than a
  // scoreboard about it. That reasoning holds for the feed, which is narration
  // chat already carries — but not for the seat rows, which are the only place
  // anyone can read their own colour, whether their command registered, whether
  // they are carrying a key, and who has finished. Panel is the shipped default,
  // so leaving those out meant most rounds ran with the player-state half of the
  // game drawn nowhere at all, and a winner simply vanished off the board.
  const joining = d.phase === 'joining';
  const done = d.phase === 'done';
  const rows = mazeRows(d, players, prev);
  // The one instruction that matters, in the one window where it can be acted on.
  const joinHint = '<div class="m-hint">!up !down !left !right — take a seat</div>';

  // The single place the board actually lands, so the two layouts cannot drift
  // over whether they animate or re-arm the clock.
  const commit = html => {
    mazeEl.innerHTML = html;
    mazeAnimateDots(before, d, prev);
    mazePrev = d;
    startMazeCountdown(d, done);
  };

  if (panel) {
    // Timer above the board: a strip along the top reads as a header rather than as
    // something dangling off the bottom. The rows go under the board, because the
    // board already fills the panel's width and the corner budget is a width.
    commit((done ? '' : '<div class="m-bar" id="m-bar"><i id="m-fill"></i></div>') +
           board + rows + (joining ? joinHint : ''));
    return;
  }

  let head = '<div class="m-title">GET OUT!!!</div>';
  // Once somebody is out, MaxCycles stops being the deadline that binds: the round
  // ends PlacementCycles after the first escape. Counting toward the cap told
  // everyone racing for second they had forty cycles when they had six, and the
  // scramble is only a race if its clock is the one on screen.
  // "Turn", not "cycle". A cycle is what the engine and maze.toml call it, and it
  // is the right word there; on screen it is the thing a player takes, and every
  // other word around it — take a seat, one move a turn — already said turn.
  let phaseLine;
  if (joining) {
    phaseLine = 'SEATS OPEN';
  } else if (done) {
    phaseLine = 'ROUND OVER';
  } else if (d.endsAtCycle > 0) {
    const left = Math.max(0, d.endsAtCycle - d.cycle);
    phaseLine = 'PLACES CLOSE IN ' + left + (left === 1 ? ' TURN' : ' TURNS');
  } else {
    phaseLine = 'TURN ' + d.cycle + ' / ' + d.maxCycles;
  }
  head += '<div class="m-cycle">' + phaseLine + '</div>';
  if (!done) {
    // The number on its own answered "how long" without ever saying what it was
    // counting toward. The label is a sibling rather than a child because the
    // countdown writes textContent into #m-clock every frame and would erase it.
    head += '<div class="m-clockrow">' +
            '<span class="m-clock" id="m-clock"></span>' +
            '<span class="m-clocklabel">left till next turn</span>' +
            '</div>' +
            '<div class="m-bar" id="m-bar"><i id="m-fill"></i></div>';
  }

  // The rolling play-by-play. It lives in the panel rather than going through the
  // notify toasts: those are serialised at ~5.5s each, so a busy cycle would back
  // the queue up and the "news" would arrive minutes after the board moved on.
  // Toasts are kept for the two rare beats worth interrupting for.
  let feed = '';
  (d.feed || []).forEach((line, i, all) => {
    const age = all.length - 1 - i; // newest last, older entries fade back
    feed += '<div class="m-feed-line" style="opacity:' + Math.max(0.35, 1 - age * 0.16).toFixed(2) + '">' +
            escHtml(line) + '</div>';
  });
  if (feed) feed = '<div class="m-feed">' + feed + '</div>';

  let foot = '';
  if (joining) {
    foot = joinHint;
  } else if (done) {
    const placed = players.filter(p => p.place).sort((a, b) => a.place - b.place);
    foot = '<div class="m-banner">' + (placed.length
      ? placed.map(p => mazeOrdinal(p.place) + ' ' + escHtml(p.name)).join('<br>')
      : 'nobody made it out') + '</div>';
  } else {
    const left = (d.keys || []).length;
    foot = '<div class="m-hint">' + (left === 0 ? 'no keys left on the board'
      : left + (left === 1 ? ' key left' : ' keys left')) + '</div>';
  }

  commit(board + '<div class="m-side">' + head + rows + feed + foot + '</div>');
}

// startMazeCountdown restarts the cycle timer. The bot pushes once per cycle plus
// once per move, so this re-arms often — and that was a bug, not a harmless one.
// It used to count tickMs down from whenever it was re-armed, so every player who
// typed sent the bar back to full and it never reached zero. Then the cycle
// resolved on the bot's own schedule, which from a seat looked exactly like the
// turn being skipped with time still showing.
//
// The payload now carries how much of the cycle is actually left, so a re-arm
// resumes the same countdown instead of starting a new one. The bar's full width
// stays tickMs, so the fraction shown is a fraction of a whole turn.
//
// The numeric readout is full-mode only, so its element is looked up rather than
// assumed — panel mode draws the bar alone.
function startMazeCountdown(d, done) {
  if (done || !(d.tickMs > 0)) return;
  const clock = document.getElementById('m-clock');
  const bar = document.getElementById('m-bar');
  const fill = document.getElementById('m-fill');
  if (!bar || !fill) return;
  const startLeft = typeof d.cycleMsLeft === 'number' ? d.cycleMsLeft : d.tickMs;
  const started = Date.now();
  const tick = () => {
    // cycleMsLeft counts the whole cycle, which is the turn plus the resolve beat
    // the bot leaves at the top of it. Capping the *displayed* value at one turn is
    // what produces the beat: while the remainder is longer than a turn the bar
    // reads full and holds, and the runners are sliding into place underneath it.
    // Then it counts a truthful turn down to zero, and a mid-cycle push resyncs it
    // rather than refilling it.
    const left = Math.min(d.tickMs, Math.max(0, startLeft - (Date.now() - started)));
    if (clock) clock.textContent = (left / 1000).toFixed(1) + 's';
    fill.style.width = (100 * left / d.tickMs) + '%';
    bar.classList.toggle('m-urgent', left < 2000);
  };
  tick();
  mazeTimer = setInterval(tick, 80);
}

// --- SSE transport ----------------------------------------------------------
function connect() {
  const es = new EventSource('/overlay/events' + q);
  es.addEventListener('play', ev => playClip(JSON.parse(ev.data)));
  es.addEventListener('stop', stopClip);
  es.addEventListener('gamble', ev => renderGamble(JSON.parse(ev.data)));
  es.addEventListener('depth', ev => renderDepth(JSON.parse(ev.data)));
  es.addEventListener('wordle', ev => renderWordle(JSON.parse(ev.data)));
  es.addEventListener('connections', ev => renderConnections(JSON.parse(ev.data)));
  es.addEventListener('maze', ev => renderMaze(JSON.parse(ev.data)));
  es.addEventListener('notify', ev => enqueueNotify(JSON.parse(ev.data)));
  // EventSource auto-reconnects on error; nothing to do.
}
connect();
