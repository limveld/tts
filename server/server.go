package main

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"unicode/utf8"
)

// Server exposes the queue over HTTP.
type Server struct {
	queue    *Queue
	overlay  *Overlay  // may be nil; when set, /overlay* routes are registered
	sfx      *sfxBoard // may be nil; when nil, /sfx returns 404 (not configured)
	voices   *VoiceMap // resolves a /say "code" to (engine, voice); may be nil
	token    string
	maxChars int
	logger   *log.Logger
}

// NewServer builds the HTTP layer. If token is non-empty, every route except
// /healthz (and the overlay, which authenticates via a ?token= query param)
// requires the bearer token.
func NewServer(q *Queue, overlay *Overlay, sfx *sfxBoard, voices *VoiceMap, token string, maxChars int, logger *log.Logger) *Server {
	return &Server{queue: q, overlay: overlay, sfx: sfx, voices: voices, token: token, maxChars: maxChars, logger: logger}
}

// controlResponse is the body returned by the control endpoints: the queue
// status, plus optional action detail.
type controlResponse struct {
	Status
	Cleared *int  `json:"cleared,omitempty"`
	Skipped *bool `json:"skipped,omitempty"`
}

// Handler returns the configured HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/say", s.auth(s.post(s.handleSay)))
	mux.HandleFunc("/sfx", s.auth(s.post(s.handleSfx)))
	mux.HandleFunc("/pause", s.auth(s.post(s.handlePause)))
	mux.HandleFunc("/resume", s.auth(s.post(s.handleResume)))
	mux.HandleFunc("/clear", s.auth(s.post(s.handleClear)))
	mux.HandleFunc("/skip", s.auth(s.post(s.handleSkip)))
	mux.HandleFunc("/status", s.auth(s.handleStatus))
	mux.HandleFunc("/voices", s.auth(s.handleVoices))
	if s.overlay != nil {
		s.overlay.routes(mux) // /overlay* — auth via ?token= query param
	}
	return mux
}

// auth enforces the bearer token when one is configured.
func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.token != "" {
			authOK := r.Header.Get("Authorization") == "Bearer "+s.token
			headerOK := r.Header.Get("X-TTS-Token") == s.token
			if !authOK && !headerOK {
				writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
				return
			}
		}
		next(w, r)
	}
}

// post rejects non-POST methods.
func (s *Server) post(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
			return
		}
		next(w, r)
	}
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleSay(w http.ResponseWriter, r *http.Request) {
	var text, code string
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		var body struct {
			Text string `json:"text"`
			Code string `json:"code"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
			return
		}
		text, code = body.Text, body.Code
	} else {
		text = r.FormValue("text")
		code = r.FormValue("code")
	}

	text = strings.TrimSpace(text)
	if text == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "text is required"})
		return
	}
	truncated := false
	if utf8.RuneCountInString(text) > s.maxChars {
		text = string([]rune(text)[:s.maxChars])
		truncated = true
	}

	// Resolve the chat code to an (engine, voice); "" / unknown → weighted random.
	engine, voice := "", strings.TrimSpace(code)
	if s.voices != nil {
		engine, voice = s.voices.Resolve(strings.TrimSpace(code))
	}
	id, position, err := s.queue.Enqueue(text, voice, engine)
	if err != nil {
		if errors.Is(err, ErrQueueFull) {
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "queue is full"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{
		"id":        id,
		"position":  position,
		"truncated": truncated,
	})
}

func (s *Server) handleSfx(w http.ResponseWriter, r *http.Request) {
	if s.sfx == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "sfx not configured"})
		return
	}
	var name string
	if strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		var body struct {
			Name string `json:"name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
			return
		}
		name = body.Name
	} else {
		name = r.FormValue("name")
	}

	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name is required"})
		return
	}
	clip, ok := s.sfx.pick(name)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown sound"})
		return
	}

	id, position, err := s.queue.EnqueueSFX(name, clip.path, clip.volume, clip.start, clip.end)
	if err != nil {
		if errors.Is(err, ErrQueueFull) {
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "queue is full"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"id": id, "position": position})
}

func (s *Server) handlePause(w http.ResponseWriter, r *http.Request) {
	s.queue.Pause()
	writeJSON(w, http.StatusOK, controlResponse{Status: s.queue.Status()})
}

func (s *Server) handleResume(w http.ResponseWriter, r *http.Request) {
	s.queue.Resume()
	writeJSON(w, http.StatusOK, controlResponse{Status: s.queue.Status()})
}

func (s *Server) handleClear(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, controlResponse{Status: s.queue.Status(), Cleared: new(s.queue.Clear())})
}

func (s *Server) handleSkip(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, controlResponse{Status: s.queue.Status(), Skipped: new(s.queue.Skip())})
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.queue.Status())
}

// handleVoices returns the code→voice map for the enabled engines (the bot's !voices).
func (s *Server) handleVoices(w http.ResponseWriter, r *http.Request) {
	var list []VoiceEntry
	if s.voices != nil {
		list = s.voices.List()
	}
	writeJSON(w, http.StatusOK, list)
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}
