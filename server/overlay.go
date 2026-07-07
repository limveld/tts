package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Overlay pushes generated clips to a browser page (added as an OBS/Streamlabs
// Browser Source). OBS renders the page's <audio> natively into the stream mix,
// so no virtual audio device (BlackHole) or desktop capture is needed — and it
// works whether the server is local or on another machine.
//
// Transport is Server-Sent Events (server -> page "play"/"stop") plus a small
// POST /overlay/done (page -> server) so the queue stays serialized. All std lib.
type Overlay struct {
	tmpDir string
	token  string // when set, endpoints require ?token=<token> (a Browser Source can't send headers)
	logger *log.Logger

	mu      sync.Mutex
	clients map[chan []byte]struct{} // connected SSE clients
	acks    map[int64]chan struct{}  // pending playback acks, keyed by clip id
}

// NewOverlay builds the hub. tmpDir is where the queue writes tts-<id>.wav files.
func NewOverlay(tmpDir, token string, logger *log.Logger) *Overlay {
	return &Overlay{
		tmpDir:  tmpDir,
		token:   token,
		logger:  logger,
		clients: make(map[chan []byte]struct{}),
		acks:    make(map[int64]chan struct{}),
	}
}

func (o *Overlay) clientCount() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return len(o.clients)
}

func (o *Overlay) authed(r *http.Request) bool {
	return o.token == "" || r.URL.Query().Get("token") == o.token
}

// broadcast sends an SSE event to every connected client (non-blocking).
func (o *Overlay) broadcast(event string, data []byte) {
	msg := fmt.Appendf(nil, "event: %s\ndata: %s\n\n", event, data)
	o.mu.Lock()
	defer o.mu.Unlock()
	for ch := range o.clients {
		select {
		case ch <- msg:
		default: // slow client; drop rather than block the worker
		}
	}
}

// Play tells the overlay to play clip id from url and blocks until the page acks
// (via /overlay/done), the ctx is canceled (skip -> also halts the page), or
// maxWait elapses (safety net if the page never acks). If no client is
// connected it returns immediately so the queue never stalls.
func (o *Overlay) Play(ctx context.Context, id int64, url string, maxWait time.Duration, volume int, start, end float64) error {
	if o.clientCount() == 0 {
		o.logger.Printf("overlay: no browser source connected, dropping clip %d", id)
		return nil
	}

	ack := make(chan struct{}, 1)
	o.mu.Lock()
	o.acks[id] = ack
	o.mu.Unlock()
	defer func() {
		o.mu.Lock()
		delete(o.acks, id)
		o.mu.Unlock()
	}()

	data, _ := json.Marshal(map[string]any{"id": id, "url": url, "volume": volume, "start": start, "end": end})
	o.broadcast("play", data)

	timer := time.NewTimer(maxWait)
	defer timer.Stop()
	select {
	case <-ack:
		return nil
	case <-ctx.Done():
		o.broadcast("stop", []byte("{}")) // halt the page mid-clip (skip)
		return ctx.Err()
	case <-timer.C:
		o.logger.Printf("overlay: clip %d timed out waiting for playback ack", id)
		return nil
	}
}

// Done is called by /overlay/done when the page finishes a clip.
func (o *Overlay) Done(id int64) {
	o.mu.Lock()
	ack := o.acks[id]
	o.mu.Unlock()
	if ack != nil {
		select {
		case ack <- struct{}{}:
		default:
		}
	}
}

// routes registers the overlay endpoints on mux.
func (o *Overlay) routes(mux *http.ServeMux) {
	mux.HandleFunc("/overlay", o.handlePage)
	mux.HandleFunc("/overlay/events", o.handleEvents)
	mux.HandleFunc("/overlay/clip/", o.handleClip)
	mux.HandleFunc("/overlay/done", o.handleDone)
}

func (o *Overlay) handlePage(w http.ResponseWriter, r *http.Request) {
	if !o.authed(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(overlayHTML))
}

func (o *Overlay) handleEvents(w http.ResponseWriter, r *http.Request) {
	if !o.authed(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch := make(chan []byte, 8)
	o.mu.Lock()
	o.clients[ch] = struct{}{}
	n := len(o.clients)
	o.mu.Unlock()
	o.logger.Printf("overlay: browser source connected (%d total)", n)
	defer func() {
		o.mu.Lock()
		delete(o.clients, ch)
		o.mu.Unlock()
		o.logger.Printf("overlay: browser source disconnected")
	}()

	fmt.Fprint(w, ": connected\n\n")
	flusher.Flush()

	keepalive := time.NewTicker(15 * time.Second)
	defer keepalive.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case msg := <-ch:
			_, _ = w.Write(msg)
			flusher.Flush()
		case <-keepalive.C:
			fmt.Fprint(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}

// handleClip serves tmpDir/tts-<id><ext> (e.g. .wav for TTS, .mp3 for SFX). The
// id must parse as an integer — which also blocks path traversal, since any
// separator in the request leaves a non-numeric id.
func (o *Overlay) handleClip(w http.ResponseWriter, r *http.Request) {
	if !o.authed(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	base := strings.TrimPrefix(r.URL.Path, "/overlay/clip/")
	ext := filepath.Ext(base) // ".wav" / ".mp3"
	id, err := strconv.ParseInt(strings.TrimSuffix(base, ext), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	ct := mime.TypeByExtension(ext)
	if ct == "" {
		ct = "audio/wav"
	}
	w.Header().Set("Content-Type", ct)
	http.ServeFile(w, r, filepath.Join(o.tmpDir, fmt.Sprintf("tts-%d%s", id, ext)))
}

func (o *Overlay) handleDone(w http.ResponseWriter, r *http.Request) {
	if !o.authed(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var body struct {
		ID int64 `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	o.Done(body.ID)
	w.WriteHeader(http.StatusNoContent)
}

// BrowserPlayer is a Player that plays clips through the overlay (OBS Browser
// Source) instead of local speakers.
type BrowserPlayer struct {
	overlay *Overlay
	logger  *log.Logger
}

func NewBrowserPlayer(o *Overlay, logger *log.Logger) *BrowserPlayer {
	return &BrowserPlayer{overlay: o, logger: logger}
}

func (p *BrowserPlayer) Play(ctx context.Context, clip Playback) error {
	ext := filepath.Ext(clip.Path)
	if ext == "" {
		ext = ".wav"
	}
	url := fmt.Sprintf("/overlay/clip/%d%s", clip.ID, ext)
	// Bound the wait so a disconnect can't stall the queue indefinitely. A trimmed
	// clip runs end-start; otherwise size WAVs from the file, and use a generous cap
	// for compressed clips (sfx mp3). The page still acks early on "ended"/trim-end,
	// so this is only a safety net.
	maxWait := 60 * time.Second
	switch {
	case clip.End > clip.Start:
		maxWait = time.Duration((clip.End-clip.Start)*float64(time.Second)) + 10*time.Second
	case ext == ".wav":
		maxWait = estimateWavDuration(clip.Path) + 10*time.Second
	}
	return p.overlay.Play(ctx, clip.ID, url, maxWait, clip.Volume, clip.Start, clip.End)
}

// estimateWavDuration estimates a clip's length from its file size (our TTS
// clips are 24 kHz mono 16-bit PCM). Falls back to 60s if the file can't be
// stat'd.
func estimateWavDuration(wav string) time.Duration {
	fi, err := os.Stat(wav)
	if err != nil {
		return 60 * time.Second
	}
	const bytesPerSec = 24000 * 2 // 24 kHz * 2 bytes/sample, mono
	secs := float64(fi.Size()) / bytesPerSec
	return time.Duration(secs * float64(time.Second))
}

const overlayHTML = `<!doctype html>
<html>
<head>
<meta charset="utf-8">
<title>TTS Overlay</title>
<style>html,body{margin:0;background:transparent;overflow:hidden}</style>
</head>
<body>
<script>
// Carry any ?token= from this page's URL onto the API calls (a Browser Source
// can't set headers, so the server accepts the token as a query param).
const token = new URLSearchParams(location.search).get('token');
const q = token ? ('?token=' + encodeURIComponent(token)) : '';
let current = null;

function connect() {
  const es = new EventSource('/overlay/events' + q);
  es.addEventListener('play', ev => {
    const d = JSON.parse(ev.data);
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
  });
  es.addEventListener('stop', () => {
    if (current) { current.pause(); current = null; }
  });
  // EventSource auto-reconnects on error; nothing to do.
}
connect();
</script>
</body>
</html>
`
