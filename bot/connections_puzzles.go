package main

import (
	_ "embed"
	"encoding/json"
	"os"
	"strings"
)

// The Connections puzzle bank. Puzzles parse directly from the Eyefyre
// NYT-Connections-Answers JSON shape ({id, date, answers:[{level, group,
// members}]}) via the tags below, so the embedded seed and the runtime file
// (refreshed by `mise run connections:sync`) share one parser. A round picks one
// at random — deliberately not the date-of-day puzzle — so a stream can replay
// endlessly with variety.

//go:embed connections_seed.json
var connectionsSeedRaw []byte

// connGroup is one of the four hidden categories: a name, a difficulty level
// (0 yellow, 1 green, 2 blue, 3 purple), and its four member words.
type connGroup struct {
	Name  string   `json:"group"`
	Level int      `json:"level"`
	Words []string `json:"members"`
}

// connPuzzle is one full puzzle: exactly four groups (16 words total).
type connPuzzle struct {
	ID     int         `json:"id"`
	Date   string      `json:"date"`
	Groups []connGroup `json:"answers"`
}

// loadConnectionsPuzzles returns the puzzle bank and a short description of where
// it came from (for a startup log line). It prefers the on-disk file at path (the
// synced corpus); on any problem — missing, unreadable, unparseable, or empty —
// it falls back to the embedded seed, which is always present.
func loadConnectionsPuzzles(path string) (puzzles []connPuzzle, source string) {
	if path != "" {
		if raw, err := os.ReadFile(path); err == nil {
			if ps := parseConnections(raw); len(ps) > 0 {
				return ps, path
			}
		}
	}
	return parseConnections(connectionsSeedRaw), "embedded seed"
}

// parseConnections unmarshals a corpus and keeps only well-formed puzzles (exactly
// four groups of four named words), upper-casing the words so submitted-group
// comparison is case-insensitive. A malformed corpus yields nil rather than an
// error — callers fall back to the seed.
func parseConnections(raw []byte) []connPuzzle {
	var all []connPuzzle
	if err := json.Unmarshal(raw, &all); err != nil {
		return nil
	}
	out := all[:0] // filter in place
	for _, p := range all {
		if len(p.Groups) != 4 {
			continue
		}
		ok := true
		for i := range p.Groups {
			g := &p.Groups[i]
			if len(g.Words) != 4 || g.Name == "" {
				ok = false
				break
			}
			for j, w := range g.Words {
				g.Words[j] = strings.ToUpper(strings.TrimSpace(w))
			}
		}
		if ok {
			normalizeLevels(&p)
			out = append(out, p)
		}
	}
	return out
}

// normalizeLevels makes the four groups carry distinct difficulty levels 0..3
// (yellow → purple). The upstream corpus leaves newer puzzles unlabeled — every
// group at level -1 — which would otherwise index the color table out of range;
// those fall back to listing order, which is the order the source publishes
// difficulties in.
func normalizeLevels(p *connPuzzle) {
	var seen [connGroupCount]bool
	for _, g := range p.Groups {
		if g.Level < 0 || g.Level >= connGroupCount || seen[g.Level] {
			for i := range p.Groups {
				p.Groups[i].Level = i
			}
			return
		}
		seen[g.Level] = true
	}
}
