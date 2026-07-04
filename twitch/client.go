// Package twitch implements the small slice of Twitch's OAuth and Helix APIs the
// bot needs to post chat messages: the Authorization Code flow (one-time consent
// plus unattended refresh) and the Send Chat Message endpoint. Standard library
// only — no third-party SDK.
package twitch

import (
	"net/http"
	"sync"
	"time"
)

// Token is an OAuth token pair plus the identity it was issued to. The identity
// (UserID/Login) is the Helix sender_id and is learned once via Validate at auth
// time; the refresh endpoint doesn't return it, so it's carried across refreshes.
type Token struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	Expiry       time.Time `json:"expiry,omitempty"`
	Scope        []string  `json:"scope,omitempty"`
	UserID       string    `json:"user_id"`
	Login        string    `json:"login"`
}

// Client talks to Twitch OAuth + Helix. It holds the app credentials, the current
// token, and a store to persist refreshed tokens. Zero value is not usable — use
// NewClient. Safe for concurrent use.
type Client struct {
	clientID     string
	clientSecret string
	store        *Store
	http         *http.Client

	// Base URLs, overridable in tests. Default to the real Twitch endpoints.
	idBase    string // .../oauth2
	helixBase string // .../helix

	mu    sync.Mutex
	token *Token
}

// NewClient builds a client for the given app credentials. store is used to
// persist tokens refreshed on a 401 (may be nil in flows that don't send).
func NewClient(clientID, clientSecret string, store *Store) *Client {
	return &Client{
		clientID:     clientID,
		clientSecret: clientSecret,
		store:        store,
		http:         &http.Client{Timeout: 10 * time.Second},
		idBase:       "https://id.twitch.tv/oauth2",
		helixBase:    "https://api.twitch.tv/helix",
	}
}

// SetToken installs the token the client should send with (loaded from the store
// at startup).
func (c *Client) SetToken(t *Token) {
	c.mu.Lock()
	c.token = t
	c.mu.Unlock()
}

// SenderID returns the authorized user's id (the Helix sender_id), or "" if no
// token is set.
func (c *Client) SenderID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token == nil {
		return ""
	}
	return c.token.UserID
}
