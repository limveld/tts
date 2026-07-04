// Package sfxlib loads the soundboard TOML shared by the TTS server (which plays
// the clips) and the bot (which registers a chat command per sound).
//
// A sound is either a single clip (terse file/url) or several clips
// ([[sounds.<name>.clips]]); when it has several, the server picks one at random
// per invocation. Both forms normalise to a non-empty []Clip.
package sfxlib

import (
	"fmt"
	"strings"

	"github.com/BurntSushi/toml"
)

// Clip is one playable sound file and its upstream source URL. file is the
// local filename under the sfx dir the server plays; url is kept for provenance
// and used by the sfx-fetch helper to download it.
type Clip struct {
	File string `toml:"file"`
	URL  string `toml:"url"`
}

// sound is the raw TOML shape for one command: a single clip (file/url) or a
// list of them (clips).
type sound struct {
	File  string `toml:"file"`
	URL   string `toml:"url"`
	Clips []Clip `toml:"clips"`
}

type document struct {
	Sounds map[string]sound `toml:"sounds"`
}

// Load parses the soundboard TOML at path into command name -> clips. Each
// command is normalised to a non-empty []Clip (the single file/url form becomes
// a one-element list). Command names are lowercased (chat is matched lowercased).
func Load(path string) (map[string][]Clip, error) {
	var doc document
	if _, err := toml.DecodeFile(path, &doc); err != nil {
		return nil, err
	}
	out := make(map[string][]Clip, len(doc.Sounds))
	for name, s := range doc.Sounds {
		clips := s.Clips
		if len(clips) == 0 {
			if s.File == "" {
				return nil, fmt.Errorf("sound %q: needs a file or a [[sounds.%s.clips]] list", name, name)
			}
			clips = []Clip{{File: s.File, URL: s.URL}}
		}
		for i, c := range clips {
			if c.File == "" {
				return nil, fmt.Errorf("sound %q clip %d: missing file", name, i)
			}
		}
		out[strings.ToLower(name)] = clips
	}
	return out, nil
}
