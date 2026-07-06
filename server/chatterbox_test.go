package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// fakeDevnen stands in for the external devnen Chatterbox server: /tts records the
// request body and returns fixed WAV bytes; /api/unload counts reclaim requests.
type fakeDevnen struct {
	wav []byte

	mu      sync.Mutex
	ttsReqs []ttsRequest
	unloads int
}

func (f *fakeDevnen) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/tts", func(w http.ResponseWriter, r *http.Request) {
		var req ttsRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		f.ttsReqs = append(f.ttsReqs, req)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "audio/wav")
		_, _ = w.Write(f.wav)
	})
	mux.HandleFunc("/api/unload", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.unloads++
		f.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
	return mux
}

func (f *fakeDevnen) requests() []ttsRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]ttsRequest, len(f.ttsReqs))
	copy(out, f.ttsReqs)
	return out
}

func (f *fakeDevnen) unloadCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.unloads
}

func newFakeDevnen(t *testing.T, wav string) (*fakeDevnen, *httptest.Server) {
	t.Helper()
	fd := &fakeDevnen{wav: []byte(wav)}
	ts := httptest.NewServer(fd.handler())
	t.Cleanup(ts.Close)
	return fd, ts
}

// TestChatterboxSynthesize drives the client directly: the request carries the
// fixed dramatic preset and predefined voice, and the WAV response lands at outPath.
func TestChatterboxSynthesize(t *testing.T) {
	fd, devnen := newFakeDevnen(t, "WAVDATA")
	client := newChatterboxClient(devnen.URL, "narrator", 0.7, 0.3, 0, log.New(io.Discard, "", 0))

	out := filepath.Join(t.TempDir(), "out.wav")
	if err := client.Synthesize(context.Background(), "hi dramatic", "ignored-voice-code", out); err != nil {
		t.Fatalf("Synthesize: %v", err)
	}

	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("reading output: %v", err)
	}
	if string(data) != "WAVDATA" {
		t.Errorf("output=%q want the WAV bytes from devnen", string(data))
	}

	reqs := fd.requests()
	if len(reqs) != 1 {
		t.Fatalf("tts requests=%d want 1", len(reqs))
	}
	r := reqs[0]
	if r.Text != "hi dramatic" {
		t.Errorf("text=%q want %q", r.Text, "hi dramatic")
	}
	if r.VoiceMode != "predefined" {
		t.Errorf("voice_mode=%q want predefined", r.VoiceMode)
	}
	if r.PredefinedVoiceID != "narrator" {
		t.Errorf("predefined_voice_id=%q want narrator", r.PredefinedVoiceID)
	}
	if r.Exaggeration != 0.7 || r.CFGWeight != 0.3 {
		t.Errorf("preset exaggeration=%v cfg_weight=%v want 0.7/0.3", r.Exaggeration, r.CFGWeight)
	}
	if r.OutputFormat != "wav" {
		t.Errorf("output_format=%q want wav", r.OutputFormat)
	}
}

// TestChatterboxOmitsEmptyVoice confirms an empty predefined voice is omitted from
// the body so devnen falls back to its own default.
func TestChatterboxOmitsEmptyVoice(t *testing.T) {
	fd, devnen := newFakeDevnen(t, "W")
	client := newChatterboxClient(devnen.URL, "", 0.7, 0.3, 0, log.New(io.Discard, "", 0))
	if err := client.Synthesize(context.Background(), "x", "", filepath.Join(t.TempDir(), "o.wav")); err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	if got := fd.requests()[0].PredefinedVoiceID; got != "" {
		t.Errorf("predefined_voice_id=%q want empty", got)
	}
}

// TestChatterboxThroughQueue exercises the real path: POST /say -> Queue worker ->
// chatterbox client -> devnen -> player, with the engine selected as chatterbox.
func TestChatterboxThroughQueue(t *testing.T) {
	fd, devnen := newFakeDevnen(t, "RIFF-chatterbox-wav")
	logger := log.New(io.Discard, "", 0)
	client := newChatterboxClient(devnen.URL, "", 0.7, 0.3, 0, logger)

	player := &recPlayer{ch: make(chan struct{}, 4)}
	q := NewQueue(client, player, t.TempDir(), 100, logger)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go q.Run(ctx)

	ts := httptest.NewServer(NewServer(q, nil, nil, "", 500, logger).Handler())
	t.Cleanup(ts.Close)

	body, _ := json.Marshal(map[string]string{"text": "this is dramatic"})
	resp, err := http.Post(ts.URL+"/say", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("/say status=%d want 202", resp.StatusCode)
	}
	waitPlay(t, player.ch)

	recs := player.records()
	if len(recs) != 1 {
		t.Fatalf("plays=%d want 1", len(recs))
	}
	if recs[0].data != "RIFF-chatterbox-wav" {
		t.Errorf("played data=%q want the devnen WAV bytes", recs[0].data)
	}
	if !strings.HasPrefix(recs[0].base, "tts-") || !strings.HasSuffix(recs[0].base, ".wav") {
		t.Errorf("played file=%q want tts-<id>.wav", recs[0].base)
	}

	reqs := fd.requests()
	if len(reqs) != 1 || reqs[0].Text != "this is dramatic" {
		t.Fatalf("devnen /tts reqs=%+v want 1 with text %q", reqs, "this is dramatic")
	}
}

// TestChatterboxUnloadCadence: with unloadEvery=N, /api/unload fires once every N
// generations and not in between.
func TestChatterboxUnloadCadence(t *testing.T) {
	fd, devnen := newFakeDevnen(t, "W")
	client := newChatterboxClient(devnen.URL, "", 0.7, 0.3, 3, log.New(io.Discard, "", 0))
	out := filepath.Join(t.TempDir(), "o.wav")

	gen := func(n int) {
		for range n {
			if err := client.Synthesize(context.Background(), "x", "", out); err != nil {
				t.Fatalf("Synthesize: %v", err)
			}
		}
	}

	gen(2)
	if got := fd.unloadCount(); got != 0 {
		t.Errorf("after 2 gens unloads=%d want 0", got)
	}
	gen(1) // 3rd generation triggers the first unload
	if got := fd.unloadCount(); got != 1 {
		t.Errorf("after 3 gens unloads=%d want 1", got)
	}
	gen(3) // 6th generation triggers the second
	if got := fd.unloadCount(); got != 2 {
		t.Errorf("after 6 gens unloads=%d want 2", got)
	}
}

// TestChatterboxNoUnloadWhenDisabled: unloadEvery=0 never calls /api/unload.
func TestChatterboxNoUnloadWhenDisabled(t *testing.T) {
	fd, devnen := newFakeDevnen(t, "W")
	client := newChatterboxClient(devnen.URL, "", 0.7, 0.3, 0, log.New(io.Discard, "", 0))
	out := filepath.Join(t.TempDir(), "o.wav")
	for range 5 {
		if err := client.Synthesize(context.Background(), "x", "", out); err != nil {
			t.Fatalf("Synthesize: %v", err)
		}
	}
	if got := fd.unloadCount(); got != 0 {
		t.Errorf("unloads=%d want 0 when disabled", got)
	}
}
