package twitch

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// AuthCodeURL builds the consent URL for the Authorization Code flow. Send the
// user here; Twitch redirects back to redirect with ?code=&state=.
func (c *Client) AuthCodeURL(redirect, state string, scopes []string) string {
	q := url.Values{}
	q.Set("client_id", c.clientID)
	q.Set("redirect_uri", redirect)
	q.Set("response_type", "code")
	q.Set("scope", strings.Join(scopes, " "))
	q.Set("state", state)
	return c.idBase + "/authorize?" + q.Encode()
}

// Exchange trades an authorization code for a token pair.
func (c *Client) Exchange(ctx context.Context, code, redirect string) (*Token, error) {
	form := url.Values{}
	form.Set("client_id", c.clientID)
	form.Set("client_secret", c.clientSecret)
	form.Set("code", code)
	form.Set("grant_type", "authorization_code")
	form.Set("redirect_uri", redirect)
	return c.tokenRequest(ctx, form)
}

// refresh exchanges a refresh token for a fresh pair.
func (c *Client) refresh(ctx context.Context, refreshToken string) (*Token, error) {
	form := url.Values{}
	form.Set("client_id", c.clientID)
	form.Set("client_secret", c.clientSecret)
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	return c.tokenRequest(ctx, form)
}

func (c *Client) tokenRequest(ctx context.Context, form url.Values) (*Token, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.idBase+"/token", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return nil, fmt.Errorf("token endpoint: %s: %s", resp.Status, bytes.TrimSpace(b))
	}
	var tr struct {
		AccessToken  string   `json:"access_token"`
		RefreshToken string   `json:"refresh_token"`
		ExpiresIn    int      `json:"expires_in"`
		Scope        []string `json:"scope"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return nil, err
	}
	t := &Token{
		AccessToken:  tr.AccessToken,
		RefreshToken: tr.RefreshToken,
		Scope:        tr.Scope,
	}
	if tr.ExpiresIn > 0 {
		t.Expiry = time.Now().Add(time.Duration(tr.ExpiresIn) * time.Second)
	}
	return t, nil
}

// Validate returns the user id and login an access token belongs to (used at auth
// time to learn the bot's own sender_id).
func (c *Client) Validate(ctx context.Context, accessToken string) (userID, login string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.idBase+"/validate", nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Authorization", "OAuth "+accessToken)
	resp, err := c.http.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("validate: %s", resp.Status)
	}
	var v struct {
		UserID string `json:"user_id"`
		Login  string `json:"login"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return "", "", err
	}
	return v.UserID, v.Login, nil
}
