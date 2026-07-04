package twitch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// SendChatMessage posts message to broadcasterID's chat as the authenticated
// user. A non-empty replyParentID threads it as a reply to that message. On a 401
// it refreshes the token once (persisting the new one) and retries.
func (c *Client) SendChatMessage(ctx context.Context, broadcasterID, message, replyParentID string) error {
	return c.send(ctx, broadcasterID, message, replyParentID, true)
}

func (c *Client) send(ctx context.Context, broadcasterID, message, replyParentID string, allowRefresh bool) error {
	c.mu.Lock()
	tok := c.token
	c.mu.Unlock()
	if tok == nil {
		return fmt.Errorf("no token; run bot-auth")
	}

	payload := map[string]string{
		"broadcaster_id": broadcasterID,
		"sender_id":      tok.UserID,
		"message":        message,
	}
	if replyParentID != "" {
		payload["reply_parent_message_id"] = replyParentID
	}
	body, _ := json.Marshal(payload)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.helixBase+"/chat/messages", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	req.Header.Set("Client-Id", c.clientID)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized && allowRefresh {
		if err := c.doRefresh(ctx); err != nil {
			return fmt.Errorf("refresh after 401: %w", err)
		}
		return c.send(ctx, broadcasterID, message, replyParentID, false)
	}
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

// doRefresh swaps the current token for a refreshed one and persists it.
func (c *Client) doRefresh(ctx context.Context) error {
	c.mu.Lock()
	tok := c.token
	c.mu.Unlock()
	if tok == nil || tok.RefreshToken == "" {
		return fmt.Errorf("no refresh token")
	}
	nt, err := c.refresh(ctx, tok.RefreshToken)
	if err != nil {
		return err
	}
	// The refresh response omits identity; carry it across.
	nt.UserID = tok.UserID
	nt.Login = tok.Login
	c.mu.Lock()
	c.token = nt
	c.mu.Unlock()
	if c.store != nil {
		if err := c.store.Save(nt); err != nil {
			return fmt.Errorf("persist refreshed token: %w", err)
		}
	}
	return nil
}
