package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
)

// OverlayPusher pushes render state (gamble/depth/wordle) to the TTS server,
// which broadcasts it to the OBS overlay over SSE and caches it for replay. The
// bot owns all game state; the overlay is a pure render target. An interface so
// game code can be tested with a fake and stays a no-op when the overlay isn't
// configured (nil pusher).
type OverlayPusher interface {
	Push(kind string, data any)
}

// OverlayClient POSTs render state to the server's /overlay/state endpoint,
// reusing the TTS server's base URL + token. Pushes are serialized through a
// single worker so per-kind ordering is preserved (a later state always lands
// after an earlier one), and enqueuing never blocks chat handling: a full queue
// drops the push rather than stalling the caller.
type OverlayClient struct {
	url    string // full /overlay/state URL, incl. ?token= when set
	http   *http.Client
	logger *log.Logger
	queue  chan []byte
}

// NewOverlayClient builds the pusher and starts its background sender. baseURL
// and token are the same values the bot uses for the TTS API; the overlay
// authenticates via the ?token= query param (like the browser source does).
func NewOverlayClient(baseURL, token string, logger *log.Logger) *OverlayClient {
	u := strings.TrimRight(baseURL, "/") + "/overlay/state"
	if token != "" {
		u += "?token=" + token
	}
	c := &OverlayClient{
		url:    u,
		http:   &http.Client{Timeout: 10 * time.Second},
		logger: logger,
		queue:  make(chan []byte, 64),
	}
	go c.run()
	return c
}

// Push enqueues a state push for kind (gamble/depth/wordle). Non-blocking: if the
// send queue is full (server wedged), the push is dropped with a log line rather
// than stalling the chat handler or a game timer.
func (c *OverlayClient) Push(kind string, data any) {
	body, err := json.Marshal(map[string]any{"kind": kind, "data": data})
	if err != nil {
		c.logger.Printf("overlay push %s: marshal: %v", kind, err)
		return
	}
	select {
	case c.queue <- body:
	default:
		c.logger.Printf("overlay push %s: queue full, dropped", kind)
	}
}

// run sends queued pushes in order, one at a time.
func (c *OverlayClient) run() {
	for body := range c.queue {
		if err := c.send(body); err != nil {
			c.logger.Printf("overlay push: %v", err)
		}
	}
}

// send POSTs one already-marshaled {kind,data} body. Synchronous; used by run
// and directly by tests.
func (c *OverlayClient) send(body []byte) error {
	req, err := http.NewRequest(http.MethodPost, c.url, bytes.NewReader(body))
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
		return fmt.Errorf("/overlay/state -> %s", resp.Status)
	}
	return nil
}
