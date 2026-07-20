package main

import (
	"context"
	"log"
	"time"
)

// Events is the notification poll loop. It tracks the stream's live state (to
// reset the shoutout session on going live) and, when the token allows, polls the
// ad schedule to warn viewers ~1 min before an ad break. It mirrors Economy.Run:
// one goroutine, ticking every poll interval.
type Events struct {
	r      *Router
	roomID func() string
	poll   time.Duration
	lead   time.Duration
	adMsg  string
	ads    bool // token carries channel:read:ads (ad polling enabled)
	logger *log.Logger

	wasLive    bool
	lastWarned time.Time // the next_ad_at we've already warned for (dedup)
}

func NewEvents(r *Router, roomID func() string, cfg NotificationsConfig, ads bool, logger *log.Logger) *Events {
	return &Events{
		r:      r,
		roomID: roomID,
		poll:   cfg.AdPoll,
		lead:   cfg.AdLead,
		adMsg:  cfg.AdMessage,
		ads:    ads,
		logger: logger,
	}
}

// Run polls until ctx is canceled. It ticks once immediately so live state (and
// thus auto-shoutout eligibility) is set promptly rather than after one interval.
func (e *Events) Run(ctx context.Context) {
	e.tick(ctx)
	ticker := time.NewTicker(e.poll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			e.tick(ctx)
		}
	}
}

func (e *Events) tick(ctx context.Context) {
	room := e.roomID()
	if room == "" {
		return // no broadcaster id known yet
	}
	callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	live, _, err := e.r.info.StreamInfo(callCtx, room)
	if err != nil {
		e.logger.Printf("events: stream info: %v", err)
		return
	}
	if live && !e.wasLive {
		e.r.resetShoutSession() // new session — allow-listed folks can be shouted again
	}
	e.r.sessionLive.Store(live)
	e.wasLive = live

	if !live || !e.ads {
		return
	}
	next, err := e.r.info.AdSchedule(callCtx, room)
	if err != nil {
		e.logger.Printf("events: ad schedule: %v", err)
		return
	}
	if adDue(next, time.Now(), e.lead, e.lastWarned) {
		e.lastWarned = next
		e.r.notify(room, e.adMsg, "ad", e.adMsg, "", "")
		e.logger.Printf("events: ad reminder (next ad %s)", next.Format(time.Kitchen))
	}
}

// adDue reports whether an ad reminder should fire now: there's an upcoming ad
// within lead of now, and it isn't the one we already warned for.
func adDue(next, now time.Time, lead time.Duration, lastWarned time.Time) bool {
	if next.IsZero() {
		return false
	}
	d := next.Sub(now)
	return d > 0 && d <= lead && !next.Equal(lastWarned)
}
