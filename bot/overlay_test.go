package main

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestOverlayClientPush(t *testing.T) {
	type received struct {
		Kind string          `json:"kind"`
		Data json.RawMessage `json:"data"`
	}
	got := make(chan received, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/overlay/state" {
			t.Errorf("path = %q, want /overlay/state", r.URL.Path)
		}
		if r.URL.Query().Get("token") != "sekret" {
			t.Errorf("token = %q, want sekret", r.URL.Query().Get("token"))
		}
		var body received
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode: %v", err)
		}
		got <- body
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := NewOverlayClient(srv.URL, "sekret", log.New(io.Discard, "", 0))
	c.Push("depth", map[string]any{"points": 4400})

	select {
	case r := <-got:
		if r.Kind != "depth" {
			t.Errorf("kind = %q, want depth", r.Kind)
		}
		if string(r.Data) != `{"points":4400}` {
			t.Errorf("data = %s, want {\"points\":4400}", r.Data)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no push received")
	}
}

func TestOverlayClientPreservesOrder(t *testing.T) {
	got := make(chan int, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Data struct {
				N int `json:"n"`
			} `json:"data"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		got <- body.Data.N
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := NewOverlayClient(srv.URL, "", log.New(io.Discard, "", 0))
	for i := 0; i < 5; i++ {
		c.Push("gamble", map[string]any{"n": i})
	}
	for i := 0; i < 5; i++ {
		select {
		case n := <-got:
			if n != i {
				t.Errorf("push %d arrived out of order as %d", i, n)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("missing push %d", i)
		}
	}
}
