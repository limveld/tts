package main

import (
	"log"
	"math/rand"
	"os"
	"path/filepath"
	"sync"

	"tts/sfxlib"
)

// sfxBoard maps a sound command name to its resolved clip file paths and picks
// one at random per play. It's the server's view of sfx.toml (the bot keeps its
// own, for command registration).
type sfxBoard struct {
	mu    sync.Mutex // guards rnd (not concurrency-safe) across HTTP handlers
	clips map[string][]string
	rnd   *rand.Rand
}

// newSFXBoard resolves each clip under dir. A missing file is warned about, not
// fatal, so a partially-downloaded library still serves what it has (run
// `mise run sfx:fetch` to fill the gaps).
func newSFXBoard(lib map[string][]sfxlib.Clip, dir string, rnd *rand.Rand, logger *log.Logger) *sfxBoard {
	clips := make(map[string][]string, len(lib))
	for name, cs := range lib {
		var paths []string
		for _, c := range cs {
			p := filepath.Join(dir, c.File)
			if _, err := os.Stat(p); err != nil {
				logger.Printf("sfx: %q clip missing: %s (run 'mise run sfx:fetch')", name, p)
				continue
			}
			paths = append(paths, p)
		}
		if len(paths) == 0 {
			logger.Printf("sfx: %q has no playable clips, skipping", name)
			continue
		}
		clips[name] = paths
	}
	return &sfxBoard{clips: clips, rnd: rnd}
}

// pick returns a random clip path for name, or ok=false if unknown.
func (b *sfxBoard) pick(name string) (string, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	paths := b.clips[name]
	if len(paths) == 0 {
		return "", false
	}
	return paths[b.rnd.Intn(len(paths))], true
}

// count is the number of playable sound commands.
func (b *sfxBoard) count() int { return len(b.clips) }
