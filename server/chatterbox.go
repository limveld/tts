package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sync"
	"time"
)

// chatterboxClient is a Synthesizer backed by an external devnen Chatterbox-TTS
// HTTP server (https://github.com/devnen/Chatterbox-TTS-Server). It POSTs text to
// /tts with a fixed dramatic preset and streams the returned WAV to disk. Unlike
// the kokoro Engine there is no local process to manage — the model lives in the
// devnen server — so Ready is always true and the per-request voice code (a kokoro
// concept) is ignored in favor of a single predefined voice.
type chatterboxClient struct {
	url          string
	voice        string // predefined_voice_id; "" => the devnen server's default
	exaggeration float64
	cfgWeight    float64
	unloadEvery  int // 0 => never; else POST /api/unload every N generations
	http         *http.Client
	logger       *log.Logger

	mu    sync.Mutex
	count int
}

// newChatterboxClient builds a client pointed at a running devnen server. The
// generous HTTP timeout is only a backstop; a !skip cancels in-flight requests
// through the request context.
func newChatterboxClient(url, voice string, exaggeration, cfgWeight float64, unloadEvery int, logger *log.Logger) *chatterboxClient {
	return &chatterboxClient{
		url:          url,
		voice:        voice,
		exaggeration: exaggeration,
		cfgWeight:    cfgWeight,
		unloadEvery:  unloadEvery,
		http:         &http.Client{Timeout: 180 * time.Second},
		logger:       logger,
	}
}

// ttsRequest is the JSON body devnen's /tts endpoint expects. predefined_voice_id
// is omitted when empty so the server falls back to its configured default voice.
type ttsRequest struct {
	Text              string  `json:"text"`
	VoiceMode         string  `json:"voice_mode"`
	PredefinedVoiceID string  `json:"predefined_voice_id,omitempty"`
	Exaggeration      float64 `json:"exaggeration"`
	CFGWeight         float64 `json:"cfg_weight"`
	OutputFormat      string  `json:"output_format"`
}

// Synthesize POSTs text to devnen's /tts and streams the WAV response to outPath.
// The voice argument is ignored (kokoro voice codes don't apply); the fixed
// predefined voice + dramatic preset configured on the client are used instead.
func (c *chatterboxClient) Synthesize(ctx context.Context, text, voice, outPath string) error {
	body, err := json.Marshal(ttsRequest{
		Text:              text,
		VoiceMode:         "predefined",
		PredefinedVoiceID: c.voice,
		Exaggeration:      c.exaggeration,
		CFGWeight:         c.cfgWeight,
		OutputFormat:      "wav",
	})
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url+"/tts", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		// Drain a little of the body for context, then fail.
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("chatterbox /tts -> %s: %s", resp.Status, bytes.TrimSpace(snippet))
	}

	out, err := os.Create(outPath)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, resp.Body); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}

	c.maybeUnload(ctx)
	return nil
}

// Ready reports whether the engine can synthesize. The devnen server is external
// and loads lazily, so there is nothing local to wait on.
func (c *chatterboxClient) Ready() bool { return true }

// Ext is the output file extension: devnen returns WAV.
func (c *chatterboxClient) Ext() string { return ".wav" }

// maybeUnload periodically asks devnen to release the model from memory, working
// around an upstream leak (chatterbox #218). It is best-effort: a failed unload is
// logged, not fatal, and devnen reloads the model lazily on the next /tts.
func (c *chatterboxClient) maybeUnload(ctx context.Context) {
	if c.unloadEvery <= 0 {
		return
	}
	c.mu.Lock()
	c.count++
	due := c.count%c.unloadEvery == 0
	c.mu.Unlock()
	if !due {
		return
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url+"/api/unload", nil)
	if err != nil {
		c.logger.Printf("chatterbox unload: %v", err)
		return
	}
	resp, err := c.http.Do(req)
	if err != nil {
		c.logger.Printf("chatterbox unload: %v", err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		c.logger.Printf("chatterbox unload -> %s", resp.Status)
		return
	}
	c.logger.Printf("chatterbox: requested model unload after %d generations", c.count)
}
