package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
)

// Player renders a synthesized clip and blocks until playback finishes.
// Canceling ctx (a skip) stops playback and returns ctx.Err().
type Player interface {
	Play(ctx context.Context, id int64, wav string) error
}

// VLCPlayer plays WAV files through the VLC command-line interface, headless —
// audio comes out of THIS machine's speakers (good for local use/testing).
type VLCPlayer struct {
	bin    string
	logger *log.Logger
}

// NewVLCPlayer builds a player that shells out to the VLC binary at bin.
func NewVLCPlayer(bin string, logger *log.Logger) *VLCPlayer {
	return &VLCPlayer{bin: bin, logger: logger}
}

// Play plays wav and blocks until playback finishes. Canceling ctx (a skip)
// kills the VLC process and returns ctx.Err(). The clip id is unused here.
func (p *VLCPlayer) Play(ctx context.Context, _ int64, wav string) error {
	// -I dummy: no interface (the `cvlc` equivalent); --play-and-exit + vlc://quit
	// guarantee VLC exits once the clip is done rather than lingering.
	cmd := exec.CommandContext(ctx, p.bin,
		"-I", "dummy",
		"--no-video",
		"--play-and-exit",
		"--quiet",
		wav,
		"vlc://quit",
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("vlc: %w (%s)", err, bytes.TrimSpace(stderr.Bytes()))
	}
	return nil
}

// ResolveVLC finds a usable VLC CLI: prefer cvlc/vlc on PATH, then fall back to
// the macOS app bundle binary.
func ResolveVLC() (string, error) {
	for _, name := range []string{"cvlc", "vlc"} {
		if path, err := exec.LookPath(name); err == nil {
			return path, nil
		}
	}
	const macPath = "/Applications/VLC.app/Contents/MacOS/VLC"
	if _, err := os.Stat(macPath); err == nil {
		return macPath, nil
	}
	return "", errors.New("VLC not found on PATH or at /Applications/VLC.app; install VLC or pass -vlc")
}
