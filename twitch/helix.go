package twitch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// SendChatMessage posts message to broadcasterID's chat as the authenticated
// user. A non-empty replyParentID threads it as a reply to that message. On a 401
// it refreshes the token once (persisting the new one) and retries.
func (c *Client) SendChatMessage(ctx context.Context, broadcasterID, message, replyParentID string) error {
	payload := map[string]string{
		"broadcaster_id": broadcasterID,
		"sender_id":      c.SenderID(),
		"message":        message,
	}
	if replyParentID != "" {
		payload["reply_parent_message_id"] = replyParentID
	}
	body, _ := json.Marshal(payload)

	resp, err := c.do(ctx, http.MethodPost, c.helixBase+"/chat/messages", body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("send chat message: %s: %s", resp.Status, bytes.TrimSpace(b))
	}

	// A 2xx can still report the message was dropped (e.g. blocked term).
	var out struct {
		Data []struct {
			IsSent     bool `json:"is_sent"`
			DropReason *struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"drop_reason"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err == nil && len(out.Data) > 0 && !out.Data[0].IsSent {
		if dr := out.Data[0].DropReason; dr != nil {
			return fmt.Errorf("message dropped: %s (%s)", dr.Message, dr.Code)
		}
		return fmt.Errorf("message not sent")
	}
	return nil
}

// do performs an authenticated Helix request, refreshing the token once on a 401
// and retrying. The caller owns resp.Body and must close it. body may be nil.
func (c *Client) do(ctx context.Context, method, url string, body []byte) (*http.Response, error) {
	return c.doRetry(ctx, method, url, body, true)
}

func (c *Client) doRetry(ctx context.Context, method, url string, body []byte, allowRefresh bool) (*http.Response, error) {
	tok := c.currentToken()
	if tok == nil {
		return nil, fmt.Errorf("no token; run bot-auth")
	}

	// Refresh before sending when the token is known to have lapsed. Without this,
	// a machine that sleeps through the token's lifetime wakes with every loop —
	// accrual, conversion, events, chat — holding the same dead token, and they all
	// 401 at the same instant. Best-effort on purpose: if the token endpoint is
	// unreachable we still send, because the recorded expiry may be pessimistic and
	// the 401 path below is the real safety net. Gated on allowRefresh so the retry
	// can't re-enter it.
	if allowRefresh && tok.needsRefresh(time.Now()) {
		if err := c.ensureFresh(ctx, tok.AccessToken); err != nil {
			c.logf("twitch: refresh before send: %v", err)
		} else if fresh := c.currentToken(); fresh != nil {
			tok = fresh
		}
	}

	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	req.Header.Set("Client-Id", c.clientID)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized && allowRefresh {
		resp.Body.Close()
		if err := c.ensureFresh(ctx, tok.AccessToken); err != nil {
			return nil, fmt.Errorf("refresh after 401: %w", err)
		}
		return c.doRetry(ctx, method, url, body, false)
	}
	return resp, nil
}

// ensureFresh guarantees the client holds a token newer than staleAccess — the
// access token whose request just 401'd, or that we spotted as expired before
// sending. Only one refresh runs at a time: the winner exchanges, and everyone
// queued behind it sees c.token has already moved past staleAccess and returns
// without exchanging again. That is what turns a wake-from-sleep 401 storm into a
// single call; concurrent exchanges race to rotate the same refresh token and
// Twitch fails the losers with "Invalid refresh token".
//
// Failures are deliberately not memoized: the next 401 tries again, which is what
// you want after a transient blip.
func (c *Client) ensureFresh(ctx context.Context, staleAccess string) error {
	select {
	case c.refreshing <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-c.refreshing }()

	tok := c.currentToken()
	if tok == nil || tok.RefreshToken == "" {
		return fmt.Errorf("no refresh token; run bot-auth")
	}
	if tok.AccessToken != staleAccess {
		return nil // someone else refreshed while we queued
	}

	nt, err := c.refresh(ctx, tok.RefreshToken)
	if err != nil {
		return err
	}
	// Carry across whatever the refresh response doesn't return. Identity never is.
	// refresh_token and scope normally are, but taking an omission literally would
	// wipe the credential we need to refresh again — and, via the store, the scopes
	// bot/main.go reads at startup to decide whether the marks economy comes up.
	nt.UserID, nt.Login = tok.UserID, tok.Login
	if nt.RefreshToken == "" {
		nt.RefreshToken = tok.RefreshToken
	}
	if len(nt.Scope) == 0 {
		nt.Scope = tok.Scope
	}

	c.mu.Lock()
	c.token = nt
	c.mu.Unlock()

	// Persisting is deliberately best-effort. The new token is already live, so the
	// caller's retry will succeed; returning this error would turn a recoverable 401
	// into a dropped redemption poll — which is exactly what the shared
	// bot.tokens.json.tmp rename race used to do. A lost write only bites on the
	// next restart (the stored refresh token may have been rotated away), so it's
	// logged loudly rather than swallowed.
	if c.store != nil {
		if err := c.store.Save(nt); err != nil {
			c.logf("twitch: refreshed token NOT persisted — a restart may need 'mise run bot:auth': %v", err)
		}
	}
	return nil
}
