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
// and used by the sfx-fetch helper to download it. Volume/Start/End control
// playback: Volume is 0-100 percent (nil = 100, full), and Start/End trim the
// clip to seconds [Start, End) of the file (End 0 = play to the natural end).
type Clip struct {
	File   string  `toml:"file"`
	URL    string  `toml:"url"`
	Volume *int    `toml:"volume"`
	Start  float64 `toml:"start"`
	End    float64 `toml:"end"`
}

// sound is the raw TOML shape for one command: a single clip (file/url, plus
// optional volume/start/end) or a list of them (clips).
type sound struct {
	File   string  `toml:"file"`
	URL    string  `toml:"url"`
	Volume *int    `toml:"volume"`
	Start  float64 `toml:"start"`
	End    float64 `toml:"end"`
	Clips  []Clip  `toml:"clips"`
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
			clips = []Clip{{File: s.File, URL: s.URL, Volume: s.Volume, Start: s.Start, End: s.End}}
		}
		for i, c := range clips {
			if c.File == "" {
				return nil, fmt.Errorf("sound %q clip %d: missing file", name, i)
			}
			if c.Volume != nil && (*c.Volume < 0 || *c.Volume > 100) {
				return nil, fmt.Errorf("sound %q clip %d: volume %d out of range 0-100", name, i, *c.Volume)
			}
			if c.Start < 0 {
				return nil, fmt.Errorf("sound %q clip %d: start %g must be >= 0", name, i, c.Start)
			}
			if c.End != 0 && c.End <= c.Start {
				return nil, fmt.Errorf("sound %q clip %d: end %g must be > start %g (or 0 for the full clip)", name, i, c.End, c.Start)
			}
		}
		out[strings.ToLower(name)] = clips
	}
	return out, nil
}
