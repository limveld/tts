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
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"tts/sfxlib"
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
	sfxConfig := flag.String("sfx-config", "sfx.toml", "soundboard TOML (command -> clip); optional")
	sfxDir := flag.String("sfx-dir", "sfx", "directory holding the downloaded soundboard clips")
	engineName := flag.String("engine", envOr("TTS_ENGINE", "kokoro"), "synthesis engine: kokoro (local sidecar) or polly (Amazon Polly) (env TTS_ENGINE)")
	pollyVoice := flag.String("polly-voice", envOr("POLLY_VOICE", "Brian"), "Amazon Polly VoiceId (env POLLY_VOICE)")
	pollyEngine := flag.String("polly-engine", envOr("POLLY_ENGINE", "neural"), "Amazon Polly engine: standard|neural|long-form|generative (env POLLY_ENGINE)")
	pollyRegion := flag.String("polly-region", envOr("AWS_REGION", ""), "AWS region for Polly (falls back to ~/.aws/config)")
	flag.Parse()

	logger := log.New(os.Stderr, "", log.LstdFlags)

	if *playerMode != "browser" && *playerMode != "vlc" {
		logger.Fatalf("invalid -player %q: use browser or vlc", *playerMode)
	}

	if err := os.MkdirAll(*tmpDir, 0o755); err != nil {
		logger.Fatalf("creating tmpdir %s: %v", *tmpDir, err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Pick the synthesis engine at startup and build only that one, so kokoro mode
	// needs no AWS credentials and polly mode needs no Python venv/sidecar.
	var synth Synthesizer
	switch *engineName {
	case "kokoro":
		pyPath := mustExist(logger, *python, "python interpreter (create the venv or pass -python)")
		scriptPath := mustExist(logger, *sidecar, "sidecar script (pass -sidecar)")
		engine := NewEngine(pyPath, scriptPath, *lang, *voice, *speed, logger)
		go engine.Run(ctx)
		synth = engine
		logger.Printf("engine: kokoro (local sidecar) — model loading in the background")
	case "polly":
		client, err := newPollyClient(ctx, *pollyRegion, *pollyVoice, *pollyEngine, logger)
		if err != nil {
			logger.Fatalf("polly: %v", err)
		}
		synth = client
		logger.Printf("engine: polly (engine=%s voice=%s)", *pollyEngine, *pollyVoice)
	default:
		logger.Fatalf("invalid -engine %q: use kokoro or polly", *engineName)
	}

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

	queue := NewQueue(synth, p, *tmpDir, *maxQueue, logger)
	go queue.Run(ctx)

	// Optional soundboard: chat commands that play pre-recorded clips through the
	// same queue/player. Absent config just disables /sfx.
	var board *sfxBoard
	if lib, err := loadSFX(*sfxConfig); err != nil {
		logger.Fatalf("sfx config %s: %v", *sfxConfig, err)
	} else if lib != nil {
		rnd := rand.New(rand.NewSource(time.Now().UnixNano()))
		board = newSFXBoard(lib, *sfxDir, rnd, logger)
		logger.Printf("sfx: %d sound command(s) loaded from %s (clips in %s)", board.count(), *sfxConfig, *sfxDir)
	}

	server := NewServer(queue, overlay, board, *token, *maxChars, logger)
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
	logger.Printf("TTS server listening on %s (%s)", *addr, authNote)

	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Fatalf("http server: %v", err)
	}
	logger.Printf("shutting down")
}

// envOr returns the value of environment variable key, or def when it is unset or
// empty. Used so flags like -engine can default from the process environment (the
// launchd plist), matching how -token defaults from TTS_TOKEN.
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// loadSFX loads the soundboard config, treating a missing file as "no
// soundboard" (nil, nil) so the feature is opt-in; a present-but-invalid file is
// a real error.
func loadSFX(path string) (map[string][]sfxlib.Clip, error) {
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	return sfxlib.Load(path)
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
