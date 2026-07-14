package twitch

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

// newTestClient wires a Client to a test server with a token already set.
func newTestClient(t *testing.T, mux *http.ServeMux) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := NewClient("cid", "secret", NewStore(filepath.Join(t.TempDir(), "tok.json")))
	c.idBase = srv.URL + "/oauth2"
	c.helixBase = srv.URL + "/helix"
	c.SetToken(&Token{AccessToken: "acc", RefreshToken: "r", UserID: "broadcaster1", Login: "streamer"})
	return c, srv
}

func TestGetChattersPaginates(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/helix/chat/chatters", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("moderator_id") != "broadcaster1" {
			t.Errorf("moderator_id=%q", r.URL.Query().Get("moderator_id"))
		}
		if r.URL.Query().Get("after") == "" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data":       []map[string]string{{"user_id": "u1", "user_login": "bob", "user_name": "Bob"}},
				"pagination": map[string]string{"cursor": "next"},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data":       []map[string]string{{"user_id": "u2", "user_login": "amy", "user_name": "Amy"}},
			"pagination": map[string]string{},
		})
	})
	c, _ := newTestClient(t, mux)

	got, err := c.GetChatters(context.Background(), "broadcaster1", "broadcaster1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].UserID != "u1" || got[1].Display != "Amy" {
		t.Fatalf("chatters=%+v", got)
	}
}

func TestIsLive(t *testing.T) {
	for _, tc := range []struct {
		name string
		data []map[string]string
		want bool
	}{
		{"live", []map[string]string{{"type": "live"}}, true},
		{"offline", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("/helix/streams", func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]any{"data": tc.data})
			})
			c, _ := newTestClient(t, mux)
			got, err := c.IsLive(context.Background(), "broadcaster1")
			if err != nil || got != tc.want {
				t.Fatalf("IsLive=%v err=%v want %v", got, err, tc.want)
			}
		})
	}
}

func TestEnsureRewardExistingVsCreate(t *testing.T) {
	t.Run("existing", func(t *testing.T) {
		mux := http.NewServeMux()
		mux.HandleFunc("/helix/channel_points/custom_rewards", func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodGet {
				t.Fatalf("unexpected %s (should not create)", r.Method)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": []map[string]string{{"id": "reward-existing", "title": "Convert to Marks"}},
			})
		})
		c, _ := newTestClient(t, mux)
		id, err := c.EnsureReward(context.Background(), "broadcaster1", "Convert to Marks", 1000, "convert")
		if err != nil || id != "reward-existing" {
			t.Fatalf("id=%q err=%v want reward-existing", id, err)
		}
	})

	t.Run("create", func(t *testing.T) {
		var created bool
		mux := http.NewServeMux()
		mux.HandleFunc("/helix/channel_points/custom_rewards", func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet {
				_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]string{}})
				return
			}
			created = true
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["title"] != "Convert to Marks" || body["cost"].(float64) != 1000 {
				t.Errorf("create body=%v", body)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": []map[string]string{{"id": "reward-new"}},
			})
		})
		c, _ := newTestClient(t, mux)
		id, err := c.EnsureReward(context.Background(), "broadcaster1", "Convert to Marks", 1000, "convert")
		if err != nil || id != "reward-new" || !created {
			t.Fatalf("id=%q err=%v created=%v want reward-new/true", id, err, created)
		}
	})
}

func TestGetUsers(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/helix/users", func(w http.ResponseWriter, r *http.Request) {
		logins := r.URL.Query()["login"]
		if len(logins) != 1 || logins[0] != "bob" {
			// Unknown login → empty data.
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]string{}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]string{{"id": "u1", "login": "bob", "display_name": "Bob"}},
		})
	})
	c, _ := newTestClient(t, mux)

	got, err := c.GetUsers(context.Background(), "bob")
	if err != nil || len(got) != 1 || got[0].ID != "u1" || got[0].Display != "Bob" {
		t.Fatalf("GetUsers bob = %+v err=%v", got, err)
	}
	if got, err := c.GetUsers(context.Background(), "ghost"); err != nil || len(got) != 0 {
		t.Fatalf("GetUsers ghost = %+v err=%v want empty", got, err)
	}
}

func TestRedemptionsFetchAndFulfill(t *testing.T) {
	var fulfilledIDs []string
	mux := http.NewServeMux()
	mux.HandleFunc("/helix/channel_points/custom_rewards/redemptions", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			if r.URL.Query().Get("status") != "UNFULFILLED" {
				t.Errorf("status=%q", r.URL.Query().Get("status"))
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": []map[string]string{
					{"id": "red1", "user_id": "u1", "user_login": "bob", "user_name": "Bob"},
				},
				"pagination": map[string]string{},
			})
		case http.MethodPatch:
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["status"] != "FULFILLED" {
				t.Errorf("patch status=%q", body["status"])
			}
			fulfilledIDs = r.URL.Query()["id"]
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []map[string]string{}})
		}
	})
	c, _ := newTestClient(t, mux)

	reds, err := c.GetRedemptions(context.Background(), "broadcaster1", "reward1", "UNFULFILLED")
	if err != nil || len(reds) != 1 || reds[0].ID != "red1" || reds[0].UserID != "u1" {
		t.Fatalf("redemptions=%+v err=%v", reds, err)
	}
	if err := c.FulfillRedemptions(context.Background(), "broadcaster1", "reward1", []string{"red1"}); err != nil {
		t.Fatal(err)
	}
	if len(fulfilledIDs) != 1 || fulfilledIDs[0] != "red1" {
		t.Fatalf("fulfilledIDs=%v want [red1]", fulfilledIDs)
	}
}
