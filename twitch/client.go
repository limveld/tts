// Package twitch implements the small slice of Twitch's OAuth and Helix APIs the
// bot needs to post chat messages: the Authorization Code flow (one-time consent
// plus unattended refresh) and the Send Chat Message endpoint. Standard library
// only — no third-party SDK.
package twitch

import (
	"log"
	"net/http"
	"sync"
	"time"
)

// refreshSkew is how far ahead of the recorded expiry a token counts as needing a
// refresh. Twitch access tokens live ~4h, so a couple of minutes costs one extra
// exchange per lifetime and buys margin for clock drift and for a request already
// in flight when the token lapses.
const refreshSkew = 2 * time.Minute

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

// needsRefresh reports whether the token should be replaced before it's sent. A
// zero Expiry means "unknown" — tokens written before the bot recorded one — and
// is deliberately not treated as expired: guessing would refresh on every single
// request. Those tokens keep the reactive 401 path instead.
func (t *Token) needsRefresh(now time.Time) bool {
	return !t.Expiry.IsZero() && !now.Before(t.Expiry.Add(-refreshSkew))
}

// Client talks to Twitch OAuth + Helix. It holds the app credentials, the current
// token, and a store to persist refreshed tokens. Zero value is not usable — use
// NewClient.
//
// Safe for concurrent use: mu is only ever held to read or swap fields, never
// across a network call or a disk write, and an installed *Token is never mutated
// in place — a refresh builds a new one and swaps the pointer.
type Client struct {
	clientID     string
	clientSecret string
	store        *Store
	http         *http.Client

	// Base URLs, overridable in tests. Default to the real Twitch endpoints.
	idBase    string // .../oauth2
	helixBase string // .../helix

	mu     sync.Mutex
	token  *Token
	errLog *log.Logger

	// refreshing is a one-slot semaphore serializing token refreshes. Three ticker
	// goroutines plus chat share this client, so after a long sleep they all 401 at
	// the same instant; without this they each exchange the same refresh token, and
	// Twitch rotates it — the losers get "Invalid refresh token". A channel rather
	// than a sync.Mutex because a waiter has to be able to give up when its request
	// context dies (the loops use 10-15s deadlines); Mutex.Lock isn't cancellable.
	refreshing chan struct{}
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
		refreshing:   make(chan struct{}, 1),
	}
}

// SetToken installs the token the client should send with (loaded from the store
// at startup).
func (c *Client) SetToken(t *Token) {
	c.mu.Lock()
	c.token = t
	c.mu.Unlock()
}

// SetLogger installs a logger for problems the client can't hand back to a caller
// — today only "the refreshed token is live but couldn't be written to disk",
// which must not fail the request that triggered the refresh. Call it before the
// first request. nil (the default, and what cmd/bot-auth leaves it as) discards
// them.
func (c *Client) SetLogger(l *log.Logger) {
	c.mu.Lock()
	c.errLog = l
	c.mu.Unlock()
}

func (c *Client) logf(format string, args ...any) {
	c.mu.Lock()
	l := c.errLog
	c.mu.Unlock()
	if l != nil {
		l.Printf(format, args...)
	}
}

// currentToken returns the token to send with, or nil when unauthorized. The
// result is safe to read after the lock is released because installed tokens are
// never mutated in place.
func (c *Client) currentToken() *Token {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.token
}

// SenderID returns the authorized user's id (the Helix sender_id), or "" if no
// token is set.
func (c *Client) SenderID() string {
	if t := c.currentToken(); t != nil {
		return t.UserID
	}
	return ""
}
