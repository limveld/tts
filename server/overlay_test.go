package main

import (
	"bufio"
	"context"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newTestOverlayServer builds an Overlay with the given token and returns an
// httptest server with its routes mounted.
func newTestOverlayServer(t *testing.T, token string) *httptest.Server {
	t.Helper()
	o := NewOverlay(t.TempDir(), token, log.New(io.Discard, "", 0))
	mux := http.NewServeMux()
	o.routes(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestOverlayServesEmbeddedPage(t *testing.T) {
	srv := newTestOverlayServer(t, "")

	resp, err := http.Get(srv.URL + "/overlay")
	if err != nil {
		t.Fatalf("GET /overlay: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /overlay status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "/overlay/overlay.js") {
		t.Errorf("page does not reference the overlay script:\n%s", body)
	}
}

func TestOverlayServesEmbeddedAssets(t *testing.T) {
	srv := newTestOverlayServer(t, "")

	for _, asset := range []string{"/overlay/overlay.js", "/overlay/overlay.css"} {
		resp, err := http.Get(srv.URL + asset)
		if err != nil {
			t.Fatalf("GET %s: %v", asset, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("GET %s status = %d, want 200", asset, resp.StatusCode)
		}
		if len(body) == 0 {
			t.Errorf("GET %s returned empty body", asset)
		}
	}
}

// readSSEEvent reads from an SSE stream until it sees an "event: <name>" frame
// with the matching data line, or the context/timeout fires.
func readSSEEvent(t *testing.T, r *bufio.Reader, want string) string {
	t.Helper()
	var lastEvent string
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("reading SSE: %v", err)
		}
		line = strings.TrimRight(line, "\n")
		switch {
		case strings.HasPrefix(line, "event: "):
			lastEvent = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: ") && lastEvent == want:
			return strings.TrimPrefix(line, "data: ")
		}
	}
}

func postState(t *testing.T, base, body string) *http.Response {
	t.Helper()
	resp, err := http.Post(base+"/overlay/state", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST /overlay/state: %v", err)
	}
	return resp
}

func TestOverlayStateBroadcast(t *testing.T) {
	srv := newTestOverlayServer(t, "")

	// Connect an SSE client first.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/overlay/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("connect events: %v", err)
	}
	defer resp.Body.Close()
	r := bufio.NewReader(resp.Body)

	// Give the server a moment to register the client, then push state.
	time.Sleep(50 * time.Millisecond)
	if pr := postState(t, srv.URL, `{"kind":"gamble","data":{"pot":40,"players":2}}`); pr.StatusCode != http.StatusNoContent {
		t.Fatalf("POST state = %d, want 204", pr.StatusCode)
	}

	got := readSSEEvent(t, r, "gamble")
	if !strings.Contains(got, `"pot":40`) || !strings.Contains(got, `"players":2`) {
		t.Errorf("gamble event data = %q, want pot/players", got)
	}
}

func TestOverlayStateReplayOnConnect(t *testing.T) {
	srv := newTestOverlayServer(t, "")

	// Push state before any client connects.
	if pr := postState(t, srv.URL, `{"kind":"depth","data":{"points":4400}}`); pr.StatusCode != http.StatusNoContent {
		t.Fatalf("POST state = %d, want 204", pr.StatusCode)
	}

	// A freshly-connected client should get the cached state replayed.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/overlay/events", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("connect events: %v", err)
	}
	defer resp.Body.Close()

	got := readSSEEvent(t, bufio.NewReader(resp.Body), "depth")
	if !strings.Contains(got, `"points":4400`) {
		t.Errorf("replayed depth event = %q, want points:4400", got)
	}
}

func TestOverlayStateRejectsUnknownKind(t *testing.T) {
	srv := newTestOverlayServer(t, "")
	resp := postState(t, srv.URL, `{"kind":"bogus","data":{}}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("unknown kind status = %d, want 400", resp.StatusCode)
	}
}

func TestOverlayPageRequiresToken(t *testing.T) {
	srv := newTestOverlayServer(t, "secret")

	// No token -> 401.
	resp, err := http.Get(srv.URL + "/overlay")
	if err != nil {
		t.Fatalf("GET /overlay: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("GET /overlay without token = %d, want 401", resp.StatusCode)
	}

	// With token -> 200.
	resp, err = http.Get(srv.URL + "/overlay?token=secret")
	if err != nil {
		t.Fatalf("GET /overlay?token: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /overlay?token=secret = %d, want 200", resp.StatusCode)
	}
}
