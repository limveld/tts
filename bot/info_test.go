package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

// fakeInfo stands in for the Twitch info lookups.
type fakeInfo struct {
	live      bool
	startedAt time.Time
	followed  map[string]time.Time // userID -> followed_at
	game      string               // ShoutoutInfo game
	avatar    string               // ShoutoutInfo avatar url
	nextAd    time.Time            // AdSchedule
}

func (f fakeInfo) StreamInfo(context.Context, string) (bool, time.Time, error) {
	return f.live, f.startedAt, nil
}
func (f fakeInfo) Followage(_ context.Context, _, userID string) (time.Time, bool, error) {
	at, ok := f.followed[userID]
	return at, ok, nil
}
func (f fakeInfo) ShoutoutInfo(context.Context, string) (string, string, error) {
	return f.game, f.avatar, nil
}
func (f fakeInfo) AdSchedule(context.Context, string) (time.Time, error) {
	return f.nextAd, nil
}

// infoRouter is a router with only the Twitch info seam wired (no economy).
func infoRouter(t *testing.T, info TwitchInfo) (*Router, *fakeChat) {
	t.Helper()
	r := newTestRouter(&fakeTTS{})
	chat := &fakeChat{}
	r.chat = chat
	r.info = info
	return r, chat
}

func TestUptimeLiveAndOffline(t *testing.T) {
	r, chat := infoRouter(t, fakeInfo{live: true, startedAt: time.Now().Add(-(2*time.Hour + 13*time.Minute))})
	r.Handle(emsg("bob", "!uptime", false))
	if !strings.Contains(lastReply(chat), "Live for 2h13m") {
		t.Fatalf("reply=%q want 'Live for 2h13m'", lastReply(chat))
	}

	r2, chat2 := infoRouter(t, fakeInfo{live: false})
	r2.Handle(emsg("bob", "!uptime", false))
	if !strings.Contains(lastReply(chat2), "Not live") {
		t.Fatalf("reply=%q want 'Not live'", lastReply(chat2))
	}
}

func TestFollowageSelf(t *testing.T) {
	info := fakeInfo{followed: map[string]time.Time{"id-bob": time.Now().Add(-90 * 24 * time.Hour)}}
	r, chat := infoRouter(t, info)
	r.Handle(emsg("bob", "!followage", false))
	if !strings.Contains(lastReply(chat), "you've followed for 3 months") {
		t.Fatalf("reply=%q want '3 months'", lastReply(chat))
	}
}

func TestFollowageNotFollowing(t *testing.T) {
	r, chat := infoRouter(t, fakeInfo{followed: map[string]time.Time{}})
	r.Handle(emsg("bob", "!followage", false))
	if !strings.Contains(lastReply(chat), "aren't following") {
		t.Fatalf("reply=%q want not-following", lastReply(chat))
	}
}

func TestInfoInertWithoutClient(t *testing.T) {
	r := newTestRouter(&fakeTTS{})
	chat := &fakeChat{}
	r.chat = chat
	// r.info is nil → these are not built-ins and do nothing.
	r.Handle(emsg("bob", "!uptime", false))
	r.Handle(emsg("bob", "!followage", false))
	if len(chat.replies) != 0 {
		t.Fatalf("expected no replies when info is nil; got %v", chat.replies)
	}
}

func TestShortDuration(t *testing.T) {
	for _, tc := range []struct {
		d    time.Duration
		want string
	}{
		{45 * time.Minute, "45m"},
		{2*time.Hour + 13*time.Minute, "2h13m"},
		{26 * time.Hour, "1d2h"},
	} {
		if got := shortDuration(tc.d); got != tc.want {
			t.Errorf("shortDuration(%v)=%q want %q", tc.d, got, tc.want)
		}
	}
}

func TestHumanAge(t *testing.T) {
	day := 24 * time.Hour
	for _, tc := range []struct {
		d    time.Duration
		want string
	}{
		{30 * time.Minute, "less than an hour"},
		{5 * time.Hour, "5 hours"},
		{1 * day, "1 day"},
		{90 * day, "3 months"},
		{400 * day, "1 year"},
	} {
		if got := humanAge(tc.d); got != tc.want {
			t.Errorf("humanAge(%v)=%q want %q", tc.d, got, tc.want)
		}
	}
}
