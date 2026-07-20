package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"tts/twitch"
)

// Informational built-ins that read live Twitch data: !uptime (how long the
// stream has been live) and !followage (how long someone has followed). Both
// migrated from StreamElements' defaults. They aren't economy-gated — only a
// Twitch client is required — and share the per-user cooldown (mods exempt).

// TwitchInfo is the read-only Helix slice these commands need (an interface so
// tests can fake it). *twitch.Client satisfies it via twitchInfo.
type TwitchInfo interface {
	StreamInfo(ctx context.Context, broadcasterID string) (live bool, startedAt time.Time, err error)
	Followage(ctx context.Context, broadcasterID, userID string) (followedAt time.Time, ok bool, err error)
	// ShoutoutInfo returns a user's last-streamed game and profile-picture URL for
	// a shoutout (best-effort: either may be empty).
	ShoutoutInfo(ctx context.Context, userID string) (game, avatar string, err error)
	// AdSchedule returns when the next scheduled ad begins (zero time when none).
	AdSchedule(ctx context.Context, broadcasterID string) (nextAd time.Time, err error)
}

// twitchInfo adapts *twitch.Client to TwitchInfo.
type twitchInfo struct{ client *twitch.Client }

func (t twitchInfo) StreamInfo(ctx context.Context, b string) (bool, time.Time, error) {
	return t.client.StreamInfo(ctx, b)
}
func (t twitchInfo) Followage(ctx context.Context, b, u string) (time.Time, bool, error) {
	return t.client.Followage(ctx, b, u)
}

// ShoutoutInfo fetches the game (Get Channel Information) and avatar (Get Users)
// in one call. Best-effort: a failure of either leaves that field empty; err is
// the first error seen so the caller can log it but still shout.
func (t twitchInfo) ShoutoutInfo(ctx context.Context, userID string) (game, avatar string, err error) {
	g, _, _, _, gerr := t.client.GetChannelInfo(ctx, userID)
	u, _, uerr := t.client.GetUserByID(ctx, userID)
	if gerr != nil {
		err = gerr
	} else if uerr != nil {
		err = uerr
	}
	return g, u.AvatarURL, err
}

func (t twitchInfo) AdSchedule(ctx context.Context, b string) (time.Time, error) {
	return t.client.AdSchedule(ctx, b)
}

// uptime replies with how long the stream has been live.
func (r *Router) uptime(m ChatMessage) {
	if !(m.IsMod || m.IsBroadcaster) && !r.cooldown.Allow(m.User) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	live, startedAt, err := r.info.StreamInfo(ctx, m.RoomID)
	if err != nil {
		r.logger.Printf("uptime: %v", err)
		return
	}
	if !live {
		r.reply(m, "Not live right now.")
		return
	}
	r.reply(m, "Live for "+shortDuration(time.Since(startedAt))+".")
}

// followage replies with how long the caller (or "@user") has followed.
func (r *Router) followage(rest string, m ChatMessage) {
	if !(m.IsMod || m.IsBroadcaster) && !r.cooldown.Allow(m.User) {
		return
	}
	login := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(rest), "@"))
	targetID, name := m.UserID, "you"
	if login != "" && login != strings.ToLower(m.User) {
		id, _, ok := r.resolveTarget(login)
		if !ok {
			r.reply(m, "no such user @"+login+".")
			return
		}
		targetID, name = id, "@"+login
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	followedAt, ok, err := r.info.Followage(ctx, m.RoomID, targetID)
	if err != nil {
		r.logger.Printf("followage: %v", err)
		return
	}
	if !ok {
		if name == "you" {
			r.reply(m, "@"+displayName(m)+" you aren't following.")
		} else {
			r.reply(m, name+" isn't following.")
		}
		return
	}
	age := humanAge(time.Since(followedAt))
	if name == "you" {
		r.reply(m, fmt.Sprintf("@%s you've followed for %s.", displayName(m), age))
	} else {
		r.reply(m, fmt.Sprintf("%s has followed for %s.", name, age))
	}
}

// shortDuration formats an elapsed stream time like "2h13m", "45m", or "1d3h".
func shortDuration(d time.Duration) string {
	d = d.Round(time.Minute)
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	switch {
	case h >= 24:
		return fmt.Sprintf("%dd%dh", h/24, h%24)
	case h > 0:
		return fmt.Sprintf("%dh%dm", h, m)
	default:
		return fmt.Sprintf("%dm", m)
	}
}

// humanAge formats a follow age coarsely: "3 months", "2 years", "5 days".
func humanAge(d time.Duration) string {
	days := int(d.Hours() / 24)
	switch {
	case days >= 365:
		return plural(days/365, "year")
	case days >= 30:
		return plural(days/30, "month")
	case days >= 1:
		return plural(days, "day")
	default:
		if h := int(d.Hours()); h >= 1 {
			return plural(h, "hour")
		}
		return "less than an hour"
	}
}

func plural(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return fmt.Sprintf("%d %ss", n, unit)
}
