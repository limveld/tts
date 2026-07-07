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

// Playback describes one clip to play: the file plus optional SFX controls.
// Volume is 0-100 percent (100 = the file's own level, the default); Start/End
// trim playback to seconds [Start, End) of the file (End 0 = play to the natural
// end). TTS clips use Volume 100 and no trim.
type Playback struct {
	ID     int64
	Path   string
	Volume int
	Start  float64
	End    float64
}

// Player renders a clip and blocks until playback finishes. Canceling ctx (a
// skip) stops playback and returns ctx.Err().
type Player interface {
	Play(ctx context.Context, clip Playback) error
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

// Play plays the clip and blocks until playback finishes. Canceling ctx (a skip)
// kills the VLC process and returns ctx.Err(). Applies the clip's trim
// (--start-time/--stop-time). Per-clip volume is NOT applied under VLC — its CLI
// has no simple linear volume control — so it's the OBS overlay's job (VLC is the
// local "does it play" path); a non-default volume is logged and ignored here.
func (p *VLCPlayer) Play(ctx context.Context, clip Playback) error {
	// -I dummy: no interface (the `cvlc` equivalent); --play-and-exit + vlc://quit
	// guarantee VLC exits once the clip is done rather than lingering.
	args := []string{"-I", "dummy", "--no-video", "--play-and-exit", "--quiet"}
	if clip.Start > 0 {
		args = append(args, fmt.Sprintf("--start-time=%g", clip.Start))
	}
	if clip.End > 0 {
		args = append(args, fmt.Sprintf("--stop-time=%g", clip.End))
	}
	if clip.Volume != 0 && clip.Volume != 100 {
		p.logger.Printf("vlc: clip %d volume %d%% not applied (use the OBS overlay for exact volume)", clip.ID, clip.Volume)
	}
	args = append(args, clip.Path, "vlc://quit")

	cmd := exec.CommandContext(ctx, p.bin, args...)
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
