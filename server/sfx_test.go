package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"tts/sfxlib"
)

// recPlayer records the clip path and bytes it was handed (reading the file
// before the queue removes the temp copy).
type recPlayer struct {
	mu    sync.Mutex
	plays []playRec
	ch    chan struct{}
}

type playRec struct {
	base string
	data string
}

func (p *recPlayer) Play(_ context.Context, _ int64, clip string) error {
	data, _ := os.ReadFile(clip)
	p.mu.Lock()
	p.plays = append(p.plays, playRec{base: filepath.Base(clip), data: string(data)})
	p.mu.Unlock()
	if p.ch != nil {
		p.ch <- struct{}{}
	}
	return nil
}

func (p *recPlayer) records() []playRec {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]playRec, len(p.plays))
	copy(out, p.plays)
	return out
}

func newSFXTestServer(t *testing.T, lib map[string][]sfxlib.Clip, sfxDir string, player Player) *httptest.Server {
	t.Helper()
	logger := log.New(io.Discard, "", 0)
	q := NewQueue(nil, "kokoro", player, t.TempDir(), 100, logger)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go q.Run(ctx)
	board := newSFXBoard(lib, sfxDir, rand.New(rand.NewSource(1)), logger)
	ts := httptest.NewServer(NewServer(q, nil, board, nil, "", 500, logger).Handler())
	t.Cleanup(ts.Close)
	return ts
}

func writeClip(t *testing.T, dir, name, data string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func postSFX(t *testing.T, ts *httptest.Server, name string) int {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"name": name})
	resp, err := http.Post(ts.URL+"/sfx", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func waitPlay(t *testing.T, ch chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for playback")
	}
}

func TestSFXEnqueueAndPlay(t *testing.T) {
	dir := t.TempDir()
	writeClip(t, dir, "boop.mp3", "BOOP")
	lib := map[string][]sfxlib.Clip{"boop": {{File: "boop.mp3"}}}
	player := &recPlayer{ch: make(chan struct{}, 4)}
	ts := newSFXTestServer(t, lib, dir, player)

	if code := postSFX(t, ts, "boop"); code != http.StatusAccepted {
		t.Fatalf("status=%d want 202", code)
	}
	waitPlay(t, player.ch)

	recs := player.records()
	if len(recs) != 1 {
		t.Fatalf("plays=%d want 1", len(recs))
	}
	if recs[0].data != "BOOP" {
		t.Errorf("played data=%q want the boop.mp3 bytes", recs[0].data)
	}
	// Served through the id-keyed temp path, preserving the clip's extension.
	if !strings.HasPrefix(recs[0].base, "tts-") || !strings.HasSuffix(recs[0].base, ".mp3") {
		t.Errorf("played file=%q want tts-<id>.mp3", recs[0].base)
	}

	// Unknown sound is a 404 and never reaches the player.
	if code := postSFX(t, ts, "nope"); code != http.StatusNotFound {
		t.Errorf("unknown sound status=%d want 404", code)
	}
}

func TestSFXRandomClip(t *testing.T) {
	dir := t.TempDir()
	writeClip(t, dir, "m1.mp3", "ONE")
	writeClip(t, dir, "m2.mp3", "TWO")
	lib := map[string][]sfxlib.Clip{"multi": {{File: "m1.mp3"}, {File: "m2.mp3"}}}
	player := &recPlayer{ch: make(chan struct{}, 32)}
	ts := newSFXTestServer(t, lib, dir, player)

	const n = 12
	for range n {
		if code := postSFX(t, ts, "multi"); code != http.StatusAccepted {
			t.Fatalf("status=%d want 202", code)
		}
		waitPlay(t, player.ch)
	}

	seen := map[string]int{}
	for _, r := range player.records() {
		if r.data != "ONE" && r.data != "TWO" {
			t.Fatalf("played data %q not drawn from the clip set", r.data)
		}
		seen[r.data]++
	}
	if len(seen) < 2 {
		t.Errorf("expected both clips across %d plays, saw %v", n, seen)
	}
}

// blockPlayer blocks until ctx is cancelled, so a /skip can be observed.
type blockPlayer struct {
	started chan struct{}
	result  chan error
}

func (p *blockPlayer) Play(ctx context.Context, _ int64, _ string) error {
	p.started <- struct{}{}
	<-ctx.Done()
	p.result <- ctx.Err()
	return ctx.Err()
}

func TestSFXSkipCancels(t *testing.T) {
	dir := t.TempDir()
	writeClip(t, dir, "long.mp3", "LONG")
	lib := map[string][]sfxlib.Clip{"long": {{File: "long.mp3"}}}
	bp := &blockPlayer{started: make(chan struct{}, 1), result: make(chan error, 1)}
	ts := newSFXTestServer(t, lib, dir, bp)

	if code := postSFX(t, ts, "long"); code != http.StatusAccepted {
		t.Fatalf("status=%d want 202", code)
	}
	select {
	case <-bp.started:
	case <-time.After(2 * time.Second):
		t.Fatal("playback never started")
	}

	resp, err := http.Post(ts.URL+"/skip", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	select {
	case err := <-bp.result:
		if err == nil {
			t.Error("expected a cancellation error from the player after /skip")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("playback was not cancelled by /skip")
	}
}
