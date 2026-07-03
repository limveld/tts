// Command tts is a local HTTP text-to-speech server for a Twitch chat command.
// It accepts text over HTTP, synthesizes speech with a persistent Kokoro Python
// sidecar, and auto-plays the audio through VLC, serializing everything through
// a queue with pause / resume / clear / skip controls.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8080", "address to listen on")
	token := flag.String("token", os.Getenv("TTS_TOKEN"), "optional bearer token required on all routes except /healthz (env TTS_TOKEN)")
	vlcBin := flag.String("vlc", "", "path to the VLC binary (auto-detected if empty)")
	python := flag.String("python", ".venv/bin/python", "path to the Python interpreter for the sidecar")
	sidecar := flag.String("sidecar", "pysidecar/tts_sidecar.py", "path to the Python sidecar script")
	voice := flag.String("voice", "af_heart", "default Kokoro voice")
	lang := flag.String("lang", "a", "Kokoro language code (a=American English)")
	speed := flag.Float64("speed", 1.0, "speech speed")
	maxChars := flag.Int("max-chars", 500, "maximum characters per request (longer text is truncated)")
	maxQueue := flag.Int("max-queue", 100, "maximum pending queue length")
	tmpDir := flag.String("tmpdir", filepath.Join(os.TempDir(), "tts-server"), "directory for temporary WAV files")
	playerMode := flag.String("player", "browser", "playback backend: browser (OBS Browser Source overlay) or vlc (local speakers)")
	flag.Parse()

	logger := log.New(os.Stderr, "", log.LstdFlags)

	if *playerMode != "browser" && *playerMode != "vlc" {
		logger.Fatalf("invalid -player %q: use browser or vlc", *playerMode)
	}

	// Validate the Python interpreter and sidecar script.
	pyPath := mustExist(logger, *python, "python interpreter (create the venv or pass -python)")
	scriptPath := mustExist(logger, *sidecar, "sidecar script (pass -sidecar)")

	if err := os.MkdirAll(*tmpDir, 0o755); err != nil {
		logger.Fatalf("creating tmpdir %s: %v", *tmpDir, err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	engine := NewEngine(pyPath, scriptPath, *lang, *voice, *speed, logger)
	go engine.Run(ctx)

	// The overlay hub is always available (serves /overlay*); the browser player
	// pushes clips to it. VLC is resolved only when actually selected.
	overlay := NewOverlay(*tmpDir, *token, logger)

	var p Player
	if *playerMode == "vlc" {
		vlcPath := *vlcBin
		if vlcPath == "" {
			resolved, err := ResolveVLC()
			if err != nil {
				logger.Fatalf("%v", err)
			}
			vlcPath = resolved
		}
		logger.Printf("player: vlc (%s) — audio plays on this machine's speakers", vlcPath)
		p = NewVLCPlayer(vlcPath, logger)
	} else {
		tokenQuery := ""
		if *token != "" {
			tokenQuery = "?token=" + *token
		}
		logger.Printf("player: browser — add an OBS Browser Source at http://%s/overlay%s", *addr, tokenQuery)
		p = NewBrowserPlayer(overlay, logger)
	}

	queue := NewQueue(engine, p, *tmpDir, *maxQueue, logger)
	go queue.Run(ctx)

	server := NewServer(queue, overlay, *token, *maxChars, logger)
	httpServer := &http.Server{Addr: *addr, Handler: server.Handler()}

	// Shut the HTTP server down when a signal arrives.
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutdownCtx)
	}()

	authNote := "no auth"
	if *token != "" {
		authNote = "token required"
	}
	logger.Printf("TTS server listening on %s (%s); the model is loading in the background", *addr, authNote)

	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Fatalf("http server: %v", err)
	}
	logger.Printf("shutting down")
}

// mustExist resolves path to an absolute path and exits if it does not exist.
func mustExist(logger *log.Logger, path, what string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		logger.Fatalf("resolving %s (%s): %v", what, path, err)
	}
	if _, err := os.Stat(abs); err != nil {
		logger.Fatalf("%s not found at %s: %v", what, abs, fmt.Errorf("%w", err))
	}
	return abs
}
