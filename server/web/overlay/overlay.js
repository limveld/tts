// Full-screen stream overlay. Renders purely from the server's SSE stream
// (/overlay/events); it holds no game state of its own and never connects to
// Twitch. Events: play/stop (audio) + gamble/depth/wordle (added in later stages).

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

  if (d.phase === 'result') {
    const label = d.cancelled ? 'CANCELLED' : ('🎉 ' + (d.winner || ''));
    gambleEl.innerHTML =
      '<div class="g-title">Gamble</div>' +
      '<div class="g-stats">' +
        '<div class="g-stat"><div class="g-num">' + (d.pot || 0) + '</div><div class="g-label">Pot</div></div>' +
        '<div class="g-stat"><div class="g-num">' + (d.players || 0) + '</div><div class="g-label">Players</div></div>' +
      '</div>' +
      '<div class="g-result' + (d.cancelled ? ' cancelled' : '') + '">' + label + '</div>';
    // fade out shortly before the server clears the cached state.
    setTimeout(() => gambleEl.classList.add('fading'), 6000);
    return;
  }

  // phase === 'open'
  gambleEl.innerHTML =
    '<div class="g-title">Gamble</div>' +
    '<div class="g-stats">' +
      '<div class="g-stat"><div class="g-num" id="g-pot">' + (d.pot || 0) + '</div><div class="g-label">Pot</div></div>' +
      '<div class="g-stat"><div class="g-num" id="g-players">' + (d.players || 0) + '</div><div class="g-label">Players</div></div>' +
    '</div>' +
    '<div class="g-countdown" id="g-countdown"></div>';

  const cd = document.getElementById('g-countdown');
  const tick = () => { cd.textContent = fmtCountdown((d.endsAt || 0) - Date.now()); };
  tick();
  if (d.endsAt) gambleCountdown = setInterval(tick, 250);
}

// --- depth rating -----------------------------------------------------------
// Renders {points, tier} as [depth-tier.png] <points> in the bottom-right. Tier
// is derived from the same thresholds the bot uses, so the payload's tier is
// only a fallback. Display caps at 9999 to match the game.
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

function renderDepth(d) {
  if (!d || typeof d.points !== 'number') { depthEl.hidden = true; return; }
  const tier = depthTier(d.points);
  const shown = Math.min(d.points, 9999);
  depthEl.hidden = false;
  depthEl.innerHTML =
    '<img src="/overlay/images/depth-' + tier + '.png" alt="Depth ' + tier + '">' +
    '<span class="d-points">' + shown + '</span>';
}

// --- SSE transport ----------------------------------------------------------
function connect() {
  const es = new EventSource('/overlay/events' + q);
  es.addEventListener('play', ev => playClip(JSON.parse(ev.data)));
  es.addEventListener('stop', stopClip);
  es.addEventListener('gamble', ev => renderGamble(JSON.parse(ev.data)));
  es.addEventListener('depth', ev => renderDepth(JSON.parse(ev.data)));
  // EventSource auto-reconnects on error; nothing to do.
}
connect();
