// Command connections-fetch downloads the Eyefyre NYT-Connections-Answers corpus
// (a JSON array of past puzzles) to a local file the bot reads at startup. The
// bot ships with an embedded seed copy, so this is only needed to refresh the
// puzzle bank with newer entries; restart the bot to pick up the new file.
//
// The file is validated before it replaces the old one (parsed + sanity-checked
// for 4 groups of 4), and written atomically (.part -> rename) so a failed or
// truncated download never clobbers a good corpus.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"
)

// defaultURL is the daily-updated corpus. It parses directly into the bot's
// connPuzzle shape ({id, date, answers:[{level, group, members}]}).
const defaultURL = "https://raw.githubusercontent.com/Eyefyre/NYT-Connections-Answers/main/connections.json"

// puzzle mirrors one entry, only enough to validate the download.
type puzzle struct {
	ID      int `json:"id"`
	Answers []struct {
		Level   int      `json:"level"`
		Group   string   `json:"group"`
		Members []string `json:"members"`
	} `json:"answers"`
}

func main() {
	url := flag.String("url", defaultURL, "source URL for the connections corpus JSON")
	dest := flag.String("out", "connections.json", "file to write the corpus to")
	flag.Parse()

	body, err := download(*url)
	if err != nil {
		log.Fatalf("download: %v", err)
	}

	var puzzles []puzzle
	if err := json.Unmarshal(body, &puzzles); err != nil {
		log.Fatalf("parse: %v (not the expected connections schema?)", err)
	}
	if n := validate(puzzles); n == 0 {
		log.Fatalf("parse: 0 usable puzzles in %d entries — refusing to write", len(puzzles))
	}

	if err := writeAtomic(*dest, body); err != nil {
		log.Fatalf("write %s: %v", *dest, err)
	}
	fmt.Printf("wrote %s (%d puzzles) — restart the bot to load them\n", *dest, len(puzzles))
}

// validate counts entries that have exactly 4 groups of 4 members, the shape the
// game requires. A corpus with none is treated as a bad download.
func validate(puzzles []puzzle) int {
	usable := 0
	for _, p := range puzzles {
		if len(p.Answers) != 4 {
			continue
		}
		ok := true
		for _, g := range p.Answers {
			if len(g.Members) != 4 || g.Group == "" {
				ok = false
				break
			}
		}
		if ok {
			usable++
		}
	}
	return usable
}

func download(url string) ([]byte, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s -> %s", url, resp.Status)
	}
	return io.ReadAll(resp.Body)
}

// writeAtomic writes body to a .part sibling then renames it over dest, so a
// crash mid-write can't leave a truncated corpus in place.
func writeAtomic(dest string, body []byte) error {
	tmp := dest + ".part"
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, dest)
}
