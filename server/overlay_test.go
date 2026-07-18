package main

import (
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
