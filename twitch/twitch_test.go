package twitch

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestAuthCodeURL(t *testing.T) {
	c := NewClient("cid", "secret", nil)
	got := c.AuthCodeURL("http://localhost:3000", "st8", []string{"user:write:chat", "user:bot"})
	u, err := url.Parse(got)
	if err != nil {
		t.Fatal(err)
	}
	if u.Host != "id.twitch.tv" || u.Path != "/oauth2/authorize" {
		t.Errorf("base=%s://%s%s", u.Scheme, u.Host, u.Path)
	}
	q := u.Query()
	if q.Get("client_id") != "cid" || q.Get("redirect_uri") != "http://localhost:3000" ||
		q.Get("response_type") != "code" || q.Get("state") != "st8" ||
		q.Get("scope") != "user:write:chat user:bot" {
		t.Errorf("params=%v", q)
	}
}

func TestExchange(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/oauth2/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if g := r.PostForm.Get("grant_type"); g != "authorization_code" {
			t.Errorf("grant_type=%q want authorization_code", g)
		}
		if r.PostForm.Get("code") != "abc" || r.PostForm.Get("client_secret") != "secret" {
			t.Errorf("form=%v", r.PostForm)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "acc", "refresh_token": "ref", "expires_in": 3600,
			"scope": []string{"user:write:chat"},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := NewClient("cid", "secret", NewStore(filepath.Join(t.TempDir(), "t.json")))
	c.idBase = srv.URL + "/oauth2"

	tok, err := c.Exchange(context.Background(), "abc", "http://localhost:3000")
	if err != nil {
		t.Fatal(err)
	}
	if tok.AccessToken != "acc" || tok.RefreshToken != "ref" {
		t.Errorf("tok=%+v", tok)
	}
}

func TestSendChatMessageRefreshesOn401(t *testing.T) {
	var mu sync.Mutex
	sendCalls, refreshCalls := 0, 0
	var lastAuth string
	var sentBody map[string]string

	mux := http.NewServeMux()
	mux.HandleFunc("/oauth2/token", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		_ = r.ParseForm()
		refreshCalls++
		if g := r.PostForm.Get("grant_type"); g != "refresh_token" {
			t.Errorf("grant_type=%q want refresh_token", g)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "new-access", "refresh_token": "new-refresh", "expires_in": 3600,
		})
	})
	mux.HandleFunc("/helix/chat/messages", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		sendCalls++
		lastAuth = r.Header.Get("Authorization")
		if got := r.Header.Get("Client-Id"); got != "cid" {
			t.Errorf("Client-Id=%q want cid", got)
		}
		if sendCalls == 1 {
			w.WriteHeader(http.StatusUnauthorized) // force a refresh + retry
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&sentBody)
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]any{{"is_sent": true}}})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	store := NewStore(filepath.Join(t.TempDir(), "tok.json"))
	c := NewClient("cid", "secret", store)
	c.idBase = srv.URL + "/oauth2"
	c.helixBase = srv.URL + "/helix"
	c.SetToken(&Token{AccessToken: "old-access", RefreshToken: "r1", UserID: "u123", Login: "botacct"})

	if err := c.SendChatMessage(context.Background(), "b456", "hi", "msg789"); err != nil {
		t.Fatalf("send: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if sendCalls != 2 || refreshCalls != 1 {
		t.Fatalf("sendCalls=%d refreshCalls=%d want 2/1", sendCalls, refreshCalls)
	}
	if lastAuth != "Bearer new-access" {
		t.Errorf("retry Authorization=%q want Bearer new-access", lastAuth)
	}
	if sentBody["broadcaster_id"] != "b456" || sentBody["sender_id"] != "u123" ||
		sentBody["message"] != "hi" || sentBody["reply_parent_message_id"] != "msg789" {
		t.Errorf("body=%v", sentBody)
	}
	if saved, _ := store.Load(); saved == nil || saved.AccessToken != "new-access" || saved.UserID != "u123" {
		t.Errorf("refreshed token not persisted: %+v", saved)
	}
}

func TestStoreRoundTrip(t *testing.T) {
	s := NewStore(filepath.Join(t.TempDir(), "tok.json"))
	if tok, err := s.Load(); err != nil || tok != nil {
		t.Fatalf("empty load = %+v, %v; want nil,nil", tok, err)
	}
	in := &Token{AccessToken: "a", RefreshToken: "r", UserID: "u", Login: "l"}
	if err := s.Save(in); err != nil {
		t.Fatal(err)
	}
	out, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if out.AccessToken != "a" || out.Login != "l" || out.UserID != "u" {
		t.Errorf("out=%+v", out)
	}
	fi, err := os.Stat(filepath.Join(filepath.Dir(s.path), filepath.Base(s.path)))
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 { // it holds a live credential
		t.Errorf("perm=%o want 600", perm)
	}
}

// TestStoreConcurrentSave is the regression test for the ENOENT that took
// redemption polling down: every saver used to write the same <path>.tmp, so the
// first rename moved it away and the rest failed with "no such file or directory"
// — and a rename could publish another saver's bytes.
func TestStoreConcurrentSave(t *testing.T) {
	dir := t.TempDir()
	s := NewStore(filepath.Join(dir, "tok.json"))

	const n = 16
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = s.Save(&Token{
				AccessToken:  fmt.Sprintf("a%d", i),
				RefreshToken: fmt.Sprintf("r%d", i),
				UserID:       "u",
				Login:        "l",
			})
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("save %d: %v", i, err)
		}
	}

	// Whichever save landed last must be readable in full — never a torn file.
	out, err := s.Load()
	if err != nil {
		t.Fatal(err)
	}
	if out == nil || !strings.HasPrefix(out.AccessToken, "a") || out.UserID != "u" {
		t.Errorf("out=%+v", out)
	}

	// No temp file may survive: it holds the same secrets as the store itself.
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 1 || ents[0].Name() != "tok.json" {
		names := make([]string, len(ents))
		for i, e := range ents {
			names[i] = e.Name()
		}
		t.Errorf("dir=%v want just tok.json", names)
	}
}
