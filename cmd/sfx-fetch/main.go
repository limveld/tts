// Command sfx-fetch downloads the soundboard clips declared in sfx.toml into the
// sfx/ dir, skipping any that already exist. myinstants gates its HTML pages
// behind Cloudflare, so we send a browser-like User-Agent + Referer; the media
// CDN files themselves generally serve fine with those headers.
package main

import (
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"tts/sfxlib"
)

const userAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) " +
	"AppleWebKit/537.36 (KHTML, like Gecko) Chrome/125.0.0.0 Safari/537.36"

func main() {
	cfg := flag.String("config", "sfx.toml", "path to the soundboard TOML")
	dir := flag.String("dir", "sfx", "directory to download clips into")
	flag.Parse()

	lib, err := sfxlib.Load(*cfg)
	if err != nil {
		log.Fatalf("loading %s: %v", *cfg, err)
	}
	if err := os.MkdirAll(*dir, 0o755); err != nil {
		log.Fatalf("mkdir %s: %v", *dir, err)
	}

	client := &http.Client{Timeout: 30 * time.Second}
	var fetched, present, failed int
	for name, clips := range lib {
		for _, c := range clips {
			dest := filepath.Join(*dir, c.File)
			if _, err := os.Stat(dest); err == nil {
				present++
				continue
			}
			if c.URL == "" {
				log.Printf("%s: %s not present and has no url; skipping", name, c.File)
				failed++
				continue
			}
			if err := download(client, c.URL, dest); err != nil {
				log.Printf("%s: %v", name, err)
				failed++
				continue
			}
			log.Printf("fetched %s -> %s", c.URL, dest)
			fetched++
		}
	}
	fmt.Printf("done: %d fetched, %d already present, %d failed\n", fetched, present, failed)
	if failed > 0 {
		os.Exit(1)
	}
}

func download(client *http.Client, url, dest string) error {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Referer", "https://www.myinstants.com/")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s -> %s", url, resp.Status)
	}

	tmp := dest + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dest)
}
