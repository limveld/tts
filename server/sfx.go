package main

import (
	"log"
	"math/rand"
	"os"
	"path/filepath"
	"sync"

	"tts/sfxlib"
)

// boardClip is a resolved, playable sound: its file path plus playback controls
// (volume 0-100 resolved from sfx.toml, and start/end trim seconds).
type boardClip struct {
	path   string
	volume int // 0-100, resolved (sfx.toml nil → 100)
	start  float64
	end    float64
}

// sfxBoard maps a sound command name to its resolved clips and picks one at
// random per play. It's the server's view of sfx.toml (the bot keeps its own,
// for command registration).
type sfxBoard struct {
	mu    sync.Mutex // guards rnd (not concurrency-safe) across HTTP handlers
	clips map[string][]boardClip
	rnd   *rand.Rand
}

// newSFXBoard resolves each clip under dir. A missing file is warned about, not
// fatal, so a partially-downloaded library still serves what it has (run
// `mise run sfx:fetch` to fill the gaps).
func newSFXBoard(lib map[string][]sfxlib.Clip, dir string, rnd *rand.Rand, logger *log.Logger) *sfxBoard {
	clips := make(map[string][]boardClip, len(lib))
	for name, cs := range lib {
		var bcs []boardClip
		for _, c := range cs {
			p := filepath.Join(dir, c.File)
			if _, err := os.Stat(p); err != nil {
				logger.Printf("sfx: %q clip missing: %s (run 'mise run sfx:fetch')", name, p)
				continue
			}
			vol := 100
			if c.Volume != nil {
				vol = *c.Volume
			}
			bcs = append(bcs, boardClip{path: p, volume: vol, start: c.Start, end: c.End})
		}
		if len(bcs) == 0 {
			logger.Printf("sfx: %q has no playable clips, skipping", name)
			continue
		}
		clips[name] = bcs
	}
	return &sfxBoard{clips: clips, rnd: rnd}
}

// pick returns a random resolved clip for name, or ok=false if unknown.
func (b *sfxBoard) pick(name string) (boardClip, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	cs := b.clips[name]
	if len(cs) == 0 {
		return boardClip{}, false
	}
	return cs[b.rnd.Intn(len(cs))], true
}

// count is the number of playable sound commands.
func (b *sfxBoard) count() int { return len(b.clips) }
