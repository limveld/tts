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

// This file holds the Helix reads/writes the loyalty-points economy needs:
// listing present viewers (watch-time accrual), checking whether the stream is
// live (accrual gating), and the channel-point custom-reward + redemption calls
// (converting Channel Points into marks). All go through Client.do, so they share
// the 401→refresh→retry behaviour with SendChatMessage.

// Chatter is one present viewer from Get Chatters.
type Chatter struct {
	UserID  string
	Login   string
	Display string
}

// Redemption is one channel-point reward redemption.
type Redemption struct {
	ID      string
	UserID  string
	Login   string
	Display string
}

// User is one account from Get Users (used to resolve a login the bot hasn't
// seen in chat yet, for !grant, and to fetch a profile picture for shoutouts).
type User struct {
	ID        string
	Login     string
	Display   string
	AvatarURL string
}

// GetUsers resolves the given logins to accounts via Get Users. Unknown logins
// are simply absent from the result (no error). Needs no special scope.
func (c *Client) GetUsers(ctx context.Context, logins ...string) ([]User, error) {
	if len(logins) == 0 {
		return nil, nil
	}
	q := url.Values{}
	for _, l := range logins {
		q.Add("login", l)
	}
	return c.getUsers(ctx, q)
}

// GetUserByID resolves a single account by user id. ok is false when no such id.
func (c *Client) GetUserByID(ctx context.Context, id string) (User, bool, error) {
	users, err := c.getUsers(ctx, url.Values{"id": {id}})
	if err != nil || len(users) == 0 {
		return User{}, false, err
	}
	return users[0], true, nil
}

func (c *Client) getUsers(ctx context.Context, q url.Values) ([]User, error) {
	var out struct {
		Data []struct {
			ID          string `json:"id"`
			Login       string `json:"login"`
			DisplayName string `json:"display_name"`
			AvatarURL   string `json:"profile_image_url"`
		} `json:"data"`
	}
	if err := c.getJSON(ctx, c.helixBase+"/users?"+q.Encode(), &out); err != nil {
		return nil, err
	}
	users := make([]User, len(out.Data))
	for i, d := range out.Data {
		users[i] = User{ID: d.ID, Login: d.Login, Display: d.DisplayName, AvatarURL: d.AvatarURL}
	}
	return users, nil
}

// GetChannelInfo returns broadcasterID's channel metadata — the game/category
// last set on their channel is what a shoutout means by "last streaming". Needs
// no special scope; empty fields when the channel has never been configured.
func (c *Client) GetChannelInfo(ctx context.Context, broadcasterID string) (game, title, login, display string, err error) {
	q := url.Values{}
	q.Set("broadcaster_id", broadcasterID)
	var out struct {
		Data []struct {
			GameName         string `json:"game_name"`
			Title            string `json:"title"`
			BroadcasterLogin string `json:"broadcaster_login"`
			BroadcasterName  string `json:"broadcaster_name"`
		} `json:"data"`
	}
	if err := c.getJSON(ctx, c.helixBase+"/channels?"+q.Encode(), &out); err != nil {
		return "", "", "", "", err
	}
	if len(out.Data) == 0 {
		return "", "", "", "", nil
	}
	d := out.Data[0]
	return d.GameName, d.Title, d.BroadcasterLogin, d.BroadcasterName, nil
}

// getJSON performs an authenticated GET and decodes the body into v.
func (c *Client) getJSON(ctx context.Context, url string, v any) error {
	resp, err := c.do(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("GET %s: %s: %s", url, resp.Status, bytes.TrimSpace(b))
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

// GetChatters returns everyone currently in broadcasterID's chat. moderatorID
// must be a moderator of the channel (the broadcaster qualifies for their own
// channel). It follows pagination to completion.
func (c *Client) GetChatters(ctx context.Context, broadcasterID, moderatorID string) ([]Chatter, error) {
	var out []Chatter
	cursor := ""
	for {
		q := url.Values{}
		q.Set("broadcaster_id", broadcasterID)
		q.Set("moderator_id", moderatorID)
		q.Set("first", "1000")
		if cursor != "" {
			q.Set("after", cursor)
		}
		var page struct {
			Data []struct {
				UserID    string `json:"user_id"`
				UserLogin string `json:"user_login"`
				UserName  string `json:"user_name"`
			} `json:"data"`
			Pagination struct {
				Cursor string `json:"cursor"`
			} `json:"pagination"`
		}
		if err := c.getJSON(ctx, c.helixBase+"/chat/chatters?"+q.Encode(), &page); err != nil {
			return nil, err
		}
		for _, d := range page.Data {
			out = append(out, Chatter{UserID: d.UserID, Login: d.UserLogin, Display: d.UserName})
		}
		if cursor = page.Pagination.Cursor; cursor == "" {
			break
		}
	}
	return out, nil
}

// StreamInfo reports whether broadcasterID is live and, if so, when the current
// stream started (for !uptime). startedAt is the zero time when offline.
func (c *Client) StreamInfo(ctx context.Context, broadcasterID string) (live bool, startedAt time.Time, err error) {
	q := url.Values{}
	q.Set("user_id", broadcasterID)
	var out struct {
		Data []struct {
			Type      string    `json:"type"`
			StartedAt time.Time `json:"started_at"`
		} `json:"data"`
	}
	if err := c.getJSON(ctx, c.helixBase+"/streams?"+q.Encode(), &out); err != nil {
		return false, time.Time{}, err
	}
	if len(out.Data) == 0 || out.Data[0].Type != "live" {
		return false, time.Time{}, nil
	}
	return true, out.Data[0].StartedAt, nil
}

// IsLive reports whether broadcasterID is currently streaming.
func (c *Client) IsLive(ctx context.Context, broadcasterID string) (bool, error) {
	live, _, err := c.StreamInfo(ctx, broadcasterID)
	return live, err
}

// AdSchedule returns when broadcasterID's next scheduled ad begins (zero time
// when none is scheduled). Needs the token scope channel:read:ads and a
// broadcaster token. Used to warn viewers ~1 min ahead.
func (c *Client) AdSchedule(ctx context.Context, broadcasterID string) (nextAd time.Time, err error) {
	q := url.Values{}
	q.Set("broadcaster_id", broadcasterID)
	var out struct {
		Data []struct {
			NextAdAt json.RawMessage `json:"next_ad_at"`
		} `json:"data"`
	}
	if err := c.getJSON(ctx, c.helixBase+"/channels/ads?"+q.Encode(), &out); err != nil {
		return time.Time{}, err
	}
	if len(out.Data) == 0 {
		return time.Time{}, nil
	}
	return parseAdTime(out.Data[0].NextAdAt), nil
}

// parseAdTime decodes Twitch's next_ad_at, which is an RFC3339 string in current
// Helix but historically an integer unix timestamp; 0/empty/null ⇒ zero time
// (no upcoming ad).
func parseAdTime(raw json.RawMessage) time.Time {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" || s == "0" || s == `""` {
		return time.Time{}
	}
	if s[0] == '"' {
		var str string
		if err := json.Unmarshal(raw, &str); err != nil || str == "" {
			return time.Time{}
		}
		t, err := time.Parse(time.RFC3339, str)
		if err != nil {
			return time.Time{}
		}
		return t
	}
	var n int64
	if err := json.Unmarshal(raw, &n); err == nil && n > 0 {
		return time.Unix(n, 0)
	}
	return time.Time{}
}

// Followage returns when userID started following broadcasterID (for
// !followage). ok is false when they aren't following. Needs the token scope
// moderator:read:followers.
func (c *Client) Followage(ctx context.Context, broadcasterID, userID string) (followedAt time.Time, ok bool, err error) {
	q := url.Values{}
	q.Set("broadcaster_id", broadcasterID)
	q.Set("user_id", userID)
	var out struct {
		Data []struct {
			FollowedAt time.Time `json:"followed_at"`
		} `json:"data"`
	}
	if err := c.getJSON(ctx, c.helixBase+"/channels/followers?"+q.Encode(), &out); err != nil {
		return time.Time{}, false, err
	}
	if len(out.Data) == 0 {
		return time.Time{}, false, nil
	}
	return out.Data[0].FollowedAt, true, nil
}

// EnsureReward returns the id of broadcasterID's custom reward titled title,
// creating it (cost channel points, prompt text) if it doesn't exist yet. Only
// rewards this client created are manageable, so a match is by title among those.
// Returns a non-nil error if the channel can't have channel points (not an
// affiliate/partner) — the caller should log it and disable conversion.
func (c *Client) EnsureReward(ctx context.Context, broadcasterID, title string, cost int, prompt string) (string, error) {
	q := url.Values{}
	q.Set("broadcaster_id", broadcasterID)
	q.Set("only_manageable_rewards", "true")
	var list struct {
		Data []struct {
			ID    string `json:"id"`
			Title string `json:"title"`
		} `json:"data"`
	}
	if err := c.getJSON(ctx, c.helixBase+"/channel_points/custom_rewards?"+q.Encode(), &list); err != nil {
		return "", err
	}
	for _, r := range list.Data {
		if r.Title == title {
			return r.ID, nil
		}
	}

	// Not found — create it.
	body, _ := json.Marshal(map[string]any{
		"title":      title,
		"cost":       cost,
		"prompt":     prompt,
		"is_enabled": true,
	})
	resp, err := c.do(ctx, http.MethodPost, c.helixBase+"/channel_points/custom_rewards?"+
		url.Values{"broadcaster_id": {broadcasterID}}.Encode(), body)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return "", fmt.Errorf("create reward: %s: %s", resp.Status, bytes.TrimSpace(b))
	}
	var created struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		return "", err
	}
	if len(created.Data) == 0 {
		return "", fmt.Errorf("create reward: empty response")
	}
	return created.Data[0].ID, nil
}

// GetRedemptions returns redemptions of rewardID in the given status
// (e.g. "UNFULFILLED"), following pagination.
func (c *Client) GetRedemptions(ctx context.Context, broadcasterID, rewardID, status string) ([]Redemption, error) {
	var out []Redemption
	cursor := ""
	for {
		q := url.Values{}
		q.Set("broadcaster_id", broadcasterID)
		q.Set("reward_id", rewardID)
		q.Set("status", status)
		q.Set("first", "50")
		if cursor != "" {
			q.Set("after", cursor)
		}
		var page struct {
			Data []struct {
				ID        string `json:"id"`
				UserID    string `json:"user_id"`
				UserLogin string `json:"user_login"`
				UserName  string `json:"user_name"`
			} `json:"data"`
			Pagination struct {
				Cursor string `json:"cursor"`
			} `json:"pagination"`
		}
		if err := c.getJSON(ctx, c.helixBase+"/channel_points/custom_rewards/redemptions?"+q.Encode(), &page); err != nil {
			return nil, err
		}
		for _, d := range page.Data {
			out = append(out, Redemption{ID: d.ID, UserID: d.UserID, Login: d.UserLogin, Display: d.UserName})
		}
		if cursor = page.Pagination.Cursor; cursor == "" {
			break
		}
	}
	return out, nil
}

// FulfillRedemptions marks the given redemption ids of rewardID as FULFILLED
// (Twitch accepts up to 50 ids per call).
func (c *Client) FulfillRedemptions(ctx context.Context, broadcasterID, rewardID string, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	q := url.Values{}
	q.Set("broadcaster_id", broadcasterID)
	q.Set("reward_id", rewardID)
	for _, id := range ids {
		q.Add("id", id)
	}
	body, _ := json.Marshal(map[string]string{"status": "FULFILLED"})
	resp, err := c.do(ctx, http.MethodPatch, c.helixBase+"/channel_points/custom_rewards/redemptions?"+q.Encode(), body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("fulfill redemptions: %s: %s", resp.Status, bytes.TrimSpace(b))
	}
	return nil
}
