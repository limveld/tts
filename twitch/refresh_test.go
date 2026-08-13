package twitch

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// refreshServer is a Twitch stand-in for the refresh path: it counts token
// exchanges, records the Authorization header of every Helix request, and 401s
// anything still carrying oldAccess.
type refreshServer struct {
	mu           sync.Mutex
	refreshCalls int
	auths        []string
	sentRefresh  []string

	// exchangeDelay widens the window in which concurrent callers can pile up.
	exchangeDelay time.Duration
	// omitFields drops refresh_token and scope from the response.
	omitFields bool
	// alwaysOK serves Helix 200 regardless of the token, so the only way a
	// refresh can happen is proactively.
	alwaysOK bool
}

func (s *refreshServer) start(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()

	mux.HandleFunc("/oauth2/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		s.mu.Lock()
		s.refreshCalls++
		s.sentRefresh = append(s.sentRefresh, r.PostForm.Get("refresh_token"))
		delay := s.exchangeDelay
		omit := s.omitFields
		s.mu.Unlock()

		if g := r.PostForm.Get("grant_type"); g != "refresh_token" {
			t.Errorf("grant_type=%q want refresh_token", g)
		}
		time.Sleep(delay)

		body := map[string]any{"access_token": "new-access", "expires_in": 3600}
		if !omit {
			body["refresh_token"] = "r2"
			body["scope"] = []string{"user:write:chat", "channel:read:redemptions"}
		}
		_ = json.NewEncoder(w).Encode(body)
	})

	helix := func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.auths = append(s.auths, r.Header.Get("Authorization"))
		ok := s.alwaysOK
		s.mu.Unlock()

		if !ok && r.Header.Get("Authorization") == "Bearer old-access" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{{"id": "1", "type": "live", "game_name": "Go", "title": "t"}},
		})
	}
	mux.HandleFunc("/helix/streams", helix)
	mux.HandleFunc("/helix/chat/messages", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.auths = append(s.auths, r.Header.Get("Authorization"))
		s.mu.Unlock()
		if r.Header.Get("Authorization") == "Bearer old-access" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"is_sent": true}}})
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func (s *refreshServer) calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refreshCalls
}

func (s *refreshServer) firstAuth() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.auths) == 0 {
		return ""
	}
	return s.auths[0]
}

// newRefreshClient wires a client to srv with the given starting token.
func newRefreshClient(srv *httptest.Server, store *Store, tok *Token) *Client {
	c := NewClient("cid", "secret", store)
	c.idBase = srv.URL + "/oauth2"
	c.helixBase = srv.URL + "/helix"
	c.SetToken(tok)
	return c
}

// TestConcurrent401RefreshesOnce is the regression test for the bug that took
// redemption polling down after a wake from sleep: accrual, conversion, events
// and chat all 401'd at the same instant and each ran its own exchange, racing to
// rotate the same refresh token and racing each other's writes to the store.
func TestConcurrent401RefreshesOnce(t *testing.T) {
	s := &refreshServer{exchangeDelay: 50 * time.Millisecond}
	srv := s.start(t)
	store := NewStore(filepath.Join(t.TempDir(), "tok.json"))
	c := newRefreshClient(srv, store, &Token{
		AccessToken: "old-access", RefreshToken: "r1", UserID: "u123", Login: "bot",
	})

	const n = 8
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = c.IsLive(context.Background(), "b1")
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("caller %d: %v", i, err)
		}
	}
	if got := s.calls(); got != 1 {
		t.Errorf("refreshCalls=%d want 1 (the herd must collapse to one exchange)", got)
	}
	s.mu.Lock()
	sent := append([]string(nil), s.sentRefresh...)
	s.mu.Unlock()
	for _, r := range sent {
		if r != "r1" {
			t.Errorf("exchanged refresh_token=%q want r1", r)
		}
	}

	saved, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if saved == nil || saved.AccessToken != "new-access" || saved.UserID != "u123" || saved.Login != "bot" {
		t.Errorf("persisted token=%+v", saved)
	}
}

// TestRefreshSurvivesSaveFailure pins the second half of the outage: the token was
// already live in memory, but returning the persist error aborted the retry and
// dropped the request entirely.
func TestRefreshSurvivesSaveFailure(t *testing.T) {
	s := &refreshServer{}
	srv := s.start(t)

	// A directory where the file should go: the rename can never land.
	path := filepath.Join(t.TempDir(), "tok.json")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}

	var logs bytes.Buffer
	c := newRefreshClient(srv, NewStore(path), &Token{
		AccessToken: "old-access", RefreshToken: "r1", UserID: "u123", Login: "bot",
	})
	c.SetLogger(log.New(&logs, "", 0))

	if err := c.SendChatMessage(context.Background(), "b1", "hi", ""); err != nil {
		t.Fatalf("send = %v; want nil (a failed persist must not fail the request)", err)
	}
	if got := s.calls(); got != 1 {
		t.Errorf("refreshCalls=%d want 1", got)
	}
	s.mu.Lock()
	auths := append([]string(nil), s.auths...)
	s.mu.Unlock()
	if len(auths) != 2 || auths[1] != "Bearer new-access" {
		t.Errorf("auths=%v want the retry to carry Bearer new-access", auths)
	}
	if !strings.Contains(logs.String(), "NOT persisted") {
		t.Errorf("logs=%q want a loud persist warning", logs.String())
	}
}

// TestProactiveRefresh covers refreshing off Token.Expiry before sending, which is
// what stops a woken-from-sleep process from 401-storming in the first place. The
// Helix handler always succeeds, so a refresh can only happen proactively.
func TestProactiveRefresh(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name      string
		expiry    time.Time
		want      int
		wantFirst string
	}{
		{"expired", now.Add(-time.Hour), 1, "Bearer new-access"},
		{"within skew", now.Add(30 * time.Second), 1, "Bearer new-access"},
		{"fresh", now.Add(time.Hour), 0, "Bearer old-access"},
		{"unknown expiry", time.Time{}, 0, "Bearer old-access"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := &refreshServer{alwaysOK: true}
			srv := s.start(t)
			store := NewStore(filepath.Join(t.TempDir(), "tok.json"))
			c := newRefreshClient(srv, store, &Token{
				AccessToken: "old-access", RefreshToken: "r1",
				Expiry: tc.expiry, UserID: "u123", Login: "bot",
			})

			if _, err := c.IsLive(context.Background(), "b1"); err != nil {
				t.Fatal(err)
			}
			if got := s.calls(); got != tc.want {
				t.Errorf("refreshCalls=%d want %d", got, tc.want)
			}
			if got := s.firstAuth(); got != tc.wantFirst {
				t.Errorf("first auth=%q want %q", got, tc.wantFirst)
			}
		})
	}
}

// TestRefreshPreservesOmittedFields: a response that omits refresh_token would
// otherwise wipe the credential we need next time, and one that omits scope would
// leave a stored token that bot/main.go reads at startup as lacking the economy
// scopes — silently disabling marks after a restart.
func TestRefreshPreservesOmittedFields(t *testing.T) {
	s := &refreshServer{omitFields: true}
	srv := s.start(t)
	store := NewStore(filepath.Join(t.TempDir(), "tok.json"))
	scopes := []string{"user:write:chat", "channel:read:redemptions", "moderator:read:chatters"}
	c := newRefreshClient(srv, store, &Token{
		AccessToken: "old-access", RefreshToken: "r1", Scope: scopes, UserID: "u123", Login: "bot",
	})

	if _, err := c.IsLive(context.Background(), "b1"); err != nil {
		t.Fatal(err)
	}
	saved, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if saved == nil {
		t.Fatal("nothing persisted")
	}
	if saved.AccessToken != "new-access" {
		t.Errorf("access=%q want new-access", saved.AccessToken)
	}
	if saved.RefreshToken != "r1" {
		t.Errorf("refresh=%q want r1 carried across", saved.RefreshToken)
	}
	if strings.Join(saved.Scope, " ") != strings.Join(scopes, " ") {
		t.Errorf("scope=%v want %v carried across", saved.Scope, scopes)
	}
}
